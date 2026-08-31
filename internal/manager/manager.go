// Package manager is the job lifecycle layer between the HTTP API and the engine.
// It owns the store, starts/stops per-job goroutines, and maps engine progress
// back onto persisted job state.
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"macdm/internal/diskspace"
	"macdm/internal/engine"
	"macdm/internal/ratelimit"
	"macdm/internal/schedule"
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
	// SpeedLimitBps caps the total transfer rate across every running job.
	// 0 means no limit. Changeable at runtime via SetSpeedLimit.
	SpeedLimitBps int64

	// Schedule confines downloading to a recurring window. The zero value is
	// disabled, and downloads run whenever they like.
	Schedule    schedule.Window
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
	// limiter is the single transfer ceiling shared by the engine and the
	// stream clients, so one figure covers everything at once.
	limiter *ratelimit.Bucket

	// schedMu guards sched only. Kept apart from mu, which start() already
	// holds while it reads the window.
	schedMu  sync.RWMutex
	sched    schedule.Window
	stopOnce sync.Once
	stopped  chan struct{}

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
	// One bucket for everything: the engine's connections and the stream
	// clients' segment fetches all draw from it, so the ceiling the user sets
	// is the total, not a per-download allowance.
	limiter := ratelimit.New(cfg.SpeedLimitBps)
	cfg.Engine.Limiter = limiter
	m := &Manager{
		cfg:     cfg,
		st:      st,
		limiter: limiter,
		eng:     engine.New(cfg.Engine),
		slots:   make(chan struct{}, cfg.MaxActive),
		running: map[string]context.CancelFunc{},
		hub:     newProposalHub(),
		sched:   cfg.Schedule,
		stopped: make(chan struct{}),
	}
	m.pruneWorkDirs()
	go m.watchSchedule(30 * time.Second)
	return m
}

// watchSchedule opens and closes the download window as the clock passes it.
// Polling rather than timing the next edge keeps it right across a laptop sleep
// or a clock change, where a timer set hours ago would fire at the wrong moment.
func (m *Manager) watchSchedule(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stopped:
			return
		case <-t.C:
			m.applySchedule()
		}
	}
}

// applySchedule stops what may not run now and starts what may.
func (m *Manager) applySchedule() {
	w := m.Schedule()
	if !w.Enabled {
		return
	}
	if w.Active(time.Now()) {
		m.releaseHeld()
		return
	}
	for _, j := range m.st.List() {
		if !holdable(j) {
			continue
		}
		// Mark the hold before cancelling: the download goroutine sets the
		// status on its way out, and must not find the flag missing.
		//
		// The status is re-checked inside the update because a job can finish
		// in the moment between being listed and being held, and dragging a
		// completed download back out of "completed" would be a lie.
		held := false
		_, _ = m.st.Update(j.ID, func(jj *store.Job) {
			if !holdable(jj) {
				return
			}
			jj.ScheduledHold = true
			jj.Error = "waiting for the download window (" + w.String() + ")"
			held = true
		})
		if held {
			_ = m.Pause(j.ID)
		}
	}
}

// holdable reports whether a job is doing work the scheduler may interrupt.
func holdable(j *store.Job) bool {
	switch j.Status {
	case store.StatusDownloading, store.StatusProbing, store.StatusQueued:
		return true
	}
	return false
}

// SetSchedule replaces the download window while the daemon runs, and applies
// it at once rather than at the next tick — someone who has just switched the
// scheduler off expects their downloads back immediately.
func (m *Manager) SetSchedule(w schedule.Window) {
	m.schedMu.Lock()
	m.sched = w
	m.schedMu.Unlock()

	if !w.Enabled || w.Active(time.Now()) {
		// Release anything the scheduler is holding, including when it has just
		// been turned off entirely.
		m.releaseHeld()
		return
	}
	m.applySchedule()
}

// releaseHeld restarts the jobs the scheduler put down — and only those. A job
// the user paused stays paused, and a job that finished while held is simply
// unmarked rather than downloaded a second time.
func (m *Manager) releaseHeld() {
	for _, j := range m.st.List() {
		if !j.ScheduledHold {
			continue
		}
		if j.Status == store.StatusCompleted {
			_, _ = m.st.Update(j.ID, func(jj *store.Job) { jj.ScheduledHold = false })
			continue
		}
		m.start(j.ID)
	}
}

