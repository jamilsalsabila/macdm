// Package manager is the job lifecycle layer between the HTTP API and the engine.
// It owns the store, starts/stops per-job goroutines, and maps engine progress
// back onto persisted job state.
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"macdm/internal/engine"
	"macdm/internal/sniff"
	"macdm/internal/store"
	"macdm/internal/tools"

	"crypto/rand"
	"encoding/hex"
)

// Config for a Manager.
type Config struct {
	DownloadDir string
	WorkDir     string // scratch space for stream segments; "" => DownloadDir/.macdm-work
	Engine      engine.Config
	MaxActive   int // concurrent downloads; 0 => 4
	Tools       tools.Set
	CookiesFrom string // browser for yt-dlp --cookies-from-browser; "" => none

	// AutoAccept skips the "New Download" dialog and starts caught downloads
	// with defaults. PromptTimeoutSec is how long a proposal waits for a UI
	// answer before auto-accepting anyway (0 => 8).
	AutoAccept       bool
	PromptTimeoutSec int
}

// Manager coordinates downloads.
type Manager struct {
	cfg   Config
	st    *store.Store
	eng   *engine.Engine
	slots chan struct{}
	hub   *proposalHub

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New wires a Manager and resumes anything left mid-flight by a previous run.
func New(cfg Config, st *store.Store) *Manager {
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 4
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.DownloadDir, ".macdm-work")
	}
	m := &Manager{
		cfg:     cfg,
		st:      st,
		eng:     engine.New(cfg.Engine),
		slots:   make(chan struct{}, cfg.MaxActive),
		running: map[string]context.CancelFunc{},
		hub:     newProposalHub(),
	}
	return m
}

// Store exposes the underlying store for the API layer (list/get/watch).
func (m *Manager) Store() *store.Store { return m.st }

// Tools exposes the resolved external-tool paths for the API layer.
func (m *Manager) Tools() tools.Set { return m.cfg.Tools }

// AddOptions carries optional per-job settings.
type AddOptions struct {
	Headers   map[string]string
	Filename  string // override; otherwise derived from the URL / response
	Conns     int
	Dest      string // full path override
	FormatID  string // quality selector (yt-dlp -f expr | HLS variant URL | "hNNN")
	Quality   string // human label for display
	Formats   []store.FormatChoice
	Fragments []store.Fragment // byte-range slices to assemble (Instagram/FB)
}

// Add creates a job for rawurl and starts it. Returns the created job.
func (m *Manager) Add(rawurl string, opt AddOptions) (*store.Job, error) {
	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid URL %q", rawurl)
	}

	kind := sniff.ClassifyURL(u)
	name := opt.Filename
	if name == "" {
		name = guessName(u)
	}
	dest := opt.Dest
	switch {
	case dest == "":
		dest = filepath.Join(m.cfg.DownloadDir, name)
	case isDir(dest):
		dest = filepath.Join(dest, name)
	}

	// De-dupe: don't start a second job for the same URL+destination that is
	// already running/queued (rapid double-clicks, sniffer re-fires).
	for _, ex := range m.st.List() {
		if ex.URL == rawurl && ex.Dest == dest && !ex.Terminal() {
			return ex, nil
		}
	}

	j := &store.Job{
		ID:          newID(),
		Kind:        kind,
		URL:         rawurl,
		Dest:        dest,
		Filename:    filepath.Base(dest),
		Headers:     opt.Headers,
		Status:      store.StatusQueued,
		Connections: opt.Conns,
		FormatID:    opt.FormatID,
		Quality:     opt.Quality,
		Formats:     opt.Formats,
		Fragments:   opt.Fragments,
		CreatedAt:   time.Now(),
	}
	if err := m.st.Put(j); err != nil {
		return nil, err
	}

	m.start(j.ID)
	return m.st.Get(j.ID)
}

// Resume restarts a paused or errored job.
func (m *Manager) Resume(id string) error {
	j, err := m.st.Get(id)
	if err != nil {
		return err
	}
	if j.Status == store.StatusDownloading {
		return nil
	}
	if j.Status == store.StatusCompleted {
		return errors.New("already completed")
	}
	m.start(id)
	return nil
}

