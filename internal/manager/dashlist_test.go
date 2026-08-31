package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macdm/internal/engine"
	"macdm/internal/mux"
	"macdm/internal/store"
	"macdm/internal/tools"
)

// A DASH presentation using SegmentList addressing must download and mux like
// any other: this form used to be skipped outright with "no downloadable
// tracks (unsupported segment addressing?)".
func TestDASHSegmentListEndToEnd(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := t.TempDir()

	// Fragmented MP4 so init + media segments concatenate into a playable file.
	clip := filepath.Join(media, "clip.mp4")
	run(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", clip)
	// dash muxer emits init-stream$N$.m4s + chunk-stream$N$-$Number$.m4s
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-f", "dash",
		"-seg_duration", "1", "-use_template", "0", "-use_timeline", "0",
		filepath.Join(media, "out.mpd"))

	names, err := filepath.Glob(filepath.Join(media, "*.m4s"))
	if err != nil || len(names) == 0 {
		t.Skipf("ffmpeg produced no DASH segments: %v", err)
	}
	var vSegs, aSegs []string
	var vInit, aInit string
	for _, n := range names {
		b := filepath.Base(n)
		switch {
		case strings.HasPrefix(b, "init-stream0"):
			vInit = b
		case strings.HasPrefix(b, "init-stream1"):
			aInit = b
		case strings.HasPrefix(b, "chunk-stream0"):
			vSegs = append(vSegs, b)
		case strings.HasPrefix(b, "chunk-stream1"):
			aSegs = append(aSegs, b)
		}
	}
	if vInit == "" || aInit == "" || len(vSegs) == 0 || len(aSegs) == 0 {
		t.Skipf("unexpected ffmpeg dash layout: v=%d a=%d vi=%q ai=%q", len(vSegs), len(aSegs), vInit, aInit)
	}

	segURLs := func(names []string) string {
		var b strings.Builder
		for _, n := range names {
			fmt.Fprintf(&b, "          <SegmentURL media=\"%s\"/>\n", n)
		}
		return b.String()
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media/manifest.mpd" {
			w.Header().Set("Content-Type", "application/dash+xml")
			fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT2S">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="800000" width="160" height="120">
        <SegmentList duration="1" timescale="1">
          <Initialization sourceURL="%s"/>
%s        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" contentType="audio">
      <Representation id="a" bandwidth="128000">
        <SegmentList duration="1" timescale="1">
          <Initialization sourceURL="%s"/>
%s        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, vInit, segURLs(vSegs), aInit, segURLs(aSegs))
			return
		}
		http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()

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
		Engine:      engine.Config{MaxConns: 2, MinChunk: 1 << 20},
	}, st)

	j := &store.Job{
		ID: "dashlist", Kind: store.KindDASH, URL: srv.URL + "/media/manifest.mpd",
		Dest: filepath.Join(dl, "dashout.mp4"), Filename: "dashout.mp4",
		Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("execStream: %v", err)
	}

	final, err := st.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.StatusCompleted {
		t.Fatalf("status = %q, want completed (err=%q)", final.Status, final.Error)
	}
	if fi, err := os.Stat(final.Dest); err != nil || fi.Size() == 0 {
		t.Fatalf("no output at %s: %v", final.Dest, err)
	}
	streams := mux.New(ffmpeg).Probe(ctx, final.Dest)
	if !strings.Contains(streams, "Video:") {
		t.Errorf("no video track in the SegmentList output:\n%s", streams)
	}
	if !strings.Contains(streams, "Audio:") {
		t.Errorf("no audio track in the SegmentList output:\n%s", streams)
	}
}
