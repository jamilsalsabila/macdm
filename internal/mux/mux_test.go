package mux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findFfmpeg looks where the daemon looks. The tests skip when it is absent so
// a checkout without the bundled tools still passes.
func findFfmpeg(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Library", "Application Support", "MacDM", "bin", "ffmpeg"),
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return c
		}
	}
	t.Skip("ffmpeg not available")
	return ""
}

// makeClip renders a short synthetic clip so the tests do not need a fixture.
func makeClip(t *testing.T, ffmpeg, path string, seconds string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", seconds, "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot render a test clip (%v): %s", err, out)
	}
}

func TestRemuxProducesPlayableFileAndProgress(t *testing.T) {
	ffmpeg := findFfmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	makeClip(t, ffmpeg, src, "2")

	m := New(ffmpeg)
	out := filepath.Join(dir, "out.mp4")
	var maxFraction float64
	var ticks int
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.Remux(ctx, src, out, func(p Progress) {
		ticks++
		if p.Fraction > maxFraction {
			maxFraction = p.Fraction
		}
		if p.Fraction < 0 || p.Fraction > 1 {
			t.Errorf("fraction out of range: %v", p.Fraction)
		}
	}); err != nil {
		t.Fatalf("Remux: %v", err)
	}

	fi, err := os.Stat(out)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no output produced: %v", err)
	}
	if ticks == 0 {
		t.Error("progress was never reported — the -progress pipe parsing is broken")
	}
	if maxFraction < 0.5 {
		t.Errorf("progress only reached %.2f; duration detection is probably broken", maxFraction)
	}
	if !strings.Contains(m.Probe(ctx, out), "Stream #") {
		t.Error("output has no streams — not a playable file")
	}
}

func TestCombineMergesVideoAndAudio(t *testing.T) {
	ffmpeg := findFfmpeg(t)
	dir := t.TempDir()
	full := filepath.Join(dir, "full.mp4")
	makeClip(t, ffmpeg, full, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Split into the video-only / audio-only pair the DASH path assembles.
	vOnly := filepath.Join(dir, "v.mp4")
	aOnly := filepath.Join(dir, "a.m4a")
	for _, a := range [][]string{
		{"-y", "-i", full, "-c", "copy", "-an", vOnly},
		{"-y", "-i", full, "-c", "copy", "-vn", aOnly},
	} {
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-hide_banner", "-loglevel", "error"}, a...)...).
			CombinedOutput(); err != nil {
			t.Skipf("cannot split the clip (%v): %s", err, out)
		}
	}

	m := New(ffmpeg)
	out := filepath.Join(dir, "merged.mp4")
	if err := m.Combine(ctx, vOnly, aOnly, out, nil); err != nil {
		t.Fatalf("Combine: %v", err)
	}
	streams := m.Probe(ctx, out)
	if !strings.Contains(streams, "Video:") || !strings.Contains(streams, "Audio:") {
		t.Fatalf("merged file is missing a track:\n%s", streams)
	}
}

// A failure must surface ffmpeg's own message, not a bare exit status.
func TestRunSurfacesFfmpegError(t *testing.T) {
	ffmpeg := findFfmpeg(t)
	m := New(ffmpeg)
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-media.mp4")
	if err := os.WriteFile(bad, []byte("this is not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := m.Remux(context.Background(), bad, filepath.Join(dir, "o.mp4"), nil)
	if err == nil {
		t.Fatal("expected an error for a non-media input")
	}
	if !strings.HasPrefix(err.Error(), "ffmpeg: ") || len(err.Error()) < 12 {
		t.Errorf("error should carry ffmpeg's message, got %q", err)
	}
}

// Cancelling mid-mux must report ctx.Err() so the manager treats it as a pause,
// not a failed download.
func TestRunReportsCancellation(t *testing.T) {
	ffmpeg := findFfmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	makeClip(t, ffmpeg, src, "1")

	m := New(ffmpeg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we start
	err := m.Remux(ctx, src, filepath.Join(dir, "o.mp4"), nil)
	if err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestNoFfmpegPathIsAnError(t *testing.T) {
	m := New("")
	if err := m.Remux(context.Background(), "a", "b", nil); err == nil {
		t.Fatal("expected an error when ffmpeg is not configured")
	}
}

// A dubbed track muxed without a language tag keeps whatever the source
// claimed, so players show the wrong language — the download then looks like
// it ignored the audio-language setting.
func TestCombineLangTagsAudio(t *testing.T) {
	ff := findFfmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	makeClip(t, ff, clip, "1")

	v := filepath.Join(dir, "v.mp4")
	a := filepath.Join(dir, "a.m4a")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"-i", clip, "-c", "copy", "-an", v},
		{"-i", clip, "-c", "copy", "-vn", a},
	} {
		full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
		if out, err := exec.CommandContext(ctx, ff, full...).CombinedOutput(); err != nil {
			t.Skipf("cannot split the clip (%v): %s", err, out)
		}
	}

	out := filepath.Join(dir, "out.mp4")
	if err := New(ff).CombineLang(ctx, v, a, out, "ind", nil); err != nil {
		t.Fatalf("CombineLang: %v", err)
	}
	probe := New(ff).Probe(ctx, out)
	if !strings.Contains(probe, "Audio:") {
		t.Fatalf("no audio track:\n%s", probe)
	}
	if !strings.Contains(probe, "(ind)") {
		t.Fatalf("audio not tagged 'ind':\n%s", probe)
	}
}