// SetConns changes a job's connection count. If it's an active HTTP download the
// job is bounced (pause → resume) so the engine re-splits the remaining bytes
// into the new number of connections; completed bytes are kept.
func (m *Manager) SetConns(id string, n int) error {
	if n < 1 || n > 64 {
		return errors.New("connections must be 1–64")
	}
	j, err := m.st.Get(id)
	if err != nil {
		return err
	}
	if _, err := m.st.Update(id, func(jj *store.Job) { jj.Connections = n }); err != nil {
		return err
	}
	// A live re-split only helps a resumable multi-connection HTTP download.
	// Bouncing a non-resumable job would restart it from zero, so don't.
	if j.Kind == store.KindHTTP && j.Resumable {
		m.mu.Lock()
		_, running := m.running[id]
		m.mu.Unlock()
		if running {
			_ = m.Pause(id)
			// wait for the goroutine to release, then restart
			go func() {
				for i := 0; i < 100; i++ {
					m.mu.Lock()
					_, busy := m.running[id]
					m.mu.Unlock()
					if !busy {
						break
					}
					time.Sleep(30 * time.Millisecond)
				}
				m.start(id)
			}()
		}
	}
	return nil
}

// Pause cancels a running job's context; its sidecar is flushed by the engine.
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	cancel, ok := m.running[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("job is not running")
	}
	cancel()
	return nil
}

// Remove pauses (if running) and deletes a job. Files on disk are left in place.
func (m *Manager) Remove(id string) error {
	_ = m.Pause(id)
	return m.st.Delete(id)
}

func (m *Manager) start(id string) {
	m.mu.Lock()
	if _, busy := m.running[id]; busy {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running[id] = cancel
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.running, id)
			m.mu.Unlock()
		}()

		// Wait for a concurrency slot, but stay cancellable while queued.
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
		case <-ctx.Done():
			m.setStatus(id, store.StatusPaused, "")
			return
		}

		if ctx.Err() != nil {
			m.setStatus(id, store.StatusPaused, "")
			return
		}

		j, err := m.st.Get(id)
		if err != nil {
			return
		}
		if j.Status == store.StatusCompleted {
			return // e.g. a SetConns bounce that raced with completion
		}
		m.setStatus(id, store.StatusProbing, "")

		switch {
		case len(j.Fragments) > 0:
			err = m.execFragments(ctx, id, j)
		case j.Kind == store.KindHLS, j.Kind == store.KindDASH:
			err = m.execStream(ctx, id, j)
		case j.Kind == store.KindExtract:
			err = m.execExtract(ctx, id, j)
		default:
			err = m.execHTTP(ctx, id, j)
		}

		switch {
		case err == nil:
			_, _ = m.st.Update(id, func(jj *store.Job) {
				jj.Status = store.StatusCompleted
				jj.SpeedBps = 0
				if jj.TotalBytes > 0 {
					jj.DoneBytes = jj.TotalBytes
				}
			})
		case errors.Is(err, context.Canceled):
			m.setStatus(id, store.StatusPaused, "")
		case errors.Is(err, errDRM):
			m.setStatus(id, store.StatusDRM, err.Error())
		default:
			m.fail(id, err.Error())
		}
	}()
}

func (m *Manager) execHTTP(ctx context.Context, id string, j *store.Job) error {
	dest := j.Dest

	// Probe first: learn the real filename, and — crucially — bail out of the
	// raw engine if the URL is actually a web page (text/html). Many "video"
	// links a user clicks are embed pages; hand those to the extractor instead
	// of saving the HTML as a file.
	if pr, e := m.eng.ProbeURL(ctx, j.URL, j.Headers); e == nil {
		ct := strings.ToLower(pr.ContentType)
		isHTML := strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml")
		// An HTML response the server did not mark as a download is a web page,
		// not a file — hand it to the extractor (yt-dlp handles embed / watch
		// pages on ~1800 sites).
		if isHTML && !pr.NamedByServer {
			_, _ = m.st.Update(id, func(jj *store.Job) { jj.Kind = store.KindExtract })
			jj, _ := m.st.Get(id)
			return m.execExtract(ctx, id, jj)
		}
		if looksGeneric(filepath.Base(dest)) && pr.NamedByServer && pr.Filename != "" {
			dest = filepath.Join(filepath.Dir(dest), sanitize(pr.Filename))
			_, _ = m.st.Update(id, func(jj *store.Job) {
				jj.Dest = dest
				jj.Filename = filepath.Base(dest)
			})
		}
	}

	if j.Status == store.StatusQueued {
		if u := m.uniqueDest(id, dest); u != dest {
			dest = u
			_, _ = m.st.Update(id, func(jj *store.Job) { jj.Dest = dest; jj.Filename = filepath.Base(dest) })
		}
	}

	var lastPersist time.Time
	spec := engine.DownloadSpec{
		URL:     j.URL,
		Dest:    dest,
		Headers: j.Headers,
		Conns:   j.Connections,
	}
	probe, err := m.eng.Run(ctx, spec, func(p engine.Progress) {
		now := time.Now()
		if now.Sub(lastPersist) < 450*time.Millisecond && p.DoneBytes < p.TotalBytes {
			return
		}
		lastPersist = now
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			jj.DoneBytes = p.DoneBytes
			if p.TotalBytes > 0 {
				jj.TotalBytes = p.TotalBytes
			}
			jj.SpeedBps = p.SpeedBps
			jj.Resumable = p.Resumable
			if p.NumConns > 0 {
				jj.Connections = p.NumConns
			}
			jj.Conns = engineConns(p.Conns)
		})
	})
	if probe != nil {
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Resumable = probe.AcceptRanges
			if jj.TotalBytes == 0 && probe.TotalBytes > 0 {
				jj.TotalBytes = probe.TotalBytes
			}
		})
	}
	return err
}

