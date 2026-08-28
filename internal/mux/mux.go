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
	"strconv"
	"strings"
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
	dur := m.durationOf(ctx, video)
	return m.run(ctx, dur, onProgress,
		"-i", video, "-i", audio, "-c", "copy",
		"-map", "0:v:0", "-map", "1:a:0", out)
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
