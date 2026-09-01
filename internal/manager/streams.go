package manager

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"macdm/internal/config"
	"macdm/internal/dash"
	"macdm/internal/diskspace"
	"macdm/internal/engine"
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
	// Kept on failure: segments land via an atomic rename, so a retry reuses the
	// ones already fetched instead of re-downloading the whole stream. Cleared on
	// success, when the job is removed, and wholesale at daemon start.
	wd := m.workDir(id)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}

	dest := ensureExt(j.Dest, ".mp4")
	if looksGeneric(filepath.Base(dest)) {
		name := "video-" + time.Now().Format("20060102-150405")
		if t := pageTitle(ctx, j.Headers["Referer"]); t != "" {
			name = sanitize(t)
		}
		dest = filepath.Join(filepath.Dir(dest), name+".mp4")
	}
	if j.Status == store.StatusQueued {
		dest = m.uniqueDest(id, dest)
	}
	if dest != j.Dest {
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
	var estTotal int64
	var avgBps float64 // EWMA so the rate doesn't snap to 0 between segments
	segProgress := func(sp streamProg) {
		now := time.Now()
		var bps int64
		if dt := now.Sub(lastT).Seconds(); dt > 0.4 {
			inst := float64(sp.bytes-lastBytes) / dt
			lastBytes, lastT = sp.bytes, now
			if avgBps == 0 && inst > 0 {
				avgBps = inst
			} else {
				avgBps += 0.25 * (inst - avgBps)
			}
			bps = int64(avgBps)
		}
		estTotal = estimateStreamTotal(sp, estTotal)
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			jj.DoneBytes = sp.bytes
			jj.TotalBytes = estTotal
			jj.Segments = sp.totalSeg
			jj.SegmentsDone = sp.doneSeg
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
	// completedBytes counts only finished segments, and is what the size
	// estimate must divide — bytes includes everything still in flight.
	completedBytes int64
	conns          []store.ConnStat
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
	c.SetLimiter(m.limiter)

	// A FormatID that is a URL is a specific variant playlist the user picked.
	startURL := j.URL
	if strings.HasPrefix(j.FormatID, "http") {
		startURL = j.FormatID
	}

	master, err := c.Parse(ctx, startURL)
	if err != nil {
		return err
	}
	pl := master
	// A variant with AUDIO="grp" is normally video-only; its audio is a separate
	// #EXT-X-MEDIA rendition. Downloading only the variant gave a silent file.
	var audioPl *hls.Playlist
	var subRend *hls.Rendition
	var ccDeclared bool
	var ccLang string
	// BANDWIDTH of the chosen variant, kept for the disk-space estimate below.
	// Per the HLS spec it already accounts for the audio rendition played with
	// it, so it covers both tracks.
	var variantBps int
	if master.IsMaster {
		v, ok := master.BestVariant()
		if !ok {
			return fmt.Errorf("master playlist has no variants")
		}
		if pl, err = c.Parse(ctx, v.URL); err != nil {
			return err
		}
		if r := master.AudioFor(v); r != nil {
			if audioPl, err = c.Parse(ctx, r.URI); err != nil {
				return fmt.Errorf("audio rendition %q: %w", r.Name, err)
			}
		}
		subRend = master.SubtitlesFor(v)
		ccDeclared, ccLang = master.ClosedCaptionsFor(v)
		variantBps = v.Bandwidth
	}
	for _, p := range []*hls.Playlist{pl, audioPl} {
		if p == nil {
			continue
		}
		if p.HasDRM() {
			return drm("HLS stream uses SAMPLE-AES")
		}
		if p.Live {
			return fmt.Errorf("this is a live stream (no #EXT-X-ENDLIST) — MacDM only downloads finished VOD")
		}
	}

	var playlistSeconds float64
	for _, sg := range pl.Segments {
		playlistSeconds += sg.Duration
	}
	if err := planSpace(wd, dest, bitrateBytes(playlistSeconds, variantBps)); err != nil {
		return err
	}

	// Segment counts and bytes from both tracks feed one progress bar, the way
	// execDASH already reports its video+audio pair.
	totalSeg := len(pl.Segments)
	if audioPl != nil {
		totalSeg += len(audioPl.Segments)
	}
	var doneSegBase int
	var doneBytesBase int64
	track := func(p hls.Progress) {
		prog(streamProg{
			doneSeg:        doneSegBase + p.Segment,
			totalSeg:       totalSeg,
			bytes:          doneBytesBase + p.DoneBytes,
			completedBytes: doneBytesBase + p.CompletedBytes,
			conns:          hlsConns(p.Workers),
		})
	}

	assembled := filepath.Join(wd, "assembled.ts")
	if err := c.Assemble(ctx, pl, hls.AssembleOptions{
		Dir: wd, OutFile: assembled, Conns: m.cfg.Engine.MaxConns,
	}, track); err != nil {
		return err
	}

	var audioFile string
	if audioPl != nil {
		doneSegBase = len(pl.Segments)
		if fi, e := os.Stat(assembled); e == nil {
			doneBytesBase = fi.Size()
		}
		audioFile = filepath.Join(wd, "assembled-audio.ts")
		// Its own scratch subdir: both tracks name segments seg-NNNNNN, so
		// sharing one directory would have them overwrite each other.
		audioDir := filepath.Join(wd, "audio")
		if err := os.MkdirAll(audioDir, 0o755); err != nil {
			return err
		}
		if err := c.Assemble(ctx, audioPl, hls.AssembleOptions{
			Dir: audioDir, OutFile: audioFile, Conns: m.cfg.Engine.MaxConns,
		}, track); err != nil {
			return err
		}
	}

	// Mux into the scratch dir and move the finished file into place, rather
	// than pointing ffmpeg at the user's Downloads folder: a cancelled or failed
	// mux would otherwise leave a truncated .mp4 sitting there under the final
	// name, indistinguishable from a completed download. Same guarantee the
	// HTTP engine gets from .part + rename.
	muxed := filepath.Join(wd, "muxed"+extOr(dest, ".mp4"))
	if audioFile != "" {
		if err := mx.Combine(ctx, assembled, audioFile, muxed, muxProg("Merging video + audio")); err != nil {
			return err
		}
	} else if err := mx.Remux(ctx, assembled, muxed, muxProg("Finalising")); err != nil {
		return err
	}
	if err := moveFile(muxed, dest); err != nil {
		return err
	}
	m.fetchHLSSubtitles(ctx, c, id, dest, subRend)
	if ccDeclared {
		m.extractClosedCaptions(ctx, mx, id, assembled, dest, ccLang)
	}
	_ = os.RemoveAll(wd) // assembled successfully — scratch segments no longer needed
	return finalize(m, id, dest)
}

// fetchHLSSubtitles saves the chosen subtitle rendition beside the video.
// Entirely best-effort: the video is already on disk by this point.
func (m *Manager) fetchHLSSubtitles(ctx context.Context, c *hls.Client, id, dest string, r *hls.Rendition) {
	if r == nil {
		return
	}
	sp, err := c.Parse(ctx, r.URI)
	if err != nil {
		log.Printf("macdm: subtitle playlist %q: %v", r.Name, err)
		return
	}
	data, err := c.FetchSubtitles(ctx, sp)
	if err != nil {
		log.Printf("macdm: subtitles %q: %v", r.Name, err)
		return
	}
	lang := strings.TrimSpace(r.Language)
	if lang == "" {
		lang = r.Name
	}
	m.writeSubtitles(id, dest, lang, ".vtt", data)
}

// execExtractDirect is the fast path: yt-dlp resolves the media URLs, MacDM's
// own engine transfers them, ffmpeg merges. yt-dlp downloads a format over one
// connection and YouTube throttles that hard — measured on one file, 1.3 MB/s
// through yt-dlp against 6.2 MB/s for the same URL over eight connections.
//
// Reports ErrNoDirectPath when the page cannot be fetched this way (a manifest,
// DRM, live, an expired or rejected URL); the caller then runs yt-dlp normally.
func (m *Manager) execExtractDirect(ctx context.Context, id string, j *store.Job,
	ex *extractor.Extractor, opt extractor.DownloadOptions, outDir, finalDir string,
	mx *mux.Muxer, prog segProgFn, muxProg muxProgFn) error {

	// A pause or shutdown is not "this page cannot be fetched directly": wrapping
	// it would make the caller wipe the scratch dir and restart through yt-dlp,
	// throwing away everything downloaded so far on every pause.
	noDirect := func(err error) error {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return context.Canceled
		}
		return fmt.Errorf("%w: %v", errNoDirectPath, err)
	}

	plan, err := ex.ResolveDirect(ctx, j.URL, opt)
	if err != nil {
		return noDirect(err)
	}

	name := sanitize(plan.Title)
	if name == "" || looksGeneric(name) {
		name = strings.TrimSuffix(filepath.Base(j.Dest), filepath.Ext(j.Dest))
	}

	// Total bytes across the streams, so one progress bar covers them all.
	var total int64
	for _, med := range []*extractor.DirectMedia{plan.Video, plan.Audio, plan.Muxed} {
		if med != nil {
			total += med.Size
		}
	}

	// Here the sizes are exact rather than estimated from a bitrate, so this is
	// the one stream path that can say for certain the download will not fit.
	// A refusal is final: it is not a reason to fall back to yt-dlp, which would
	// only run out of space more slowly.
	if err := planSpace(outDir, j.Dest, total); err != nil {
		return err
	}
	var doneBase int64
	fetch := func(med *extractor.DirectMedia, file string) error {
		// The resolver hands back a freshly signed URL every time, so the sidecar
		// must key on something stable or every resume would restart from zero.
		ident := fmt.Sprintf("%s|%s|%d", j.URL, med.Kind, med.Size)
		_, err := m.eng.Run(ctx, engine.DownloadSpec{
			URL: med.URL, Dest: file, Headers: med.Headers, Conns: j.Connections,
			Identity: ident,
		}, func(p engine.Progress) {
			prog(streamProg{
				doneSeg: 0, totalSeg: 0,
				bytes: doneBase + p.DoneBytes,
				conns: engineConns(p.Conns),
			})
			_, _ = m.st.Update(id, func(jj *store.Job) {
				jj.TotalBytes = total
				jj.DoneBytes = doneBase + p.DoneBytes
				jj.SpeedBps = p.SpeedBps
			})
		})
		if err == nil {
			if fi, e := os.Stat(file); e == nil {
				doneBase += fi.Size()
			}
		}
		return err
	}

	var vFile, aFile, muxedIn string
	switch {
	case plan.Muxed != nil:
		muxedIn = filepath.Join(outDir, "media."+extOrPlain(plan.Muxed.Ext, "mp4"))
		if err := fetch(plan.Muxed, muxedIn); err != nil {
			return noDirect(err)
		}
	default:
		vFile = filepath.Join(outDir, "video."+extOrPlain(plan.Video.Ext, "mp4"))
		if err := fetch(plan.Video, vFile); err != nil {
			return noDirect(err)
		}
		aFile = filepath.Join(outDir, "audio."+extOrPlain(plan.Audio.Ext, "m4a"))
		if err := fetch(plan.Audio, aFile); err != nil {
			return noDirect(err)
		}
	}

	// Past this point the bytes are ours: a mux failure is a real error, not a
	// reason to re-download everything through yt-dlp.
	muxed := filepath.Join(outDir, "muxed.mp4")
	if muxedIn != "" {
		if err := mx.Remux(ctx, muxedIn, muxed, muxProg("Finalising")); err != nil {
			return err
		}
	} else if err := mx.CombineLang(ctx, vFile, aFile, muxed,
		extractor.ISO6392(opt.AudioLang), muxProg("Merging video + audio")); err != nil {
		return err
	}

	final := m.uniqueDest(id, filepath.Join(finalDir, name+".mp4"))
	if err := moveFile(muxed, final); err != nil {
		return err
	}
	for _, sub := range plan.Subtitles {
		data, err := fetchURL(ctx, sub.URL, opt.CookiesFrom == "", plan.Video)
		if err != nil {
			log.Printf("macdm: subtitle %s: %v", sub.Lang, err)
			continue
		}
		m.writeSubtitles(id, final, sub.Lang, "."+extOrPlain(sub.Ext, "srt"), data)
	}
	_ = os.RemoveAll(outDir)
	return finalize(m, id, final)
}

