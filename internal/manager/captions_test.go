package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macdm/internal/engine"
	"macdm/internal/store"
	"macdm/internal/tools"
)

// CEA-608 captions live inside the video bitstream, not in a track of their
// own, so nothing downloads them by accident — they have to be decoded out.
// This runs against DASH-IF's reference stream, which declares
// <Accessibility ... value="CC1=eng;CC3=swe"/>.
func TestDASHClosedCaptionsExtracted(t *testing.T) {
	// Downloads a full hour from DASH-IF's reference server (~135 MB, minutes),
	// so it is opt-in rather than part of every run: MACDM_E2E=1 go test ./...
	if os.Getenv("MACDM_E2E") == "" {
		t.Skip("set MACDM_E2E=1 to run the DASH-IF reference download")
	}
	ffmpeg := ffmpegPath(t)
	dl := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	m := New(Config{
		DownloadDir: dl,
		WorkDir:     filepath.Join(t.TempDir(), "work"),
		MaxActive:   2,
		Tools:       tools.Set{Ffmpeg: ffmpeg},
		Engine:      engine.Config{MaxConns: 4, MinChunk: 1 << 20},
	}, st)

	j := &store.Job{
		ID: "cc", Kind: store.KindDASH,
		URL:      "https://livesim2.dashif.org/vod/testpic_2s/cea608.mpd",
		Dest:     filepath.Join(dl, "cc.mp4"),
		Filename: "cc.mp4", Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Skipf("reference stream unavailable: %v", err)
	}

	final, _ := st.Get(j.ID)
	stem := strings.TrimSuffix(final.Dest, filepath.Ext(final.Dest))
	// The MPD declares CC1=eng, so the sidecar is language-tagged.
	srt := stem + ".eng.srt"
	body, err := os.ReadFile(srt)
	if err != nil {
		got, _ := filepath.Glob(stem + "*")
		t.Fatalf("no caption sidecar at %s (found: %v)", srt, got)
	}
	text := string(body)
	if !strings.Contains(text, "-->") {
		t.Fatalf("sidecar is not valid SRT:\n%s", text[:min(200, len(text))])
	}
	if strings.Contains(text, `{\an`) {
		t.Error("ASS positioning codes leaked into the sidecar")
	}
	if !strings.Contains(text, "eng") {
		t.Errorf("expected the English caption text:\n%s", text[:min(200, len(text))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
