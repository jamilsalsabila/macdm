package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// serveBlob is a minimal Range-capable static handler. If dropAt > 0, the first
// response across the whole server writes only dropAt bytes of its slice and
// then hijacks-closes, simulating a mid-download connection failure exactly once.
func serveBlob(body []byte, ranges bool, dropAt int64) http.Handler {
	var dropped atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), int64(len(body)-1)
		code := http.StatusOK
		if ranges {
			w.Header().Set("Accept-Ranges", "bytes")
			if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
				parts := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
				start, _ = strconv.ParseInt(parts[0], 10, 64)
				if parts[1] != "" {
					end, _ = strconv.ParseInt(parts[1], 10, 64)
				}
				code = http.StatusPartialContent
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			}
		}
		seg := body[start : end+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
		w.WriteHeader(code)

		if dropAt > 0 && !dropped.Load() {
			dropped.Store(true)
			n := dropAt
			if n > int64(len(seg)) {
				n = int64(len(seg))
			}
			_, _ = w.Write(seg[:n])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		_, _ = w.Write(seg)
	})
}

func TestMultiConnectionDownload(t *testing.T) {
	body := make([]byte, 5<<20)
	rand.New(rand.NewSource(1)).Read(body)
	srv := httptest.NewServer(serveBlob(body, true, 0))
	defer srv.Close()

	e := New(Config{MaxConns: 6, MinChunk: 256 << 10, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "blob.bin")

	pr, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !pr.AcceptRanges || pr.TotalBytes != int64(len(body)) {
		t.Fatalf("probe wrong: %+v", pr)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch: got %d of %d bytes", len(got), len(body))
	}
	if _, err := os.Stat(dest + ".macdm"); !os.IsNotExist(err) {
		t.Fatal("sidecar should be gone after completion")
	}
}

func TestResumeAfterDrop(t *testing.T) {
	body := make([]byte, 3<<20)
	rand.New(rand.NewSource(2)).Read(body)
	srv := httptest.NewServer(serveBlob(body, true, 300<<10))
	defer srv.Close()

	e := New(Config{MaxConns: 4, MinChunk: 512 << 10, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "blob.bin")
	spec := DownloadSpec{URL: srv.URL, Dest: dest}

	if _, err := e.Run(context.Background(), spec, nil); err != nil {
		t.Fatalf("Run (retry should have covered the drop): %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch after resume: %d of %d bytes", len(got), len(body))
	}
}

func TestPauseThenResume(t *testing.T) {
	body := make([]byte, 8<<20)
	rand.New(rand.NewSource(3)).Read(body)
	// Throttle so the pause lands mid-download.
	srv := httptest.NewServer(throttle(serveBlob(body, true, 0), 2<<20))
	defer srv.Close()

	e := New(Config{MaxConns: 2, MinChunk: 1 << 20, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "blob.bin")
	spec := DownloadSpec{URL: srv.URL, Dest: dest}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel() }()
	_, err := e.Run(ctx, spec, nil)
	if err != context.Canceled {
		t.Fatalf("want context.Canceled on pause, got %v", err)
	}
	sc := loadSidecar(dest)
	if sc == nil || sc.completedBytes() == 0 || sc.completedBytes() >= int64(len(body)) {
		t.Fatalf("sidecar should hold partial progress, got %v", sc)
	}

	if _, err := e.Run(context.Background(), spec, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch after pause/resume")
	}
}

func TestNewSidecarSplit(t *testing.T) {
	p := &Probe{TotalBytes: 10 << 20, AcceptRanges: true}
	sc := newSidecar("u", p, 4, 1<<20)
	if len(sc.Chunks) != 4 {
		t.Fatalf("want 4 chunks, got %d", len(sc.Chunks))
	}
	var total int64
	for i, c := range sc.Chunks {
		total += c.length()
		if i > 0 && c.Start != sc.Chunks[i-1].End+1 {
			t.Fatalf("chunk %d not contiguous", i)
		}
	}
	if total != p.TotalBytes {
		t.Fatalf("chunks cover %d, want %d", total, p.TotalBytes)
	}
}

func TestNewSidecarNoRanges(t *testing.T) {
	sc := newSidecar("u", &Probe{TotalBytes: 999, AcceptRanges: false}, 8, 1<<20)
	if len(sc.Chunks) != 1 || sc.Chunks[0].End != -1 {
		t.Fatalf("no-range download should be one open-ended chunk, got %+v", sc.Chunks)
	}
}

func throttle(h http.Handler, bytesPerSec int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&slowWriter{ResponseWriter: w, rate: bytesPerSec}, r)
	})
}

type slowWriter struct {
	http.ResponseWriter
	rate int
}