// errNoDirectPath marks a page the fast path cannot handle; the caller retries
// with yt-dlp doing the download.
var errNoDirectPath = errors.New("no direct download path")

func extOrPlain(ext, fallback string) string {
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if ext == "" {
		return fallback
	}
	return ext
}

// fetchURL grabs a small resource (a subtitle file), reusing the media headers
// so a host that checks User-Agent or Referer still answers.
func fetchURL(ctx context.Context, url string, _ bool, like *extractor.DirectMedia) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if like != nil {
		for k, v := range like.Headers {
			req.Header.Set(k, v)
		}
	}
	resp, err := streamHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// One byte over the cap: a LimitReader that simply stops gives back a
	// truncated file and no error at all, which for a subtitle means a track
	// that quietly ends early.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 16<<20 {
		return nil, fmt.Errorf("GET %s: larger than the 16 MB subtitle limit", url)
	}
	return data, nil
}

// moveSubtitleSidecars carries the .srt/.vtt files yt-dlp wrote beside the
// video in the scratch dir over to the final location, renaming them onto the
// final stem so "<video>.<lang>.srt" still lines up after uniqueDest picked a
// different name. Best-effort: the video is already in place.
func moveSubtitleSidecars(scratchVideo, finalVideo, outDir string) {
	oldStem := strings.TrimSuffix(filepath.Base(scratchVideo), filepath.Ext(scratchVideo))
	newStem := strings.TrimSuffix(finalVideo, filepath.Ext(finalVideo))

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".srt" && ext != ".vtt" && ext != ".ass" {
			continue
		}
		// yt-dlp names them "<video stem>.<lang>.<ext>" — keep the tail.
		tail := strings.TrimPrefix(name, oldStem)
		if tail == name { // not ours
			continue
		}
		if err := moveFile(filepath.Join(outDir, name), newStem+tail); err != nil {
			log.Printf("macdm: subtitle sidecar %s: %v", name, err)
		}
	}
}

