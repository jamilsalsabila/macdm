// Package store holds the job model and a simple JSON-file-backed persistence layer.
//
// A single macdmd process owns the store; concurrent goroutines coordinate through
// the store's mutex. Persistence is a whole-file atomic rewrite on every change —
// fine for the handful-of-jobs scale a personal download manager runs at.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status values for a Job.
const (
	StatusQueued      = "queued"
	StatusProbing     = "probing"
	StatusDownloading = "downloading"
	StatusPaused      = "paused"
	StatusCompleted   = "completed"
	StatusError       = "error"
	// StatusDRM marks a stream we can see but deliberately will not download
	// (Widevine / PlayReady / unknown SAMPLE-AES). Not a failure to retry.
	StatusDRM = "drm_protected"
)

// Kind values describe how a job is fetched.
const (
	KindHTTP    = "http"    // a single addressable file (possibly multi-connection)
	KindHLS     = "hls"     // an .m3u8 manifest to assemble
	KindDASH    = "dash"    // an .mpd manifest to assemble
	KindExtract = "extract" // a page URL to resolve via the extractor (yt-dlp)
)

// Job is one download as tracked by the daemon.
type Job struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	URL      string            `json:"url"`
	Dest     string            `json:"dest"`     // absolute path to the final file
	Filename string            `json:"filename"` // basename of Dest
	Headers  map[string]string `json:"headers,omitempty"`

	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	TotalBytes  int64 `json:"total_bytes"`
	DoneBytes   int64 `json:"done_bytes"`
	Connections int   `json:"connections"`
	Resumable   bool  `json:"resumable"`

	// Segments / SegmentsDone: for HLS/DASH the byte totals above are estimates
	// (so the % bar moves smoothly); these carry the real segment counts for the
	// "N segments" text.
	Segments     int `json:"segments,omitempty"`
	SegmentsDone int `json:"segments_done,omitempty"`

	// Conns is the per-connection breakdown for the IDM-style detail table.
	// Runtime state, persisted opportunistically like SpeedBps.
	Conns []ConnStat `json:"conns,omitempty"`

	// Video/quality selection (extractor & adaptive streams).
	Quality  string `json:"quality,omitempty"`
	FormatID string `json:"format_id,omitempty"`
	// Per-job overrides for the extractor path; empty falls back to Settings.
	AudioLang     string         `json:"audio_lang,omitempty"`
	SubtitleLangs string         `json:"subtitle_langs,omitempty"`
	Formats       []FormatChoice `json:"formats,omitempty"`

	// Fragments: byte-range slices of one file, each with its own URL
	// (Instagram/Facebook). Present => assemble them instead of a plain GET.
	Fragments []Fragment `json:"fragments,omitempty"`

	// SpeedBps is a runtime-only estimate, refreshed by the engine; it is
	// persisted opportunistically but never trusted across restarts.
	SpeedBps int64 `json:"speed_bps"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConnStat is one connection's progress, mirrored from engine.ConnProgress.
type ConnStat struct {
	Index      int    `json:"index"`
	Start      int64  `json:"start"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Status     string `json:"status"`
	Info       string `json:"info,omitempty"` // free-text for the table's "Info" column
}

// Fragment is one byte-range slice of a media file (mirrors engine.Fragment).
type Fragment struct {
	URL   string `json:"url"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

// FormatChoice is a user-facing quality option (kept here to avoid an import
// cycle; internal/sniff defines the canonical builder helpers).
type FormatChoice struct {
	ID        string `json:"id"`
	Label     string `json:"label"` // "1080p60", "720p", "audio only"…
	Height    int    `json:"height,omitempty"`
	FPS       int    `json:"fps,omitempty"`
	Ext       string `json:"ext,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Kind      string `json:"kind,omitempty"` // video+audio | video | audio
}

// Percent returns completion in the range [0,100]; 0 when the size is unknown.
func (j *Job) Percent() float64 {
	if j.TotalBytes <= 0 {
		return 0
	}
	p := float64(j.DoneBytes) / float64(j.TotalBytes) * 100
	if p > 100 {
		return 100
	}
	return p
}

// Terminal reports whether the job has reached a state the engine will not leave
// on its own.
func (j *Job) Terminal() bool {
	switch j.Status {
	case StatusCompleted, StatusError, StatusDRM:
		return true
	}
	return false
}