func (s *slowWriter) Write(p []byte) (int, error) {
	const step = 64 << 10
	written := 0
	for written < len(p) {
		end := written + step
		if end > len(p) {
			end = len(p)
		}
		n, err := s.ResponseWriter.Write(p[written:end])
		written += n
		if f, ok := s.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		if err != nil {
			return written, err
		}
		time.Sleep(time.Duration(float64(step) / float64(s.rate) * float64(time.Second)))
	}
	return written, nil
}

func TestReplanConns(t *testing.T) {
	// 10 MiB file, initially 4 chunks, chunk 0 fully done, chunk 2 half done.
	total := int64(10 << 20)
	sc := newSidecar("u", &Probe{TotalBytes: total, AcceptRanges: true}, 4, 1<<20)
	if len(sc.Chunks) != 4 {
		t.Fatalf("want 4 chunks, got %d", len(sc.Chunks))
	}
	sc.Chunks[0].Done = sc.Chunks[0].length()
	sc.Chunks[2].Done = sc.Chunks[2].length() / 2
	doneBefore := sc.completedBytes()

	sc.replanConns(8, 1<<20)

	if sc.completedBytes() != doneBefore {
		t.Fatalf("completed bytes changed on replan: %d -> %d", doneBefore, sc.completedBytes())
	}
	// every sampled byte must be covered exactly once (Done ∪ Chunks == file)
	covered := map[int64]int{}
	mark := func(a, b int64) {
		start := ((a + 4095) / 4096) * 4096
		for x := start; x <= b; x += 4096 {
			covered[x]++
		}
	}
	for _, v := range sc.Done {
		mark(v.A, v.B)
	}
	for _, c := range sc.Chunks {
		mark(c.Start, c.End)
	}
	for b := int64(0); b < total; b += 4096 {
		if covered[b] != 1 {
			t.Fatalf("byte %d covered %d times after replan", b, covered[b])
		}
	}
	if sc.Conns != 8 {
		t.Fatalf("Conns not updated: %d", sc.Conns)
	}
}

func TestComplementIv(t *testing.T) {
	got := complementIv([]iv{{0, 99}, {200, 299}}, 500)
	want := []iv{{100, 199}, {300, 499}}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// Some CDNs (Instagram/Facebook) ignore the Range header and serve the whole
// body with 200. If that body covers the file, the download still succeeds.
func TestServerIgnoresRange(t *testing.T) {
	body := make([]byte, 2<<20)
	rand.New(rand.NewSource(7)).Read(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// advertise ranges (so the engine tries multi) but always send 200 full
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if r.Header.Get("Range") == "bytes=0-0" {
			// probe: honour it so TotalBytes is learned
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[:1])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	e := New(Config{MaxConns: 6, MinChunk: 128 << 10, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "ig.bin")
	if _, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch: %d of %d", len(got), len(body))
	}
}

// TestStalledConnectionRecovers: the server sends the first half then hangs.
// The chunk read must time out (not block forever) and the retry must finish.
func TestStalledConnectionRecovers(t *testing.T) {
	old := stallTimeout
	stallTimeout = 1 * time.Second
	defer func() { stallTimeout = old }()

	body := bytes.Repeat([]byte("Z"), 400<<10)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if n == 1 {
			// first attempt: send a bit, then hang past the stall timeout
			w.WriteHeader(200)
			w.Write(body[:20<<10])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(4 * time.Second)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	e := New(Config{MaxConns: 1, MinChunk: 1 << 20, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "out.bin")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Run(ctx, DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if len(got) != len(body) {
		t.Fatalf("size: got %d want %d", len(got), len(body))
	}
	if hits.Load() < 2 {
		t.Fatalf("expected a retry, got %d requests", hits.Load())
	}
}

// TestTruncatedRangeResponseRetries: a 206 that closes the connection cleanly
// before delivering its whole range must NOT be accepted as a finished chunk —
// otherwise a corrupt .part gets renamed to the final file. The retry must
// resume from where it stopped and produce the exact bytes.
func TestTruncatedRangeResponseRetries(t *testing.T) {
	body := make([]byte, 2<<20)
	rand.New(rand.NewSource(11)).Read(body)
	var full atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rng := r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || rng == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			w.WriteHeader(200)
			w.Write(body)
			return
		}
		seg := body[start : end+1]
		// First delivery of any real range: hand back only half, then stop.
		short := len(seg)
		if start != end && full.Add(1) <= 3 {
			short = len(seg) / 2
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", short))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(seg[:short])
	}))
	defer srv.Close()

	e := New(Config{MaxConns: 3, MinChunk: 256 << 10, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "archive.zip")
	if _, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch after truncated-range retry: %d of %d bytes", len(got), len(body))
	}
}

// TestFlakyHostManyDrops: the server kills every connection after ~12KB, so a
// 400KB chunk needs ~35 resume attempts — far past the old fixed cap of 5.
// Because each attempt still makes progress the no-progress budget resets and
// the download completes.
func TestFlakyHostManyDrops(t *testing.T) {
	body := make([]byte, 400<<10)
	rand.New(rand.NewSource(99)).Read(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Accept-Ranges", "bytes")
		start := int64(0)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.ParseInt(p[0], 10, 64)
		}
		seg := body[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
		w.WriteHeader(http.StatusPartialContent)
		n := 12 << 10
		if n > len(seg) {
			n = len(seg)
		}
		w.Write(seg[:n])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if n < len(seg) {
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
		}
	}))
	defer srv.Close()

	e := New(Config{MaxConns: 1, MinChunk: 1 << 20, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "flaky.bin")
	if _, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch: %d of %d bytes", len(got), len(body))
	}
	if hits.Load() < 10 {
		t.Fatalf("expected many resume attempts, got %d", hits.Load())
	}
}