// firstNonBlank returns the first argument that is not empty after trimming.
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// firstPeriodVideoFile returns the first muxed period, which is where in-band
// captions are read from. Multi-period presentations only get the captions of
// their opening period; extracting from every one and stitching the timelines
// is not worth it for a track that is a bonus.
func firstPeriodVideoFile(periodFiles []string) string {
	if len(periodFiles) == 0 {
		return ""
	}
	return periodFiles[0]
}

// extractClosedCaptions saves CEA-608/708 captions carried inside the video
// bitstream as an SRT beside the finished file. Best-effort and silent when
// there are none — most videos carry no captions at all.
func (m *Manager) extractClosedCaptions(ctx context.Context, mx *mux.Muxer, id, video, dest, lang string) {
	tmp := video + ".cc.srt"
	found, err := mx.ExtractClosedCaptions(ctx, video, tmp)
	if err != nil {
		log.Printf("macdm: closed captions: %v", err)
		return
	}
	if !found {
		return
	}
	defer os.Remove(tmp)
	data, err := os.ReadFile(tmp)
	if err != nil {
		return
	}
	m.writeSubtitles(id, dest, lang, ".srt", data)
}

// writeSubtitles saves a subtitle track next to the video as
// "<name>.<lang>.vtt". Subtitles are a bonus: every failure here is logged into
// the job note and swallowed, because losing them must never fail a download
// that otherwise succeeded.
func (m *Manager) writeSubtitles(id, dest, lang, ext string, data []byte) {
	if len(data) == 0 {
		return
	}
	stem := strings.TrimSuffix(dest, filepath.Ext(dest))
	// Only sanitise a language we actually have: sanitize("") returns the
	// "download" placeholder, which would name the file "<video>.download.vtt".
	if lang = strings.TrimSpace(lang); lang != "" {
		stem += "." + sanitize(lang)
	}
	path := m.uniqueDest(id, stem+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("macdm: subtitles for %s: %v", id, err)
	}
}