// execFragments assembles a byte-range-fragmented file (Instagram/Facebook).
func (m *Manager) execFragments(ctx context.Context, id string, j *store.Job) error {
	dest := j.Dest
	if looksGeneric(filepath.Base(dest)) {
		name := "video-" + time.Now().Format("20060102-150405")
		if t := pageTitle(ctx, headerVal(j.Headers, "Referer")); t != "" {
			name = sanitize(t)
		}
		dest = filepath.Join(filepath.Dir(dest), name+".mp4")
	}
	// Instagram/Facebook slices are always progressive MP4; make sure the name
	// carries the extension even when it came from a page title.
	if !strings.EqualFold(filepath.Ext(dest), ".mp4") {
		dest += ".mp4"
	}
	// Only on a fresh job — a resume must keep the name it was already writing.
	if j.Status == store.StatusQueued {
		dest = m.uniqueDest(id, dest)
	}
	if dest != j.Dest {
		_, _ = m.st.Update(id, func(jj *store.Job) { jj.Dest = dest; jj.Filename = filepath.Base(dest) })
	}

	frags := make([]engine.Fragment, len(j.Fragments))
	for i, f := range j.Fragments {
		frags[i] = engine.Fragment{URL: f.URL, Start: f.Start, End: f.End}
	}

	var lastPersist time.Time
	err := m.eng.RunFragments(ctx, dest, j.Headers, frags, j.Connections, func(p engine.Progress) {
		now := time.Now()
		if now.Sub(lastPersist) < 400*time.Millisecond && p.DoneBytes < p.TotalBytes {
			return
		}
		lastPersist = now
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			jj.DoneBytes = p.DoneBytes
			jj.TotalBytes = p.TotalBytes
			jj.SpeedBps = p.SpeedBps
			jj.Resumable = true
			jj.Connections = p.NumConns
			jj.Conns = engineConns(p.Conns)
		})
	})
	if err != nil {
		return err
	}
	return finalize(m, id, dest)
}

func headerVal(h map[string]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// engineConns converts engine per-connection progress to the store shape and
// fills the human-readable Info column shown in the detail table.
func engineConns(cs []engine.ConnProgress) []store.ConnStat {
	if len(cs) == 0 {
		return nil
	}
	out := make([]store.ConnStat, len(cs))
	for i, c := range cs {
		info := c.Status
		switch c.Status {
		case "receiving":
			info = "Receiving data..."
		case "connecting":
			info = "Connecting..."
		case "done":
			info = "Completed"
		case "error":
			info = "Error — retrying"
		case "idle":
			info = "Waiting"
		}
		out[i] = store.ConnStat{
			Index: c.Index, Start: c.Start, Downloaded: c.Downloaded,
			Total: c.Total, Status: c.Status, Info: info,
		}
	}
	return out
}

func (m *Manager) setStatus(id, status, msg string) {
	_, _ = m.st.Update(id, func(j *store.Job) {
		j.Status = status
		j.Error = msg
		if status != store.StatusDownloading {
			j.SpeedBps = 0
		}
	})
}

func (m *Manager) fail(id, msg string) {
	_, _ = m.st.Update(id, func(j *store.Job) {
		j.Status = store.StatusError
		j.Error = msg
		j.SpeedBps = 0
	})
}

// Shutdown cancels every running job (which pauses them and kills any
// subprocess) and waits briefly for them to unwind. Call it before the process
// exits so yt-dlp/ffmpeg children don't leak.
func (m *Manager) Shutdown(wait time.Duration) {
	m.mu.Lock()
	for _, cancel := range m.running {
		cancel()
	}
	m.mu.Unlock()

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := len(m.running)
		m.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ResumeAll restarts every job the previous run left paused.
func (m *Manager) ResumeAll() {
	for _, j := range m.st.List() {
		if j.Status == store.StatusPaused {
			m.start(j.ID)
		}
	}
}

func guessName(u *url.URL) string {
	base := filepath.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		return "download"
	}
	return base
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
