// Package mux wraps ffmpeg for the container operations MacDM needs after a
// stream has been assembled: remux a raw elementary/segmented stream into a
// clean container, and combine a separate video file and audio file into one.
//
// Everything is stream copy (-c copy): no re-encoding, so it is fast and
// lossless. If codecs are incompatible with the target container ffmpeg fails
// and the error is surfaced.
package mux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"macdm/internal/subs"
	"time"
)

// Muxer holds a resolved ffmpeg path.
type Muxer struct{ ffmpeg string }

// New returns a Muxer; ffmpeg must be a valid path.
func New(ffmpeg string) *Muxer { return &Muxer{ffmpeg: ffmpeg} }

// Progress reports how far a mux has got (0..1), plus the media timestamp.
type Progress struct {
	Fraction float64
	OutTime  time.Duration
}

// Remux copies a single assembled input into out. No -movflags +faststart: that
// forces a second full-file rewrite pass (very slow for a movie-length capture)
// and only matters for pseudo-streaming playback, which a downloaded file does
// not need.
func (m *Muxer) Remux(ctx context.Context, in, out string, onProgress func(Progress)) error {
	dur := m.durationOf(ctx, in)
	return m.run(ctx, dur, onProgress,
		"-fflags", "+genpts", "-i", in, "-c", "copy", out)
}

// Combine muxes a video-only file and an audio-only file into out.
func (m *Muxer) Combine(ctx context.Context, video, audio, out string, onProgress func(Progress)) error {
	return m.CombineLang(ctx, video, audio, out, "", onProgress)
}

// CombineLang is Combine with an explicit audio language tag (an ISO 639-2
// code). Without it the container keeps whatever the source claimed, so a
// dubbed track ends up labelled with the wrong language and players show it
// that way.
func (m *Muxer) CombineLang(ctx context.Context, video, audio, out, lang string, onProgress func(Progress)) error {
	dur := m.durationOf(ctx, video)
	args := []string{"-i", video, "-i", audio, "-c", "copy",
		"-map", "0:v:0", "-map", "1:a:0"}
	if lang != "" {
		args = append(args, "-metadata:s:a:0", "language="+lang)
	}
	return m.run(ctx, dur, onProgress, append(args, out)...)
}

// Concat joins already-muxed files end to end with the concat demuxer, which
// re-times each input — that is what makes it correct across DASH periods, where
// every period has its own init segment and its own timeline.
//
// It is still a stream copy, so the inputs must share codecs and parameters.
// When they do not, ffmpeg fails and the error is surfaced rather than a
// silently broken file being produced.
func (m *Muxer) Concat(ctx context.Context, inputs []string, out string, onProgress func(Progress)) error {
	if len(inputs) == 0 {
		return fmt.Errorf("nothing to concatenate")
	}
	if len(inputs) == 1 {
		return m.Remux(ctx, inputs[0], out, onProgress)
	}
	list, err := os.CreateTemp(filepath.Dir(out), "concat-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(list.Name())
	for _, in := range inputs {
		// The demuxer's own quoting: single quotes, with ' written as '\''.
		if _, err := fmt.Fprintf(list, "file '%s'\n", strings.ReplaceAll(in, "'", `'\''`)); err != nil {
			list.Close()
			return err
		}
	}
	if err := list.Close(); err != nil {
		return err
	}

	var total time.Duration
	for _, in := range inputs {
		total += m.durationOf(ctx, in)
	}
	return m.run(ctx, total, onProgress,
		"-f", "concat", "-safe", "0", "-i", list.Name(), "-c", "copy", out)
}

// ExtractClosedCaptions pulls CEA-608/708 captions out of a video file into an
// SRT at out, and reports whether any were found.
//
// These captions ride inside the video bitstream (H.264/H.265 SEI) rather than
// in a track of their own, so ffmpeg only exposes them through the movie
// filter's `subcc` output. A file without captions simply yields nothing, which
// is not an error — most videos have none.
func (m *Muxer) ExtractClosedCaptions(ctx context.Context, video, out string) (bool, error) {
	if m.ffmpeg == "" {
		return false, fmt.Errorf("ffmpeg path not set")
	}
	// The filter takes a filename inside a graph description, where : \ and '
	// are syntax. Escaping keeps a path with spaces or quotes working.
	esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`).Replace(video)
	tmp := out + ".raw"
	defer os.Remove(tmp)

	cmd := exec.CommandContext(ctx, m.ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "movie='"+esc+"'[out+subcc]",
		// -f srt explicitly: the temp file's extension is not .srt, and ffmpeg
		// would otherwise fail to infer a format for it.
		"-map", "0:s:0", "-f", "srt", tmp)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// No caption stream at all: the map fails, and that is the normal case.
		return false, nil
	}
	raw, err := os.ReadFile(tmp)
	if err != nil || len(raw) == 0 {
		return false, nil
	}
	cleaned := subs.CleanSRT(raw)
	if len(cleaned) == 0 {
		return false, nil // captions declared but empty
	}
	return true, os.WriteFile(out, cleaned, 0o644)
}

func (m *Muxer) run(ctx context.Context, total time.Duration, onProgress func(Progress), args ...string) error {
	if m.ffmpeg == "" {
		return fmt.Errorf("ffmpeg path not set")
	}
	full := append([]string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-progress", "pipe:1", "-nostats",
	}, args...)
	cmd := exec.CommandContext(ctx, m.ffmpeg, full...)

	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	// Drain stdout fully before cmd.Wait() — Wait closes the pipe and it is a
	// documented error to call it while reads are still in flight.
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		if stdout == nil {
			return
		}
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
			if !ok || onProgress == nil {
				continue
			}
			us, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			ot := time.Duration(us) * time.Microsecond
			frac := 0.0
			if total > 0 {
				frac = float64(ot) / float64(total)
				if frac > 1 {
					frac = 1
				}
			}
			onProgress(Progress{Fraction: frac, OutTime: ot})
		}
	}()
	<-scanDone

	err := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err() // pause / shutdown, not a mux failure
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg: %s", lastLine(msg))
	}
	return nil
}

// durationOf reads a media file's duration (best-effort, 0 if unknown).
func (m *Muxer) durationOf(ctx context.Context, file string) time.Duration {
	out, _ := exec.CommandContext(ctx, m.ffmpeg, "-hide_banner", "-i", file).CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "Duration:") {
			continue
		}
		// Duration: 01:23:45.67, ...
		field := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(ln, ",", 2)[0], "Duration:"))
		var h, mi, s, cs int
		if _, err := fmt.Sscanf(field, "%d:%d:%d.%d", &h, &mi, &s, &cs); err == nil {
			return time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute +
				time.Duration(s)*time.Second + time.Duration(cs)*10*time.Millisecond
		}
	}
	return 0
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Probe reports the stream summary of a file. Best-effort; "" on failure.
func (m *Muxer) Probe(ctx context.Context, file string) string {
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	out, _ := exec.CommandContext(ctx, m.ffmpeg, "-hide_banner", "-i", file).CombinedOutput()
	var streams []string
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.Contains(ln, "Stream #") {
			streams = append(streams, strings.TrimSpace(ln))
		}
	}
	return strings.Join(streams, "\n")
}
