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
	"macdm/internal/store"
	"macdm/internal/tools"
)

// buildAV renders a short clip and splits it into video-only and audio-only TS
// segments, returning the media directory.
func buildAV(t *testing.T, ffmpeg string) string {
	t.Helper()
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	run(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", clip)
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-an", "-f", "mpegts", filepath.Join(dir, "v0.ts"))
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-vn", "-f", "mpegts", filepath.Join(dir, "a0.ts"))
	return dir
}

func newStreamMgr(t *testing.T, ffmpeg, dl string) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return New(Config{
		DownloadDir: dl,
		WorkDir:     filepath.Join(t.TempDir(), "work"),
		MaxActive:   2,
		Tools:       tools.Set{Ffmpeg: ffmpeg},
		Engine:      engine.Config{MaxConns: 2, MinChunk: 1 << 20},
	}, st), st
}

// Two VTT segments, each timed from its own local clock via X-TIMESTAMP-MAP —
// the shape that naive concatenation gets wrong.
const vttSeg0 = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n" +
	"00:00:00.500 --> 00:00:01.500\nhello\n"
const vttSeg1 = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:90000,LOCAL:00:00:00.000\n\n" +
	"00:00:00.200 --> 00:00:01.000\nworld\n"

func TestHLSSubtitlesWrittenAsSidecar(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := buildAV(t, ffmpeg)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,URI="%s/audio.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="%s/subs.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,AUDIO="aud",SUBTITLES="subs"
%s/video.m3u8
`, srv.URL, srv.URL, srv.URL)
		case "/video.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/v0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		case "/audio.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/a0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		case "/subs.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/s0.vtt\n#EXTINF:1.0,\n%s/s1.vtt\n#EXT-X-ENDLIST\n", srv.URL, srv.URL)
		case "/s0.vtt":
			fmt.Fprint(w, vttSeg0)
		case "/s1.vtt":
			fmt.Fprint(w, vttSeg1)
		default:
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		}
	}))
	defer srv.Close()

	dl := t.TempDir()
	m, st := newStreamMgr(t, ffmpeg, dl)
	j := &store.Job{
		ID: "subs", Kind: store.KindHLS, URL: srv.URL + "/master.m3u8",
		Dest: filepath.Join(dl, "movie.mp4"), Filename: "movie.mp4", Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("execStream: %v", err)
	}

	final, _ := st.Get(j.ID)
	stem := strings.TrimSuffix(final.Dest, filepath.Ext(final.Dest))
	vtt := stem + ".en.vtt"
	body, err := os.ReadFile(vtt)
	if err != nil {
		t.Fatalf("no subtitle sidecar at %s: %v", vtt, err)
	}
	got := string(body)
	if n := strings.Count(got, "WEBVTT"); n != 1 {
		t.Errorf("want one WEBVTT header, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("cues missing:\n%s", got)
	}
	// Second segment's map is MPEGTS:90000 => +1s, so 00:00:00.200 becomes
	// 00:00:01.200. Getting this wrong stacks both segments at zero.
	if !strings.Contains(got, "00:00:01.200 --> 00:00:02.000") {
		t.Errorf("second segment not shifted onto the presentation timeline:\n%s", got)
	}
}

// Subtitles are a bonus. A broken subtitle playlist must not cost the video.
func TestHLSSubtitleFailureDoesNotFailTheVideo(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := buildAV(t, ffmpeg)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, `#EXTM3U
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="%s/broken.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,SUBTITLES="subs"
%s/video.m3u8
`, srv.URL, srv.URL)
		case "/video.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/v0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		case "/broken.m3u8":
			http.Error(w, "gone", http.StatusNotFound)
		default:
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		}
	}))
	defer srv.Close()

	dl := t.TempDir()
	m, st := newStreamMgr(t, ffmpeg, dl)
	j := &store.Job{
		ID: "brokensubs", Kind: store.KindHLS, URL: srv.URL + "/master.m3u8",
		Dest: filepath.Join(dl, "movie2.mp4"), Filename: "movie2.mp4", Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("a 404 subtitle playlist must not fail the download: %v", err)
	}
	final, _ := st.Get(j.ID)
	if final.Status != store.StatusCompleted {
		t.Fatalf("status = %q, want completed", final.Status)
	}
	if fi, err := os.Stat(final.Dest); err != nil || fi.Size() == 0 {
		t.Fatalf("video missing at %s: %v", final.Dest, err)
	}
}

// DASH: a single-file WebVTT AdaptationSet.
func TestDASHSubtitlesWrittenAsSidecar(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := buildAV(t, ffmpeg)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/manifest.mpd":
			w.Header().Set("Content-Type", "application/dash+xml")
			fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT2S">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="800000" width="160" height="120">
        <BaseURL>%s/v0.ts</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="text/vtt" contentType="text" lang="en">
      <Representation id="s" bandwidth="1000">
        <BaseURL>%s/whole.vtt</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, srv.URL, srv.URL)
		case "/whole.vtt":
			fmt.Fprint(w, "WEBVTT\n\n00:00:00.500 --> 00:00:01.500\nsingle file\n")
		default:
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		}
	}))
	defer srv.Close()

	dl := t.TempDir()
	m, st := newStreamMgr(t, ffmpeg, dl)
	j := &store.Job{
		ID: "dashsubs", Kind: store.KindDASH, URL: srv.URL + "/media/manifest.mpd",
		Dest: filepath.Join(dl, "dashmovie.mp4"), Filename: "dashmovie.mp4",
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
	final, _ := st.Get(j.ID)
	stem := strings.TrimSuffix(final.Dest, filepath.Ext(final.Dest))
	// AdaptationSet@lang="en" must reach the filename, as it does for HLS.
	body, err := os.ReadFile(stem + ".en.vtt")
	if err != nil {
		got, _ := filepath.Glob(stem + "*")
		t.Fatalf("no DASH subtitle sidecar at %s.en.vtt (found: %v): %v", stem, got, err)
	}
	if !strings.Contains(string(body), "single file") {
		t.Fatalf("sidecar content wrong:\n%s", body)
	}
}