// extOr returns path's extension, or fallback when it has none.
func extOr(path, fallback string) string {
	if e := filepath.Ext(path); e != "" {
		return e
	}
	return fallback
}

func (m *Manager) execDASH(ctx context.Context, id string, j *store.Job, wd, dest string, mx *mux.Muxer, prog segProgFn, muxProg muxProgFn) error {
	c := dash.NewClient(streamHTTPClient(), j.Headers)
	c.SetLimiter(m.limiter)

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

	// Every period counts towards one progress bar. Downloading only the first
	// period used to silently truncate the video and still report success.
	total := 0
	for _, pd := range man.Periods {
		if pd.Video != nil {
			total += len(pd.Video.Segments)
		}
		if pd.Audio != nil {
			total += len(pd.Audio.Segments)
		}
	}
	// The busiest period's bitrate, not the sum: periods run one after another,
	// so they never overlap in time. Taking the largest overestimates slightly,
	// which is the safe direction for a space check.
	peakBps := 0
	for _, pd := range man.Periods {
		bps := 0
		if pd.Video != nil {
			bps += pd.Video.Bandwidth
		}
		if pd.Audio != nil {
			bps += pd.Audio.Bandwidth
		}
		if bps > peakBps {
			peakBps = bps
		}
	}
	if err := planSpace(wd, dest, bitrateBytes(man.Duration.Seconds(), peakBps)); err != nil {
		return err
	}

	var doneSegBase, doneBytesBase int64
	step := func(p dash.Progress) {
		prog(streamProg{
			doneSeg:  int(doneSegBase) + p.Segment,
			totalSeg: total,
			bytes:    doneBytesBase + p.DoneBytes,
			// A finished track's bytes are all completed bytes, so the same
			// base serves both.
			completedBytes: doneBytesBase + p.CompletedBytes,
			conns:          dashConns(p.Workers),
		})
	}

	audioOnly := true
	var periodFiles []string
	for i, pd := range man.Periods {
		// Each period gets its own scratch dir: tracks name segments
		// "<kind>-NNNNNN", which would collide between periods.
		pdir := filepath.Join(wd, fmt.Sprintf("p%03d", i))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			return err
		}
		opts := dash.DownloadOptions{Dir: pdir, Conns: m.cfg.Engine.MaxConns}

		var vFile, aFile string
		if pd.Video != nil {
			audioOnly = false
			vFile = filepath.Join(pdir, "video.m4s")
			if err := c.AssembleTrack(ctx, pd.Video, vFile, opts, step); err != nil {
				return err
			}
			if fi, e := os.Stat(vFile); e == nil {
				doneBytesBase += fi.Size()
			}
			doneSegBase += int64(len(pd.Video.Segments))
		}
		if pd.Audio != nil {
			aFile = filepath.Join(pdir, "audio.m4s")
			if err := c.AssembleTrack(ctx, pd.Audio, aFile, opts, step); err != nil {
				return err
			}
			if fi, e := os.Stat(aFile); e == nil {
				doneBytesBase += fi.Size()
			}
			doneSegBase += int64(len(pd.Audio.Segments))
		}

		stage := "Finalising"
		if len(man.Periods) > 1 {
			stage = fmt.Sprintf("Finalising part %d of %d", i+1, len(man.Periods))
		}
		muxed := filepath.Join(pdir, "muxed.mp4")
		switch {
		case vFile != "" && aFile != "":
			if err := mx.Combine(ctx, vFile, aFile, muxed, muxProg("Merging video + audio")); err != nil {
				return err
			}
		case vFile != "":
			if err := mx.Remux(ctx, vFile, muxed, muxProg(stage)); err != nil {
				return err
			}
		case aFile != "":
			muxed = filepath.Join(pdir, "muxed.m4a")
			if err := mx.Remux(ctx, aFile, muxed, muxProg(stage)); err != nil {
				return err
			}
		default:
			continue
		}
		periodFiles = append(periodFiles, muxed)
	}
	if len(periodFiles) == 0 {
		return fmt.Errorf("no tracks to assemble")
	}
	if audioOnly {
		dest = ensureExt(dest, ".m4a")
	}

	// Concat re-times each input, which is what makes joining periods correct:
	// every period has its own init segment and its own timeline. A single
	// period takes the plain remux path inside Concat.
	final := filepath.Join(wd, "muxed"+extOr(dest, ".mp4"))
	if err := mx.Concat(ctx, periodFiles, final, muxProg("Joining parts")); err != nil {
		return err
	}
	if err := moveFile(final, dest); err != nil {
		return err
	}
	// Only when the MPD declares them: the extraction decodes the whole video,
	// roughly a minute per hour, which is not worth spending blindly.
	if man.CaptionLang != "" {
		if vTrack := firstPeriodVideoFile(periodFiles); vTrack != "" {
			m.extractClosedCaptions(ctx, mx, id, vTrack, dest, man.CaptionLang)
		}
	}
	if man.Subtitle != nil {
		if data, ext, err := c.FetchSubtitles(ctx, man.Subtitle); err != nil {
			log.Printf("macdm: subtitles: %v", err) // best-effort, never fatal
		} else {
			m.writeSubtitles(id, dest, man.Subtitle.Language, ext, data)
		}
	}
	_ = os.RemoveAll(wd) // assembled successfully — scratch segments no longer needed
	return finalize(m, id, dest)
}

