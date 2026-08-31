package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"macdm/internal/dash"
	"macdm/internal/mux"
	"macdm/internal/store"
)

// durationOf reads a media file's length in seconds via ffmpeg.
func durationSeconds(t *testing.T, ffmpeg, file string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-i", file).CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "Duration:") {
			continue
		}
		f := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(ln, ",", 2)[0], "Duration:"))
		p := strings.Split(f, ":")
		if len(p) != 3 {
			return 0
		}
		h, _ := strconv.ParseFloat(p[0], 64)
		m, _ := strconv.ParseFloat(p[1], 64)
		s, _ := strconv.ParseFloat(p[2], 64)
		return h*3600 + m*60 + s
	}
	return 0
}

// A two-period MPD must produce a video as long as both periods put together.
// Only the first period used to be downloaded, and the job still reported
// success — a silently truncated file.
func TestDASHMultiPeriodConcatenatesAllPeriods(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := t.TempDir()

	// Two 2s clips with identical encoding parameters, so a stream copy can
	// join them, each split into video-only and audio-only TS pieces.
	for i, name := range []string{"a", "b"} {
		clip := filepath.Join(media, name+".mp4")
		run(t, ffmpeg, "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=160x120:rate=10:duration=2"),
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=2", 300+i*100),
			"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-ar", "44100", "-ac", "1", clip)
		run(t, ffmpeg, "-i", clip, "-c", "copy", "-an", "-f", "mpegts", filepath.Join(media, name+"_v.ts"))
		run(t, ffmpeg, "-i", clip, "-c", "copy", "-vn", "-f", "mpegts", filepath.Join(media, name+"_a.ts"))
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media/manifest.mpd" {
			w.Header().Set("Content-Type", "application/dash+xml")
			period := func(vf, af string) string {
				return fmt.Sprintf(`  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="800000" width="160" height="120">
        <BaseURL>%s/%s</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" contentType="audio">
      <Representation id="a" bandwidth="128000">
        <BaseURL>%s/%s</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
`, srv.URL, vf, srv.URL, af)
			}
			fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT4S">
%s%s</MPD>`, period("a_v.ts", "a_a.ts"), period("b_v.ts", "b_a.ts"))
			return
		}
		http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()

	dl := t.TempDir()
	m, st := newStreamMgr(t, ffmpeg, dl)
	j := &store.Job{
		ID: "multiperiod", Kind: store.KindDASH, URL: srv.URL + "/media/manifest.mpd",
		Dest: filepath.Join(dl, "twoparts.mp4"), Filename: "twoparts.mp4",
		Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("execStream: %v", err)
	}

	final, _ := st.Get(j.ID)
	if final.Status != store.StatusCompleted {
		t.Fatalf("status = %q (err=%q)", final.Status, final.Error)
	}
	if _, err := os.Stat(final.Dest); err != nil {
		t.Fatalf("no output: %v", err)
	}
	streams := mux.New(ffmpeg).Probe(ctx, final.Dest)
	if !strings.Contains(streams, "Video:") || !strings.Contains(streams, "Audio:") {
		t.Fatalf("tracks missing:\n%s", streams)
	}
	// Each period is 2s. One period alone would be ~2s; both give ~4s.
	d := durationSeconds(t, ffmpeg, final.Dest)
	if d < 3.0 {
		t.Fatalf("output is %.2fs — only the first period was downloaded (want ~4s)", d)
	}
}

// The parser must surface every period, not just the first.
func TestDASHParseExposesAllPeriods(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v1" bandwidth="1" width="160" height="120"><BaseURL>%s/1.ts</BaseURL></Representation>
    </AdaptationSet>
  </Period>
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v2" bandwidth="1" width="160" height="120"><BaseURL>%s/2.ts</BaseURL></Representation>
    </AdaptationSet>
  </Period>
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v3" bandwidth="1" width="160" height="120"><BaseURL>%s/3.ts</BaseURL></Representation>
    </AdaptationSet>
  </Period>
</MPD>`, srv.URL, srv.URL, srv.URL)
	}))
	defer srv.Close()

	man, err := dashParse(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Periods) != 3 {
		t.Fatalf("want 3 periods, got %d", len(man.Periods))
	}
	for i, pd := range man.Periods {
		if pd.Video == nil || len(pd.Video.Segments) != 1 {
			t.Fatalf("period %d has no video track", i)
		}
		want := fmt.Sprintf("/%d.ts", i+1)
		if !strings.HasSuffix(pd.Video.Segments[0], want) {
			t.Errorf("period %d points at %s, want %s", i, pd.Video.Segments[0], want)
		}
	}
	// The singular fields still describe the first period.
	if man.Video == nil || man.Video.Segments[0] != man.Periods[0].Video.Segments[0] {
		t.Error("Manifest.Video should alias the first period")
	}
}

func dashParse(t *testing.T, srv *httptest.Server) (*dash.Manifest, error) {
	t.Helper()
	return dash.NewClient(srv.Client(), nil).ParseQuality(context.Background(), srv.URL+"/m.mpd", 0)
}

// #EXT-X-DISCONTINUITY marks a segment whose timeline restarts. This already
// works because mux.Remux passes -fflags +genpts, which regenerates timestamps
// across the join; the test guards that flag from being dropped.
func TestHLSDiscontinuityKeepsFullDuration(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := t.TempDir()
	for i, n := range []string{"a", "b"} {
		clip := filepath.Join(media, n+".mp4")
		run(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=2", 300+i*100),
			"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-ar", "44100", "-ac", "1", clip)
		run(t, ffmpeg, "-i", clip, "-c", "copy", "-f", "mpegts", filepath.Join(media, n+".ts"))
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/i.m3u8" {
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/a.ts\n"+
				"#EXT-X-DISCONTINUITY\n#EXTINF:2.0,\n%s/b.ts\n#EXT-X-ENDLIST\n", srv.URL, srv.URL)
			return
		}
		http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()

	dl := t.TempDir()
	m, st := newStreamMgr(t, ffmpeg, dl)
	j := &store.Job{ID: "disc", Kind: store.KindHLS, URL: srv.URL + "/i.m3u8",
		Dest: filepath.Join(dl, "disc.mp4"), Filename: "disc.mp4", Status: store.StatusQueued}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("execStream: %v", err)
	}
	final, _ := st.Get(j.ID)
	if d := durationSeconds(t, ffmpeg, final.Dest); d < 3.5 {
		t.Fatalf("output is %.2fs — the segment after the discontinuity was lost (want ~4s)", d)
	}
}