// TestWorkStealing: the first 2 MiB of the file is served at ~512 KiB/s *per
// connection*; the rest is instant. With a fixed chunk-per-connection split the
// download is gated by that one slow 2 MiB chunk (~4s). Work-stealing lets the
// finished workers pile onto the slow range so it completes in ~1s.
func TestWorkStealing(t *testing.T) {
	const total = 8 << 20
	const slow = 2 << 20
	body := make([]byte, total)
	rand.New(rand.NewSource(5)).Read(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := int64(0), int64(total-1)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.ParseInt(p[0], 10, 64)
			if p[1] != "" {
				end, _ = strconv.ParseInt(p[1], 10, 64)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
		}
		seg := body[start : end+1]
		fl, _ := w.(http.Flusher)
		if start < slow {
			for off := 0; off < len(seg); off += 32 << 10 {
				e := off + 32<<10
				if e > len(seg) {
					e = len(seg)
				}
				w.Write(seg[off:e])
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(60 * time.Millisecond) // ~512 KiB/s per connection
			}
			return
		}
		w.Write(seg)
	}))
	defer srv.Close()

	e := New(Config{MaxConns: 4, MinChunk: 256 << 10, Timeout: 30 * time.Second})
	dest := filepath.Join(t.TempDir(), "big.bin")
	t0 := time.Now()
	if _, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(t0)
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch: %d of %d bytes", len(got), len(body))
	}
	t.Logf("work-stealing download took %v", elapsed)
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("work-stealing did not kick in: took %v (expected < 2.5s)", elapsed)
	}
}

// TestWorkStealingOnResume: resuming with only ONE chunk left is exactly the
// long-tail case stealing exists to fix. Regression guard for a worker-count
// cap that used to start a single worker here, disabling stealing entirely.
func TestWorkStealingOnResume(t *testing.T) {
	const total = 8 << 20
	body := make([]byte, total)
	rand.New(rand.NewSource(17)).Read(body)

	var inflight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inflight.Add(-1)
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := int64(0), int64(total-1)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.ParseInt(p[0], 10, 64)
			if p[1] != "" {
				end, _ = strconv.ParseInt(p[1], 10, 64)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
		}
		seg := body[start : end+1]
		fl, _ := w.(http.Flusher)
		for off := 0; off < len(seg); off += 32 << 10 {
			e := off + 32<<10
			if e > len(seg) {
				e = len(seg)
			}
			w.Write(seg[off:e])
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(30 * time.Millisecond) // slow, per connection
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "resume.bin")
	f, err := os.Create(dest + ".part")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(total); err != nil {
		t.Fatal(err)
	}
	// 8 chunks, the first 7 already finished — only the tail remains. Their real
	// bytes must already be in the .part, or the final checksum can't match.
	sc := newSidecar(srv.URL, &Probe{TotalBytes: total, AcceptRanges: true}, 8, 256<<10)
	for i := 0; i < len(sc.Chunks)-1; i++ {
		c := &sc.Chunks[i]
		if _, err := f.WriteAt(body[c.Start:c.End+1], c.Start); err != nil {
			t.Fatal(err)
		}
		c.Done = c.length()
	}
	f.Close()
	if err := sc.save(dest); err != nil {
		t.Fatal(err)
	}

	e := New(Config{MaxConns: 8, MinChunk: 256 << 10, Timeout: 30 * time.Second})
	if _, err := e.Run(context.Background(), DownloadSpec{URL: srv.URL, Dest: dest}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(body) {
		t.Fatalf("content mismatch: %d of %d bytes", len(got), len(body))
	}
	if peak.Load() < 2 {
		t.Fatalf("work-stealing never engaged on resume: peak %d concurrent request(s)", peak.Load())
	}
}