func (m *Manager) execExtract(ctx context.Context, id string, j *store.Job) error {
	ex, err := extractor.New(m.cfg.Tools)
	if err != nil {
		return err
	}

	finalDir := m.cfg.DownloadDir
	if d := filepath.Dir(j.Dest); d != "." && d != "" {
		finalDir = d
	}
	// yt-dlp downloads into a per-job scratch dir; we then move the result to a
	// non-clobbering path (yt-dlp itself would silently skip a name that already
	// exists, leaving the job with no file).
	// NB: the scratch dir is deliberately NOT removed on failure. yt-dlp resumes
	// its own .part files, so an automatic (or manual) retry continues instead of
	// re-downloading gigabytes from zero. It is cleared on success below, when
	// the job is removed, and wholesale at daemon start.
	outDir := m.workDir(id)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
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
		case <-time.After(150 * time.Second):
			close(stalled)
			wdCancel() // kills the yt-dlp subprocess via ctx
		}
	}()

	cfg := config.Load() // fresh — Settings can change these without a restart
	opt := extractor.DownloadOptions{
		OutDir:         outDir,
		FormatSelector: j.FormatID,
		CookiesFrom:    cfg.CookiesFrom,
		MergeFormat:    "mp4",
		AudioLang:      firstNonBlank(j.AudioLang, cfg.AudioLang),
		SubLangs:       firstNonBlank(j.SubtitleLangs, cfg.SubtitleLangs),
		AutoSubs:       cfg.AutoSubs,
	}

	// Fast path: let yt-dlp resolve the URLs and download them ourselves, over
	// many connections. Falls through to yt-dlp's own downloader for anything it
	// cannot address — a manifest, DRM, live, or a URL the host rejects.
	mx := mux.New(m.cfg.Tools.Ffmpeg)
	muxProg := func(stage string) func(mux.Progress) {
		return func(p mux.Progress) {
			_, _ = m.st.Update(id, func(jj *store.Job) {
				jj.SpeedBps = 0
				jj.Conns = []store.ConnStat{{
					Status: "receiving",
					Info:   fmt.Sprintf("%s (ffmpeg) — %d%%", stage, int(p.Fraction*100)),
				}}
			})
		}
	}
	segProg := func(sp streamProg) {
		// Disarm the watchdog. It exists to catch an extraction that never
		// starts, and it was only ever disarmed by yt-dlp's own progress
		// callback — so on this path, which is the one YouTube normally takes,
		// it stayed armed and cancelled the job at 150 seconds. Every download
		// longer than that died silently, reported as "paused" with no reason.
		select {
		case started <- struct{}{}:
		default:
		}
		_, _ = m.st.Update(id, func(jj *store.Job) {
			jj.Status = store.StatusDownloading
			jj.Conns = sp.conns
		})
	}
	if m.cfg.Tools.Ffmpeg != "" {
		err := m.execExtractDirect(wdCtx, id, j, ex, opt, outDir, finalDir, mx, segProg, muxProg)
		if err == nil {
			return nil
		}
		// A watchdog kill arrives here as a plain cancellation, which the caller
		// reads as "the user paused it" — a job that stopped for a reason would
		// then sit there explaining nothing. Say what happened.
		select {
		case <-stalled:
			return fmt.Errorf("gave up waiting for this video to start downloading (150s) — it may be throttled or need a newer yt-dlp (Settings → Update now)")
		default:
		}
		if !errors.Is(err, errNoDirectPath) {
			return err
		}
		log.Printf("macdm: direct download unavailable (%v) — falling back to yt-dlp", err)
		// Start yt-dlp from a clean slate: partial engine output would confuse it.
		_ = os.RemoveAll(outDir)
		if e := os.MkdirAll(outDir, 0o755); e != nil {
			return e
		}
	}

	res, err := ex.Download(wdCtx, j.URL, extractor.DownloadOptions{
		OutDir:         outDir,
		FormatSelector: j.FormatID, // "" => extractor's 1080p-capped default
		CookiesFrom:    cfg.CookiesFrom,
		MergeFormat:    "mp4",
		AudioLang:      firstNonBlank(j.AudioLang, cfg.AudioLang),
		SubLangs:       firstNonBlank(j.SubtitleLangs, cfg.SubtitleLangs),
		AutoSubs:       cfg.AutoSubs,
		// yt-dlp downloads in its own process, out of reach of the shared
		// bucket, so the ceiling has to be handed to it directly.
		LimitBps: m.limiter.Limit(),
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
			return fmt.Errorf("yt-dlp couldn't resolve this video within 150s — it may be throttled or need a newer yt-dlp (Settings → Update now)")
		default:
		}
		if strings.Contains(strings.ToLower(err.Error()), "drm") {
			return drm(err.Error())
		}
		return err
	}
	if res.Path == "" {
		return fmt.Errorf("yt-dlp produced no file")
	}
	final := m.uniqueDest(id, filepath.Join(finalDir, filepath.Base(res.Path)))
	if err := moveFile(res.Path, final); err != nil {
		return err
	}
	moveSubtitleSidecars(res.Path, final, outDir)
	_ = os.RemoveAll(outDir)
	return finalize(m, id, final)
}

