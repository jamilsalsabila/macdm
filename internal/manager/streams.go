package manager

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"macdm/internal/dash"
	"macdm/internal/extractor"
	"macdm/internal/hls"
	"macdm/internal/mux"
	"macdm/internal/store"
)

// streamHTTPClient has no overall timeout — a single segment can be a whole
// on-demand track file — but the shared transport caps response-header wait and
// the job context handles cancellation.
func streamHTTPClient() *http.Client {
	return &http.Client{Transport: sharedTransport}
}

// errDRM is returned (wrapped) when a stream is protected; the runner maps it to
// StatusDRM instead of StatusError so the UI can say "can't — DRM" plainly.
var errDRM = errors.New("DRM-protected")

func drm(msg string) error { return fmt.Errorf("%s: %w", msg, errDRM) }

func (m *Manager) workDir(id string) string { return filepath.Join(m.cfg.WorkDir, id) }

// execStream handles KindHLS and KindDASH: fetch the manifest, download every
// segment concurrently, then remux/mux into a single container file.
func (m *Manager) execStream(ctx context.Context, id string, j *store.Job) error {
	ffmpeg, err := m.cfg.Tools.RequireFfmpeg()
	if err != nil {
		return err
	}
	mx := mux.New(ffmpeg)
	wd := m.workDir(id)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(wd)

	dest := ensureExt(j.Dest, ".mp4")
	if looksGeneric(filepath.Base(dest)) {
		name := "video-" + time.Now().Format("20060102-150405")
		if t := pageTitle(ctx, j.Headers["Referer"]); t != "" {
			name = sanitize(t)
		}
		dest = filepath.Join(filepath.Dir(dest), name+".mp4")
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Dest = dest
			jj.Filename = filepath.Base(dest)
		})
	}
	// segProgress reports segment counts, real bytes, and per-connection state.
	// The file size stays "unknown" (0) — an adaptive stream has no single size —
	// but done/total track segments so the % bar works; finalize() sets bytes.
	var lastBytes int64
	var lastT = time.Now()
	segProgress := func(sp streamProg) {
		now := time.Now()
		var bps int64
		if dt := now.Sub(lastT).Seconds(); dt > 0.4 {
			bps = int64(float64(sp.bytes-lastBytes) / dt)
			lastBytes, lastT = sp.bytes, now
		}
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			jj.DoneBytes = int64(sp.doneSeg)
			jj.TotalBytes = int64(sp.totalSeg)
			jj.Connections = len(sp.conns)
			if bps > 0 {
				jj.SpeedBps = bps
			}
			jj.Conns = sp.conns
		})
	}

	muxProgress := func(stage string) func(mux.Progress) {
		return func(p mux.Progress) {
			_, _ = m.st.Update(id, func(jj *store.Job) {
				jj.SpeedBps = 0
				pct := int(p.Fraction * 100)
				jj.Conns = []store.ConnStat{{
					Status: "receiving",
					Info:   fmt.Sprintf("%s (ffmpeg) — %d%%", stage, pct),
				}}
			})
		}
	}

	switch j.Kind {
	case store.KindHLS:
		return m.execHLS(ctx, id, j, wd, dest, mx, segProgress, muxProgress)
	case store.KindDASH:
		return m.execDASH(ctx, id, j, wd, dest, mx, segProgress, muxProgress)
	default:
		return fmt.Errorf("not a stream kind: %s", j.Kind)
	}
}

type streamProg struct {
	doneSeg, totalSeg int
	bytes             int64
	conns             []store.ConnStat
}
type segProgFn = func(streamProg)
type muxProgFn = func(stage string) func(mux.Progress)

func hlsConns(ws []hls.WorkerState) []store.ConnStat {
	out := make([]store.ConnStat, len(ws))
	for i, w := range ws {
		out[i] = workerRow(w.ID, w.Segment, w.Status, w.Bytes)
	}
	return out
}
func dashConns(ws []dash.WorkerState) []store.ConnStat {
	out := make([]store.ConnStat, len(ws))
	for i, w := range ws {
		out[i] = workerRow(w.ID, w.Segment, w.Status, w.Bytes)
	}
	return out
}
func workerRow(id, seg int, status string, bytes int64) store.ConnStat {
	info := "Idle"
	switch status {
	case "connecting":
		info = fmt.Sprintf("Connecting — segment %d", seg)
	case "receiving":
		info = fmt.Sprintf("Receiving data — segment %d", seg)
	}
	return store.ConnStat{
		Index: id, Downloaded: bytes, Total: 0, Status: status, Info: info,
	}
}