// Schedule reports the window in force.
func (m *Manager) Schedule() schedule.Window {
	m.schedMu.RLock()
	defer m.schedMu.RUnlock()
	return m.sched
}

// scheduleBlocks reports whether the window is shut right now.
func (m *Manager) scheduleBlocks() (schedule.Window, bool) {
	w := m.Schedule()
	return w, w.Enabled && !w.Active(time.Now())
}

// pruneWorkDirs deletes per-job scratch dirs that no live job owns. Dirs
// belonging to an unfinished job are kept: stream segments and yt-dlp's .part
// files let a resume pick up where the previous run stopped, which matters most
// exactly when the daemon restarted mid-download.
func (m *Manager) pruneWorkDirs() {
	entries, err := os.ReadDir(m.cfg.WorkDir)
	if err != nil {
		return // no scratch root yet — nothing to prune
	}
	keep := map[string]bool{}
	for _, j := range m.st.List() {
		if j.Status != store.StatusCompleted {
			keep[j.ID] = true
		}
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			_ = os.RemoveAll(filepath.Join(m.cfg.WorkDir, e.Name()))
		}
	}
}

// SetSpeedLimit changes the total transfer ceiling in bytes per second while
// downloads are running; 0 removes it. Takes effect on the next block of bytes,
// with no restart and no interruption to jobs in flight.
func (m *Manager) SetSpeedLimit(bytesPerSec int64) {
	m.limiter.SetLimit(bytesPerSec)
}

// SpeedLimit reports the current ceiling in bytes per second; 0 means none.
func (m *Manager) SpeedLimit() int64 { return m.limiter.Limit() }

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
	AudioLang string // dubbed soundtrack language ("id"); "" => Settings default
	SubLangs  string // yt-dlp --sub-langs expression; "" => Settings default
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
	name := safeName(opt.Filename)
	if name == "" {
		name = safeName(guessName(u))
	}
	if name == "" {
		name = "download"
	}
	// opt.Dest is either empty (use the default dir) or a folder the user picked
	// in the dialog; the filename component is always the sanitised `name`, so an
	// untrusted document.title can't traverse out with "../".
	dest := opt.Dest
	switch {
	case dest == "":
		dest = filepath.Join(m.cfg.DownloadDir, name)
	case isDir(dest):
		dest = filepath.Join(dest, name)
	default:
		dest = filepath.Join(filepath.Dir(dest), safeName(filepath.Base(dest)))
	}

	// De-dupe: don't start a second job for the same URL+destination that is
	// already running/queued (rapid double-clicks, sniffer re-fires).
	for _, ex := range m.st.List() {
		if ex.URL == rawurl && ex.Dest == dest && !ex.Terminal() {
			return ex, nil
		}
	}

	j := &store.Job{
		ID:            newID(),
		Kind:          kind,
		URL:           rawurl,
		Dest:          dest,
		Filename:      filepath.Base(dest),
		Headers:       opt.Headers,
		AudioLang:     opt.AudioLang,
		SubtitleLangs: opt.SubLangs,
		Status:        store.StatusQueued,
		Connections:   opt.Conns,
		FormatID:      opt.FormatID,
		Quality:       opt.Quality,
		Formats:       opt.Formats,
		Fragments:     opt.Fragments,
		CreatedAt:     time.Now(),
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
	// A live re-split only helps a download the engine drives and can resume.
	// That is a resumable HTTP job, and now also an extract job on the direct
	// path — its streams go through the same engine, and the sidecar keys on a
	// stable identity rather than the re-signed URL, so a bounce continues
	// instead of starting over. Bouncing anything else would restart from zero.
	if (j.Kind == store.KindHTTP && j.Resumable) || j.Kind == store.KindExtract {
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
	if ok {
		cancel()
		return nil
	}
	// A job the scheduler is holding is not running, but pausing it still means
	// something: leave it alone when the window opens.
	if j, err := m.st.Get(id); err == nil && j.ScheduledHold {
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.ScheduledHold = false
			jj.Error = ""
			// A job that finished under a stray hold keeps its result.
			if jj.Status != store.StatusCompleted {
				jj.Status = store.StatusPaused
			}
		})
		return nil
	}
	return errors.New("job is not running")
}