// moveFile renames, falling back to copy+remove across filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
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
		// Same reasoning as sanitize: cut whole characters, not bytes.
		title = strings.TrimSpace(strings.ToValidUTF8(title[:120], ""))
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

// planSpace refuses a stream download that cannot possibly fit, before a single
// byte is fetched.
//
// A stream needs room twice over: every segment lands in the scratch directory
// and the muxed result is written beside it, and the scratch is only cleared
// once that has succeeded. Scratch and destination are usually the same volume,
// so the two have to be weighed together — which is what diskspace.CheckAll
// does, while still handling a destination on an external drive.
//
// The estimate comes from the manifest's own declared bitrate and is therefore
// approximate. It is used only to turn away a download that misses by a wide
// margin; est <= 0 means "no idea", and never blocks anything.
func planSpace(wd, dest string, est int64) error {
	if est <= 0 {
		return nil
	}
	return diskspace.CheckAll(
		diskspace.Need{Path: wd, Bytes: est},
		diskspace.Need{Path: dest, Bytes: est},
	)
}

// bitrateBytes converts a declared bitrate and a duration into a byte count.
func bitrateBytes(seconds float64, bitsPerSec int) int64 {
	if seconds <= 0 || bitsPerSec <= 0 {
		return 0
	}
	return int64(seconds * float64(bitsPerSec) / 8)
}

// estimateStreamTotal sizes a whole stream from the segments that have already
// finished, so the % bar can advance smoothly instead of stepping once per
// segment.
//
// Only completed segments may be divided. Dividing the running byte count
// instead counts the partial bytes of every segment still in flight, which
// inflates the answer by roughly the number of workers — measured at 247 MB
// for a 28 MB stream on eight connections. The old code then clamped the
// estimate to never decrease, locking that first wild guess in: at 97% of
// segments done the bar still read 12%, and only snapped to 100% at the end.
//
// The estimate is therefore allowed to be revised, and is floored at the bytes
// already in hand so the bar can never read past 100%.
func estimateStreamTotal(sp streamProg, prev int64) int64 {
	est := prev
	if sp.doneSeg > 0 && sp.totalSeg > 0 && sp.completedBytes > 0 {
		est = sp.completedBytes * int64(sp.totalSeg) / int64(sp.doneSeg)
	}
	if est < sp.bytes {
		est = sp.bytes
	}
	return est
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