func (m *Manager) execHLS(ctx context.Context, id string, j *store.Job, wd, dest string, mx *mux.Muxer, prog segProgFn, muxProg muxProgFn) error {
	c := hls.NewClient(streamHTTPClient(), j.Headers)

	// A FormatID that is a URL is a specific variant playlist the user picked.
	startURL := j.URL
	if strings.HasPrefix(j.FormatID, "http") {
		startURL = j.FormatID
	}

	pl, err := c.Parse(ctx, startURL)
	if err != nil {
		return err
	}
	if pl.IsMaster {
		v, ok := pl.BestVariant()
		if !ok {
			return fmt.Errorf("master playlist has no variants")
		}
		if pl, err = c.Parse(ctx, v.URL); err != nil {
			return err
		}
	}
	if pl.HasDRM() {
		return drm("HLS stream uses SAMPLE-AES")
	}
	if pl.Live {
		return fmt.Errorf("this is a live stream (no #EXT-X-ENDLIST) — MacDM only downloads finished VOD")
	}

	assembled := filepath.Join(wd, "assembled.ts")
	err = c.Assemble(ctx, pl, hls.AssembleOptions{
		Dir: wd, OutFile: assembled, Conns: m.cfg.Engine.MaxConns,
	}, func(p hls.Progress) {
		prog(streamProg{p.Segment, p.Total, p.DoneBytes, hlsConns(p.Workers)})
	})
	if err != nil {
		return err
	}

	if err := mx.Remux(ctx, assembled, dest, muxProg("Finalising")); err != nil {
		return err
	}
	return finalize(m, id, dest)
}

func (m *Manager) execDASH(ctx context.Context, id string, j *store.Job, wd, dest string, mx *mux.Muxer, prog segProgFn, muxProg muxProgFn) error {
	c := dash.NewClient(streamHTTPClient(), j.Headers)

	preferHeight := 0
	if strings.HasPrefix(j.FormatID, "h") {
		fmt.Sscanf(j.FormatID, "h%d", &preferHeight)
	}
	man, err := c.ParseQuality(ctx, j.URL, preferHeight)
	if err != nil {
		if strings.Contains(err.Error(), "DRM-protected") {
			return drm(err.Error())
		}
		return err
	}

	total := 0
	if man.Video != nil {
		total += len(man.Video.Segments)
	}
	if man.Audio != nil {
		total += len(man.Audio.Segments)
	}
	var prevTrackBytes, prevTrackSeg int64
	step := func(p dash.Progress) {
		prog(streamProg{
			doneSeg:  int(prevTrackSeg) + p.Segment,
			totalSeg: total,
			bytes:    prevTrackBytes + p.DoneBytes,
			conns:    dashConns(p.Workers),
		})
	}

	var vFile, aFile string
	if man.Video != nil {
		vFile = filepath.Join(wd, "video.m4s")
		if err := c.AssembleTrack(ctx, man.Video, vFile, dash.DownloadOptions{Dir: wd, Conns: m.cfg.Engine.MaxConns}, step); err != nil {
			return err
		}
		if fi, e := os.Stat(vFile); e == nil {
			prevTrackBytes = fi.Size()
		}
		prevTrackSeg = int64(len(man.Video.Segments))
	}
	if man.Audio != nil {
		aFile = filepath.Join(wd, "audio.m4s")
		if err := c.AssembleTrack(ctx, man.Audio, aFile, dash.DownloadOptions{Dir: wd, Conns: m.cfg.Engine.MaxConns}, step); err != nil {
			return err
		}
	}

	switch {
	case vFile != "" && aFile != "":
		if err := mx.Combine(ctx, vFile, aFile, dest, muxProg("Merging video + audio")); err != nil {
			return err
		}
	case vFile != "":
		if err := mx.Remux(ctx, vFile, dest, muxProg("Finalising")); err != nil {
			return err
		}
	case aFile != "":
		dest = ensureExt(dest, ".m4a")
		if err := mx.Remux(ctx, aFile, dest, muxProg("Finalising")); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no tracks to assemble")
	}
	return finalize(m, id, dest)
}