// Remove pauses (if running) and deletes a job.
//
// A finished download is the user's file and stays where it is. The half-built
// one does not: dropping the job leaves its .part and .macdm sidecar owned by
// nothing, with no job left to explain them, and the .part reports its full
// final size because it was created sparse — so abandoning a 100 MB download
// left what looks like a 100 MB mystery file in the download folder. Only those
// two are removed, never the finished file, which is safe because completing a
// download renames the .part away and deletes the sidecar.
func (m *Manager) Remove(id string) error {
	j, getErr := m.st.Get(id)
	_ = m.Pause(id)
	_ = os.RemoveAll(m.workDir(id))
	if getErr == nil && j.Dest != "" && j.Status != store.StatusCompleted {
		// Give the download goroutine a moment to let go of the file first;
		// removing it underneath a running write would be worse than litter.
		m.mu.Lock()
		_, stillRunning := m.running[id]
		m.mu.Unlock()
		for i := 0; stillRunning && i < 40; i++ {
			time.Sleep(50 * time.Millisecond)
			m.mu.Lock()
			_, stillRunning = m.running[id]
			m.mu.Unlock()
		}
		_ = os.Remove(j.Dest + ".part")
		_ = os.Remove(j.Dest + ".macdm")
	}
	return m.st.Delete(id)
}

