package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macdm/internal/engine"
	"macdm/internal/mux"
	"macdm/internal/store"
	"macdm/internal/tools"
)

func ffmpegPath(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	cands := []string{filepath.Join(home, "Library", "Application Support", "MacDM", "bin", "ffmpeg")}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		cands = append(cands, p)
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.Mode()&0o111 != 0 {
			return c
		}
	}
	t.Skip("ffmpeg not available")
	return ""
}

func run(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.CommandContext(ctx, ffmpeg, full...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg %v failed: %v: %s", args, err, out)
	}
}

// A master playlist whose variant is video-only, with the audio in a separate
// #EXT-X-MEDIA rendition. Downloading only the variant used to yield a silent
// file; the assembled result must carry both tracks.
func TestHLSAlternativeAudioIsMuxedIn(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := t.TempDir()

	// Source clip, then split into video-only and audio-only TS segments.
	clip := filepath.Join(media, "clip.mp4")
	run(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", clip)
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-an", "-f", "mpegts", filepath.Join(media, "v0.ts"))
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-vn", "-f", "mpegts", filepath.Join(media, "a0.ts"))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="%s/audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=160x120,CODECS="avc1.42c00c,mp4a.40.2",AUDIO="aud"
%s/video.m3u8
`, srv.URL, srv.URL)
		case "/video.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/v0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		case "/audio.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/a0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		default:
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		}
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
		ID: "hlsaudio", Kind: store.KindHLS, URL: srv.URL + "/master.m3u8",
		Dest: filepath.Join(dl, "out.mp4"), Filename: "out.mp4", Status: store.StatusQueued,
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
	fi, err := os.Stat(final.Dest)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no output file at %s: %v", final.Dest, err)
	}

	streams := mux.New(ffmpeg).Probe(ctx, final.Dest)
	if !strings.Contains(streams, "Video:") {
		t.Errorf("output has no video track:\n%s", streams)
	}
	if !strings.Contains(streams, "Audio:") {
		t.Errorf("alternative audio rendition was not muxed in — this is the silent-download bug:\n%s", streams)
	}
}

// The two tracks name their segments identically (seg-000000), so they must not
// share a scratch directory or they overwrite each other.
func TestHLSAudioUsesSeparateScratchDir(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	media := t.TempDir()
	clip := filepath.Join(media, "clip.mp4")
	run(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", clip)
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-an", "-f", "mpegts", filepath.Join(media, "v0.ts"))
	run(t, ffmpeg, "-i", clip, "-c", "copy", "-vn", "-f", "mpegts", filepath.Join(media, "a0.ts"))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,URI="%s/audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,AUDIO="aud"
%s/video.m3u8
`, srv.URL, srv.URL)
		case "/video.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/v0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		case "/audio.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/a0.ts\n#EXT-X-ENDLIST\n", srv.URL)
		default:
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		}
	}))
	defer srv.Close()

	dl := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	m := New(Config{
		DownloadDir: dl, WorkDir: work, MaxActive: 2,
		Tools:  tools.Set{Ffmpeg: ffmpeg},
		Engine: engine.Config{MaxConns: 2, MinChunk: 1 << 20},
	}, st)

	j := &store.Job{
		ID: "scratch", Kind: store.KindHLS, URL: srv.URL + "/master.m3u8",
		Dest: filepath.Join(dl, "o.mp4"), Filename: "o.mp4", Status: store.StatusQueued,
	}
	if err := st.Put(j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := m.execStream(ctx, j.ID, j); err != nil {
		t.Fatalf("execStream: %v", err)
	}
	// Read the real destination back: execStream renames a generic-looking
	// filename, so assuming the path here would test nothing.
	final, err := st.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final.Dest); err != nil {
		t.Fatalf("no output at %s: %v", final.Dest, err)
	}
	// Both tracks name their segments seg-000000. Sharing one scratch directory
	// would have the audio overwrite the video before the mux ran.
	streams := mux.New(ffmpeg).Probe(ctx, final.Dest)
	if !strings.Contains(streams, "Audio:") || !strings.Contains(streams, "Video:") {
		t.Fatalf("tracks collided in the scratch dir:\n%s", streams)
	}
}