// execExtract handles KindExtract: hand the page URL to yt-dlp, which resolves
// the real media (running the site's cipher logic) and merges with ffmpeg.
func (m *Manager) execExtract(ctx context.Context, id string, j *store.Job) error {
	ex, err := extractor.New(m.cfg.Tools)
	if err != nil {
		return err
	}

	outDir := m.cfg.DownloadDir
	if d := filepath.Dir(j.Dest); d != "." && d != "" {
		outDir = d
	}

	// Watchdog: if yt-dlp never starts downloading (stuck in extraction —
	// throttled, broken extractor, dead network) fail with a clear message
	// instead of leaving the job on "Resolving…" forever.
	wdCtx, wdCancel := context.WithCancel(ctx)
	defer wdCancel()
	started := make(chan struct{}, 1)
	stalled := make(chan struct{})
	go func() {
		select {
		case <-started:
		case <-wdCtx.Done():
		case <-time.After(75 * time.Second):
			close(stalled)
			wdCancel() // kills the yt-dlp subprocess via ctx
		}
	}()

	res, err := ex.Download(wdCtx, j.URL, extractor.DownloadOptions{
		OutDir:         outDir,
		FormatSelector: j.FormatID, // "" => extractor's 1080p-capped default
		CookiesFrom:    m.cfg.CookiesFrom,
		MergeFormat:    "mp4",
	}, func(p extractor.Progress) {
		select {
		case started <- struct{}{}: // first callback — extraction got somewhere
		default:
		}
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			switch p.Stage {
			case "title":
				if p.Title != "" && looksGeneric(jj.Filename) {
					jj.Filename = sanitizeTitle(p.Title) + ".mp4"
				}
				return
			case "merge":
				jj.SpeedBps = 0
				jj.Conns = []store.ConnStat{{Downloaded: jj.DoneBytes, Total: jj.TotalBytes,
					Status: "receiving", Info: "Merging video + audio (ffmpeg)…"}}
				return
			}
			jj.DoneBytes = p.DoneBytes
			if p.TotalBytes > 0 {
				jj.TotalBytes = p.TotalBytes
			}
			jj.SpeedBps = p.SpeedBps
			jj.Conns = []store.ConnStat{{
				Downloaded: p.DoneBytes, Total: 0, Status: "receiving",
				Info: fmt.Sprintf("yt-dlp · %s of %s @ %s/s",
					humanBytes(p.DoneBytes), humanBytes(p.TotalBytes), humanBytes(p.SpeedBps)),
			}}
		})
	})
	if err != nil {
		select {
		case <-stalled:
			return fmt.Errorf("yt-dlp couldn't resolve this video within 75s — it may be throttled or need a newer yt-dlp (Settings → Update now)")
		default:
		}
		if strings.Contains(strings.ToLower(err.Error()), "drm") {
			return drm(err.Error())
		}
		return err
	}
	return finalize(m, id, res.Path)
}

func finalize(m *Manager, id, path string) error {
	fi, _ := os.Stat(path)
	_, _ = m.st.Update(id, func(jj *store.Job) {
		jj.Status = store.StatusCompleted
		jj.Dest = path
		jj.Filename = filepath.Base(path)
		jj.SpeedBps = 0
		if fi != nil {
			jj.TotalBytes = fi.Size()
			jj.DoneBytes = fi.Size()
		}
		// normalise the detail row so the bar/table read 100%
		jj.Conns = []store.ConnStat{{
			Downloaded: jj.TotalBytes, Total: jj.TotalBytes,
			Status: "done", Info: "Complete",
		}}
	})
	return nil
}

// uniqueDest returns path if nothing is there, otherwise "name (2).ext",
// "name (3).ext"… so a second download of the same video does not clobber the
// first. It also skips names already claimed by another job's destination.
func (m *Manager) uniqueDest(selfID, path string) string {
	taken := map[string]bool{}
	for _, j := range m.st.List() {
		if j.ID != selfID && j.Dest != "" && !j.Terminal() {
			taken[j.Dest] = true
		}
	}
	free := func(p string) bool {
		if taken[p] {
			return false
		}
		for _, cand := range []string{p, p + ".part", p + ".macdm", p + ".macdmf"} {
			if _, err := os.Stat(cand); err == nil {
				return false
			}
		}
		return true
	}
	if free(path) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if free(cand) {
			return cand
		}
	}
	return path
}

func ensureExt(path, ext string) string {
	if strings.EqualFold(filepath.Ext(path), ext) {
		return path
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}

func sanitizeTitle(s string) string { return sanitize(s) }

// pageTitle fetches refererURL and returns its <title>, trimmed of common site
// suffixes. Best-effort — "" on any failure.
func pageTitle(ctx context.Context, refererURL string) string {
	if refererURL == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, refererURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	lc := strings.ToLower(string(body))
	i := strings.Index(lc, "<title")
	if i < 0 {
		return ""
	}
	j := strings.Index(lc[i:], ">")
	if j < 0 {
		return ""
	}
	rest := string(body)[i+j+1:]
	k := strings.Index(strings.ToLower(rest), "</title>")
	if k < 0 {
		return ""
	}
	title := html.UnescapeString(strings.TrimSpace(rest[:k]))
	// drop trailing " - YouTube", " | Site", etc.
	for _, sep := range []string{" - ", " | ", " – ", " — "} {
		if idx := strings.LastIndex(title, sep); idx > 10 {
			title = title[:idx]
		}
	}
	if len(title) > 120 {
		title = title[:120]
	}
	return title
}

// looksGeneric reports whether a filename is a URL-derived placeholder rather
// than a real title (so a resolved title should replace it).
func looksGeneric(name string) bool {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	switch base {
	case "", "watch", "download", "index", "video", "media", "playlist",
		"embed", "master", "manifest", "chunklist", "stream", "hls", "dash", "out":
		return true
	}
	if strings.Contains(base, "watch?v=") || strings.HasPrefix(base, "videoplayback") {
		return true
	}
	// pure number ("0", "1080", a hash fragment) or very short
	if _, err := strconv.Atoi(base); err == nil {
		return true
	}
	return len(base) <= 2
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1024 {
			if u == "B" {
				return fmt.Sprintf("%.0f %s", f, u)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f PB", f)
}