func (m *Manager) start(id string) {
	// Gated here rather than at each caller: Add, Accept, Resume and the
	// scheduler's own sweep all funnel through this one place.
	if w, blocked := m.scheduleBlocks(); blocked {
		_, _ = m.st.Update(id, func(j *store.Job) {
			if j.Status == store.StatusCompleted {
				return
			}
			j.ScheduledHold = true
			j.Status = store.StatusPaused
			j.SpeedBps = 0
			j.Error = "waiting for the download window (" + w.String() + ")"
		})
		return
	}

	m.mu.Lock()
	if _, busy := m.running[id]; busy {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running[id] = cancel
	m.mu.Unlock()

	// Running now, so it is no longer waiting on the clock.
	_, _ = m.st.Update(id, func(j *store.Job) {
		if j.ScheduledHold {
			j.ScheduledHold = false
			j.Error = ""
		}
	})

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.running, id)
			m.mu.Unlock()
			// If the job was deleted while running, Remove's own cleanup raced
			// this goroutine and may have missed files it went on to write.
			// Now that the work has stopped, clear the scratch dir for good.
			if _, err := m.st.Get(id); errors.Is(err, store.ErrNotFound) {
				_ = os.RemoveAll(m.workDir(id))
			}
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

		runOnce := func() error {
			jj, e := m.st.Get(id)
			if e != nil {
				return e
			}
			m.setStatus(id, store.StatusProbing, "")
			switch {
			case len(jj.Fragments) > 0:
				return m.execFragments(ctx, id, jj)
			case jj.Kind == store.KindHLS, jj.Kind == store.KindDASH:
				return m.execStream(ctx, id, jj)
			case jj.Kind == store.KindExtract:
				return m.execExtract(ctx, id, jj)
			default:
				return m.execHTTP(ctx, id, jj)
			}
		}

		// A dropped connection or a flaky host shouldn't need a manual Resume
		// click. Retry the whole job a few times — every exec path re-probes and
		// resumes from the .part / sidecar, so completed bytes are kept.
		const maxAutoResumes = 4
		for attempt := 0; ; attempt++ {
			err = runOnce()
			if err == nil || attempt >= maxAutoResumes || !transientErr(err) {
				break
			}
			wait := time.Duration(attempt+1) * autoResumeBackoff
			m.setStatus(id, store.StatusProbing,
				fmt.Sprintf("connection lost — retrying (%d/%d)", attempt+1, maxAutoResumes))
			select {
			case <-ctx.Done():
				err = context.Canceled
			case <-time.After(wait):
			}
			if errors.Is(err, context.Canceled) {
				break
			}
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

	// TikTok / ByteDance CDNs reject any request that doesn't look like a media
	// element load — force the fetch-metadata headers a <video> sends. Harmless
	// elsewhere, so only gate on the host. Work on a copy; j.Headers is shared.
	headers := map[string]string{}
	for k, v := range j.Headers {
		headers[k] = v
	}
	if isTikTokMedia(j.URL) {
		def := map[string]string{
			"Sec-Fetch-Dest": "video", "Sec-Fetch-Mode": "no-cors",
			"Sec-Fetch-Site": "same-site", "Accept": "*/*",
			"Referer":    "https://www.tiktok.com/",
			"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		}
		for k, v := range def {
			if !headerSet(headers, k) {
				headers[k] = v
			}
		}
	}

	// Probe first: learn the real filename, and — crucially — bail out of the
	// raw engine if the URL is actually a web page (text/html). Many "video"
	// links a user clicks are embed pages; hand those to the extractor instead
	// of saving the HTML as a file.
	if pr, e := m.eng.ProbeURL(ctx, j.URL, headers); e == nil {
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
		// A manifest the URL did not advertise. Kinds are guessed from the path
		// suffix, but plenty of CDNs serve HLS and DASH from an extensionless
		// path or behind a signed query, and saving those as a file hands the
		// user a few hundred bytes of playlist text named like a video — with
		// the job reported as completed. The response says what it really is.
		if hit, ok := sniff.ClassifyResponse(j.URL, pr.ContentType, "", pr.TotalBytes); ok &&
			(hit.Kind == store.KindHLS || hit.Kind == store.KindDASH) {
			_, _ = m.st.Update(id, func(jj *store.Job) { jj.Kind = hit.Kind })
			jj, _ := m.st.Get(id)
			return m.execStream(ctx, id, jj)
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
		Headers: headers,
		Conns:   j.Connections,
	}
	probe, err := m.eng.Run(ctx, spec, func(p engine.Progress) {
		now := time.Now()
		if now.Sub(lastPersist) < 200*time.Millisecond && p.DoneBytes < p.TotalBytes {
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
		if now.Sub(lastPersist) < 200*time.Millisecond && p.DoneBytes < p.TotalBytes {
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

func headerSet(h map[string]string, key string) bool {
	for k := range h {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// isTikTokMedia matches tiktok.com hosts and the ByteDance video CDNs.
func isTikTokMedia(rawurl string) bool {
	u, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, ".tiktok.com") || h == "tiktok.com" ||
		strings.Contains(h, "tiktokcdn") || strings.HasSuffix(h, ".tiktokv.com") ||
		strings.HasSuffix(h, ".muscdn.com") || strings.HasSuffix(h, ".byteoversea.com")
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

// autoResumeBackoff is the base wait between automatic job retries (grows
// linearly per attempt). A var so tests can shorten it.
var autoResumeBackoff = 3 * time.Second

// transientErr reports whether an exec failure is worth an automatic retry.
// It's a denylist: retry anything that isn't obviously permanent (bad URL,
// DRM, auth/not-found, or a user cancel).
func transientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errDRM) {
		return false
	}
	// Running out of disk is permanent until the user frees some. Retrying
	// changes nothing, and "connection lost — retrying" would misdescribe it.
	var noSpace *diskspace.Error
	if errors.As(err, &noSpace) {
		return false
	}
	s := strings.ToLower(err.Error())
	permanent := []string{
		"drm", "unsupported url", "byte-range fragment", "not a downloadable",
		"http 400", "http 401", "http 403", "http 404", "http 410",
		" 400 ", " 401 ", " 403 ", " 404 ", " 410 ",
		"no fragments", "playlist has no segments", "no downloadable tracks",
	}
	for _, p := range permanent {
		if strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func (m *Manager) setStatus(id, status, msg string) {
	_, _ = m.st.Update(id, func(j *store.Job) {
		j.Status = status
		// A scheduler hold owns the note explaining itself. Cancelling a job
		// makes its goroutine report "paused" with no message on the way out,
		// which would wipe "waiting for the download window" the instant the
		// scheduler wrote it. A real message still wins.
		if !j.ScheduledHold || msg != "" {
			j.Error = msg
		}
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
	m.stopOnce.Do(func() { close(m.stopped) })
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

// safeName reduces an arbitrary string to a single path-safe filename component:
// no directory separators, no "..", no leading/trailing dots, bounded length.
func safeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(s)
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", "_")
	}
	s = strings.Trim(s, ". ")
	if len(s) > 200 {
		// Whole characters, not bytes: half a character is not valid UTF-8 and
		// makes a filename Finder renders as mangled text.
		s = strings.Trim(strings.ToValidUTF8(s[:200], ""), ". ")
	}
	if s == "" || s == "_" {
		return ""
	}
	return s
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
