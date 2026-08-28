// Package extractor is the "active" download path: given a *page* URL (a YouTube
// watch page, a Vimeo link, ...), it uses yt-dlp to resolve the real media —
// running the site's signature/cipher logic, choosing formats, and merging
// video+audio with ffmpeg.
//
// This is the complement to the sniffer. The sniffer reuses a request the
// browser already made; the extractor reproduces what the browser's player
// would have done.
package extractor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"macdm/internal/store"
	"macdm/internal/tools"
)

// Format is one downloadable rendition reported by yt-dlp.
type Format struct {
	ID           string  `json:"format_id"`
	Ext          string  `json:"ext"`
	Resolution   string  `json:"resolution"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	FPS          float64 `json:"fps"`
	VCodec       string  `json:"vcodec"`
	ACodec       string  `json:"acodec"`
	Filesize     int64   `json:"filesize"`
	FilesizeAppx int64   `json:"filesize_approx"`
	TBR          float64 `json:"tbr"`
	Protocol     string  `json:"protocol"`
	Note         string  `json:"format_note"`
}

// HasVideo / HasAudio interpret yt-dlp's "none" sentinel.
func (f Format) HasVideo() bool { return f.VCodec != "" && f.VCodec != "none" }
func (f Format) HasAudio() bool { return f.ACodec != "" && f.ACodec != "none" }

func (f Format) Size() int64 {
	if f.Filesize > 0 {
		return f.Filesize
	}
	return f.FilesizeAppx
}

// Info is the subset of yt-dlp's -J dump MacDM uses.
type Info struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Extractor string   `json:"extractor_key"`
	Duration  float64  `json:"duration"`
	Thumbnail string   `json:"thumbnail"`
	IsLive    bool     `json:"is_live"`
	Formats   []Format `json:"formats"`
	// DRM-protected entries set this; MacDM refuses those.
	HasDRMField bool `json:"_has_drm"`
}

// QualityChoices collapses yt-dlp's format list into the handful of user-facing
// options a quality menu should show: one row per distinct video height/fps
// ("1080p60", "720p", …), plus an "Audio only" row. Each carries a yt-dlp -f
// selector in ID.
func (in *Info) QualityChoices() []store.FormatChoice {
	type key struct {
		h   int
		f50 bool
	}
	seen := map[key]store.FormatChoice{}
	var bestAudio int64

	for _, f := range in.Formats {
		if f.HasAudio() && !f.HasVideo() {
			if s := f.Size(); s > bestAudio {
				bestAudio = s
			}
			continue
		}
		if !f.HasVideo() {
			continue
		}
		h := f.Height
		if h == 0 {
			h = heightFromResolution(f.Resolution)
		}
		if h == 0 {
			continue
		}
		hi := f.FPS >= 50
		k := key{h, hi}
		fps := 30
		if hi {
			fps = 60
		}
		label := fmt.Sprintf("%dp", h)
		if hi {
			label = fmt.Sprintf("%dp%d", h, fps)
		}
		sel := fmt.Sprintf("bv*[height<=%d][fps<=%d]+ba/bv*[height<=%d]+ba/b[height<=%d]", h, fps+5, h, h)
		cur, ok := seen[k]
		if !ok || f.Size() > cur.SizeBytes {
			seen[k] = store.FormatChoice{
				ID: sel, Label: label, Height: h, FPS: fps,
				Ext: "mp4", SizeBytes: f.Size(), Kind: "video+audio",
			}
		}
	}

	out := make([]store.FormatChoice, 0, len(seen)+1)
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height > out[j].Height
		}
		return out[i].FPS > out[j].FPS
	})
	out = append(out, store.FormatChoice{
		ID: "ba/bestaudio", Label: "Audio only", Ext: "m4a",
		SizeBytes: bestAudio, Kind: "audio",
	})
	return out
}

func heightFromResolution(res string) int {
	// "1920x1080" -> 1080
	if i := strings.LastIndexByte(res, 'x'); i >= 0 {
		h, _ := strconv.Atoi(strings.TrimSpace(res[i+1:]))
		return h
	}
	return 0
}

// Extractor wraps a yt-dlp binary.
type Extractor struct {
	bin    string
	ffmpeg string
}

// New returns an Extractor or a helpful error if yt-dlp is missing.
func New(t tools.Set) (*Extractor, error) {
	bin, err := t.RequireYtDlp()
	if err != nil {
		return nil, err
	}
	return &Extractor{bin: bin, ffmpeg: t.Ffmpeg}, nil
}

// Probe resolves formats for a page URL without downloading.
func (e *Extractor) Probe(ctx context.Context, pageURL string) (*Info, error) {
	// --retries 1: a probe must be quick — don't let yt-dlp's default
	// retry-with-backoff hold up the New Download dialog. (Keep the socket
	// timeout at yt-dlp's default; YouTube's player-JS fetch can be slow.)
	args := []string{
		"-J", "--no-warnings", "--no-playlist", "--retries", "1",
	}
	if e.ffmpeg != "" {
		args = append(args, "--ffmpeg-location", e.ffmpeg)
	}
	args = append(args, pageURL)

	out, err := exec.CommandContext(ctx, e.bin, args...).Output()
	if err != nil {
		return nil, wrapYtErr(err)
	}
	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp json: %w", err)
	}
	return &info, nil
}

// DownloadOptions steers a download.
type DownloadOptions struct {
	FormatSelector string // yt-dlp -f expression; "" => "bv*+ba/b"
	OutDir         string
	CookiesFrom    string // browser name for --cookies-from-browser; "" => none
	MergeFormat    string // container for merged output; "" => "mp4"
}

// Result reports the finished file.
type Result struct {
	Path  string
	Title string
}

// Progress is emitted per yt-dlp progress line.
type Progress struct {
	DoneBytes  int64
	TotalBytes int64
	SpeedBps   int64
	Stage      string // "title", "video", "audio", "merge"
	Title      string // set when Stage == "title"
}

// Download fetches the media for pageURL, merging tracks via ffmpeg, and returns
// the final file path. onProgress may be nil.
func (e *Extractor) Download(ctx context.Context, pageURL string, opt DownloadOptions, onProgress func(Progress)) (*Result, error) {
	sel := opt.FormatSelector
	if sel == "" {
		// Cap at 1080p by default: picking the raw "best" silently pulls 4K /
		// multi-GB files on sites that offer them.
		sel = "bv*[height<=?1080]+ba/b[height<=?1080]/bv*+ba/b"
	}
	merge := opt.MergeFormat
	if merge == "" {
		merge = "mp4"
	}
	outTmpl := filepath.Join(opt.OutDir, "%(title).200B [%(id)s].%(ext)s")

	args := []string{
		// --progress forces progress output even though our stdout is a pipe,
		// not a TTY; without it yt-dlp emits nothing until the file is done.
		"--no-warnings", "--no-playlist", "--newline", "--progress",
		"-f", sel,
		"--merge-output-format", merge,
		"-o", outTmpl,
		// machine-readable progress on stdout, one line per tick. The
		// "download:" scope prefix is required — a bare template is ignored.
		"--progress-template", "download:PROG %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s %(progress.speed)s",
		// title early (printed right after extraction, before download starts)
		// and the final path after yt-dlp moves the merged file
		"--print", "MACDM_TITLE %(title)s",
		"--print", "after_move:MACDM_FILE %(filepath)s",
	}
	if e.ffmpeg != "" {
		args = append(args, "--ffmpeg-location", e.ffmpeg)
	}
	if opt.CookiesFrom != "" {
		args = append(args, "--cookies-from-browser", opt.CookiesFrom)
	}
	args = append(args, pageURL)

	cmd := exec.CommandContext(ctx, e.bin, args...)
	// yt-dlp is Python; when its stdout is a pipe (not a TTY) Python block-
	// buffers it and we'd get every progress line at once on exit. Force
	// line/unbuffered output so progress streams live.
	cmd.Env = append(cmd.Environ(), "PYTHONUNBUFFERED=1")
	// Run yt-dlp in its own process group and, on pause/shutdown, SIGKILL the
	// whole group so the ffmpeg child it spawns for merging dies too instead of
	// being orphaned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var finalPath, title string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// yt-dlp terminates progress lines with a bare CR (even with --newline),
	// which bufio.ScanLines does not treat as a boundary. Split on CR or LF.
	sc.Split(scanLinesCR)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		switch {
		case strings.HasPrefix(line, "MACDM_TITLE "):
			title = strings.TrimSpace(strings.TrimPrefix(line, "MACDM_TITLE "))
			if title != "" && title != "NA" && onProgress != nil {
				onProgress(Progress{Stage: "title", Title: title})
			}
		case strings.HasPrefix(line, "MACDM_FILE "):
			finalPath = strings.TrimSpace(strings.TrimPrefix(line, "MACDM_FILE "))
		case strings.HasPrefix(line, "PROG ") && onProgress != nil:
			if p, ok := parseProg(line); ok {
				onProgress(p)
			}
		case strings.HasPrefix(line, "[Merger]"):
			if onProgress != nil {
				onProgress(Progress{Stage: "merge"})
			}
		}
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err() // pause / shutdown, not a yt-dlp failure
	}
	if werr != nil {
		msg := firstNonEmpty(lastLines(stderr.String(), 3), werr.Error())
		if strings.Contains(strings.ToLower(msg), "drm") {
			return nil, fmt.Errorf("stream is DRM-protected")
		}
		return nil, fmt.Errorf("yt-dlp: %s", msg)
	}
	if finalPath == "" {
		return nil, fmt.Errorf("yt-dlp finished but did not report an output path")
	}
	return &Result{Path: finalPath, Title: title}, nil
}

// scanLinesCR is a bufio.SplitFunc that breaks on '\n' or '\r' (or "\r\n"),
// so carriage-return-updated progress lines each become their own token.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			adv := i + 1
			if b == '\r' && adv < len(data) && data[adv] == '\n' {
				adv++
			}
			return adv, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseProg(line string) (Progress, bool) {
	// PROG <downloaded> <total> <total_estimate> <speed>
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Progress{}, false
	}
	atoi := func(s string) int64 {
		if s == "NA" || s == "" || s == "None" {
			return 0
		}
		f, _ := strconv.ParseFloat(s, 64)
		return int64(f)
	}
	total := atoi(fields[2])
	if total == 0 {
		total = atoi(fields[3])
	}
	return Progress{
		DoneBytes:  atoi(fields[1]),
		TotalBytes: total,
		SpeedBps:   atoi(fields[4]),
	}, true
}

func wrapYtErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		msg := lastLines(string(ee.Stderr), 3)
		if strings.Contains(msg, "DRM") {
			return fmt.Errorf("stream is DRM-protected")
		}
		if msg != "" {
			return fmt.Errorf("yt-dlp: %s", msg)
		}
	}
	return fmt.Errorf("yt-dlp: %w", err)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