// ErrNotFound is returned by lookups for an unknown job ID.
var ErrNotFound = errors.New("job not found")

// Store is an in-memory job map with JSON-file persistence.
type Store struct {
	mu       sync.RWMutex
	path     string
	jobs     map[string]*Job
	watchers map[int]chan Event
	nextW    int

	dirty    bool          // a progress update is waiting to be flushed
	flushReq chan struct{} // wake the flush loop for an immediate write
	stop     chan struct{}
	done     chan struct{} // closed once the flush loop has written and exited
}

// Event is broadcast to watchers whenever a job is created or changes.
type Event struct {
	Type string `json:"type"` // "job" for an upsert, "delete" for removal
	Job  *Job   `json:"job"`
}

// Open loads the store from path, creating an empty one if the file is absent.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:     path,
		jobs:     map[string]*Job{},
		watchers: map[int]chan Event{},
		flushReq: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		go s.flushLoop()
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, j := range list {
		// A job caught mid-download at shutdown is resumed, not abandoned.
		if j.Status == StatusDownloading || j.Status == StatusProbing {
			j.Status = StatusPaused
		}
		s.jobs[j.ID] = j
	}
	go s.flushLoop()
	return s, nil
}

// flushLoop persists the store at most once a second (or immediately on an
// explicit flush request) so a burst of progress updates during a download does
// not turn into a burst of whole-file rewrites blocking the download goroutine.
func (s *Store) flushLoop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			s.flush()
			return
		case <-s.flushReq:
			s.flush()
		case <-t.C:
			s.flush()
		}
	}
}

func (s *Store) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		// best-effort; the next tick retries
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}

// Close stops the flush loop and blocks until its final write has landed —
// otherwise the daemon can exit with the last progress/status update still
// only in memory. Safe to call more than once.
func (s *Store) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second): // never hang shutdown on a stuck disk
	}
}

// requestFlush asks the flush loop to write now (non-blocking).
func (s *Store) requestFlush() {
	select {
	case s.flushReq <- struct{}{}:
	default:
	}
}

// List returns all jobs, newest first.
func (s *Store) List() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out
}

// Get returns a copy of the job with the given ID.
func (s *Store) Get(id string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *j
	return &cp, nil
}

// Put inserts or replaces a job and notifies watchers.
func (s *Store) Put(j *Job) error {
	s.mu.Lock()
	j.UpdatedAt = time.Now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = j.UpdatedAt
	}
	stored := *j
	s.jobs[j.ID] = &stored
	s.dirty = true
	// broadcast a SEPARATE copy — a later Update mutates the map entry in place
	// and a watcher may still be marshalling the event.
	ev := *j
	s.broadcastLocked(Event{Type: "job", Job: &ev})
	s.mu.Unlock()
	s.requestFlush() // a new job is worth writing right away
	return nil
}

// Update applies fn to the stored job under the lock, then persists and notifies.
func (s *Store) Update(id string, fn func(*Job)) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	fn(j)
	j.UpdatedAt = time.Now()
	cp := *j
	s.dirty = true
	s.broadcastLocked(Event{Type: "job", Job: &cp})
	// Flush now for state the user must not lose on a crash; progress ticks ride
	// the 1s timer.
	if terminalStatus(cp.Status) {
		s.requestFlush()
	}
	return &cp, nil
}

func terminalStatus(s string) bool {
	switch s {
	case StatusCompleted, StatusError, StatusDRM, StatusPaused:
		return true
	}
	return false
}

// Delete removes a job.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.jobs, id)
	cp := *j
	s.dirty = true
	s.broadcastLocked(Event{Type: "delete", Job: &cp})
	s.requestFlush() // non-blocking
	return nil
}

// Watch registers a channel that receives every subsequent Event. The returned
// cancel func unregisters and closes it.
func (s *Store) Watch() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextW
	s.nextW++
	ch := make(chan Event, 64)
	s.watchers[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.watchers[id]; ok {
			delete(s.watchers, id)
			close(c)
		}
	}
}

func (s *Store) broadcastLocked(ev Event) {
	for id, ch := range s.watchers {
		select {
		case ch <- ev:
		default:
			// A watcher that cannot keep up is dropped rather than stalling
			// every writer; the SSE client will reconnect and re-sync.
			delete(s.watchers, id)
			close(ch)
		}
	}
}

func (s *Store) persistLocked() error {
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
