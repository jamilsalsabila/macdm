package engine

import (
	"context"
	"errors"
	"macdm/internal/diskspace"
	"macdm/internal/ratelimit"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunFragments serves one logical file as many byte-range slices, each at its
// own URL (Instagram/Facebook style: the range lives in the query string and the
// server answers 200, ignoring any Range header), and checks reassembly.
func TestRunFragments(t *testing.T) {
	full := make([]byte, 50000)
	for i := range full {
		full[i] = byte(i*7 + 3)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := strconv.Atoi(r.URL.Query().Get("bytestart"))
		be, _ := strconv.Atoi(r.URL.Query().Get("byteend"))
		if bs < 0 || be >= len(full) || bs > be {
			http.Error(w, "range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK) // deliberately not 206
		_, _ = w.Write(full[bs : be+1])
	}))
	defer srv.Close()

	const step = 4096
	var frags []Fragment
	for start := 0; start < len(full); start += step {
		end := start + step - 1
		if end >= len(full) {
			end = len(full) - 1
		}
		u := srv.URL + "/v.mp4?bytestart=" + strconv.Itoa(start) + "&byteend=" + strconv.Itoa(end)
		frags = append(frags, Fragment{URL: u, Start: int64(start), End: int64(end)})
	}

	e := New(Config{MaxConns: 4, Timeout: 10 * time.Second})
	dest := filepath.Join(t.TempDir(), "out.mp4")

	if err := e.RunFragments(context.Background(), dest, nil, frags, 4, nil); err != nil {
		t.Fatalf("RunFragments: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(full) {
		t.Fatalf("size: got %d want %d", len(got), len(full))
	}
	for i := range full {
		if got[i] != full[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	if _, err := os.Stat(dest + ".macdmf"); !os.IsNotExist(err) {
		t.Fatalf("sidecar not cleaned up")
	}
}

// TestRunFragmentsResume drops the connection for the tail fragments on the first
// run, then re-runs with the failure disabled and expects the already-fetched
// fragments to be skipped and the file to complete.
func TestRunFragmentsResume(t *testing.T) {
	full := make([]byte, 20000)
	for i := range full {
		full[i] = byte(i)
	}
	const failFrom = 12000
	var failing atomic.Bool
	failing.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := strconv.Atoi(r.URL.Query().Get("bytestart"))
		be, _ := strconv.Atoi(r.URL.Query().Get("byteend"))
		if bs >= failFrom && failing.Load() {
			hj, ok := w.(http.Hijacker)
			if ok {
				c, _, _ := hj.Hijack()
				_ = c.Close() // abrupt drop
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full[bs : be+1])
	}))
	defer srv.Close()

	mk := func() []Fragment {
		var frags []Fragment
		const step = 2000
		for start := 0; start < len(full); start += step {
			end := start + step - 1
			pu, _ := url.Parse(srv.URL + "/v.mp4")
			q := pu.Query()
			q.Set("bytestart", strconv.Itoa(start))
			q.Set("byteend", strconv.Itoa(end))
			pu.RawQuery = q.Encode()
			frags = append(frags, Fragment{URL: pu.String(), Start: int64(start), End: int64(end)})
		}
		return frags
	}

	e := New(Config{MaxConns: 2, Timeout: 5 * time.Second})
	dest := filepath.Join(t.TempDir(), "out.bin")

	if err := e.RunFragments(context.Background(), dest, nil, mk(), 2, nil); err == nil {
		t.Fatalf("expected first run to fail")
	}
	failing.Store(false)
	if err := e.RunFragments(context.Background(), dest, nil, mk(), 2, nil); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if len(got) != len(full) {
		t.Fatalf("size: got %d want %d", len(got), len(full))
	}
	for i := range full {
		if got[i] != full[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

// TestRunFragmentsGap rejects a fragment set with a hole rather than writing an
// unplayable file.
func TestRunFragmentsGap(t *testing.T) {
	e := New(Config{MaxConns: 2, Timeout: 5 * time.Second})
	frags := []Fragment{
		{URL: "http://x/1", Start: 0, End: 999},
		{URL: "http://x/2", Start: 2000, End: 2999}, // gap 1000..1999
	}
	err := e.RunFragments(context.Background(), filepath.Join(t.TempDir(), "o"), nil, frags, 2, nil)
	if err == nil {
		t.Fatal("expected a gap error")
	}
}

// fragServer serves one logical file as Instagram-style byte-range slices and
// returns the fragment list describing it.
func fragServer(t *testing.T, size, step int) (*httptest.Server, []Fragment, []byte) {
	t.Helper()
	full := make([]byte, size)
	for i := range full {
		full[i] = byte(i*11 + 5)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := strconv.Atoi(r.URL.Query().Get("bytestart"))
		be, _ := strconv.Atoi(r.URL.Query().Get("byteend"))
		if bs < 0 || be >= len(full) || bs > be {
			http.Error(w, "range", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full[bs : be+1])
	}))
	t.Cleanup(srv.Close)
	var frags []Fragment
	for start := 0; start < size; start += step {
		end := start + step - 1
		if end >= size {
			end = size - 1
		}
		frags = append(frags, Fragment{
			URL:   srv.URL + "/v.mp4?bytestart=" + strconv.Itoa(start) + "&byteend=" + strconv.Itoa(end),
			Start: int64(start), End: int64(end),
		})
	}
	return srv, frags, full
}

// Fragments are a separate download loop from Run, so the disk-space guard had
// to be repeated here — without it an Instagram video larger than the disk
// started happily and died partway, which is the exact bug the guard exists to
// prevent everywhere else.
func TestRunFragmentsRefusesWhenTheDiskIsTooSmall(t *testing.T) {
	dir := t.TempDir()
	avail, err := diskspace.Avail(dir)
	if err != nil {
		t.Skip("cannot measure the volume:", err)
	}
	_, frags, _ := fragServer(t, 4096, 1024)
	// Claim a total far past the free space without serving any of it: the
	// refusal must come before a single fragment is fetched.
	frags[len(frags)-1].End = avail * 4

	e := New(Config{MaxConns: 4, Timeout: 10 * time.Second})
	dest := filepath.Join(dir, "huge.mp4")
	err = e.RunFragments(context.Background(), dest, nil, frags, 4, nil)
	if err == nil {
		t.Fatal("a fragmented download four times the size of the disk must be refused")
	}
	var de *diskspace.Error
	if !errors.As(err, &de) {
		t.Fatalf("want a *diskspace.Error, got %T: %v", err, err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("no .part should be left by a download that never started")
	}
}

// Likewise the speed limit: a ceiling wired only into Run would have quietly
// exempted every Instagram and Facebook video.
func TestRunFragmentsRespectsTheSpeedLimit(t *testing.T) {
	const size = 1 << 20
	_, frags, full := fragServer(t, size, 32<<10)

	run := func(t *testing.T, bps int64) time.Duration {
		t.Helper()
		e := New(Config{MaxConns: 6, Timeout: 10 * time.Second, Limiter: ratelimit.New(bps)})
		dest := filepath.Join(t.TempDir(), "out.mp4")
		start := time.Now()
		if err := e.RunFragments(context.Background(), dest, nil, frags, 6, nil); err != nil {
			t.Fatalf("RunFragments: %v", err)
		}
		el := time.Since(start)
		got, _ := os.ReadFile(dest)
		if sha(got) != sha(full) {
			t.Fatal("throttling must not corrupt the assembled file")
		}
		return el
	}

	limited := run(t, size) // 1 MB through a 1 MB/s ceiling
	if limited < 600*time.Millisecond {
		t.Errorf("1 MB through a 1 MB/s ceiling took %v — the limit never reached the fragment loop", limited)
	}
	unlimited := run(t, 0)
	if unlimited > limited/2 {
		t.Errorf("unlimited run took %v vs %v limited; the comparison proves nothing", unlimited, limited)
	}
}

// dribbleServer serves one fragment in `steps` pieces, pausing `gap` between
// them — a connection that is slow but never silent.
func dribbleServer(t *testing.T, size, steps int, gap time.Duration, stallAfter int) (*httptest.Server, []Fragment, []byte) {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 3)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := strconv.Atoi(r.URL.Query().Get("bytestart"))
		be, _ := strconv.Atoi(r.URL.Query().Get("byteend"))
		body := payload[bs : be+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		step := len(body)/steps + 1
		for i, sent := 0, 0; i < len(body); i, sent = i+step, sent+1 {
			if stallAfter > 0 && sent >= stallAfter {
				<-r.Context().Done() // go quiet and stay quiet
				return
			}
			end := i + step
			if end > len(body) {
				end = len(body)
			}
			if _, err := w.Write(body[i:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(gap)
		}
	}))
	t.Cleanup(srv.Close)
	frags := []Fragment{{
		URL:   srv.URL + "/f?bytestart=0&byteend=" + strconv.Itoa(size-1),
		Start: 0, End: int64(size - 1),
	}}
	return srv, frags, payload
}

// The bug: the fetch had an absolute 90-second deadline, on the assumption that
// a fragment is only ever a few hundred KB. On a slow line that assumption
// breaks — a fragment needing longer was cut off at the same point on every one
// of its three attempts, and the assembly failed, even though the connection
// was healthy and delivering steadily throughout.
func TestSlowButSteadyFragmentIsNotCutOff(t *testing.T) {
	// 20 pieces, 300ms apart: 6 seconds of steady delivery, far longer than the
	// stall window, and never once silent for it.
	_, frags, want := dribbleServer(t, 200<<10, 20, 300*time.Millisecond, 0)

	e := New(Config{MaxConns: 1, Timeout: 30 * time.Second, FragmentStall: 2 * time.Second})
	dest := filepath.Join(t.TempDir(), "out.bin")
	start := time.Now()
	if err := e.RunFragments(context.Background(), dest, nil, frags, 1, nil); err != nil {
		t.Fatalf("a fragment that is merely slow must still finish: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if sha(got) != sha(want) {
		t.Fatal("content mismatch")
	}
	if el := time.Since(start); el < 4*time.Second {
		t.Errorf("finished in %v — the server cannot have dribbled as intended", el)
	}
}

// The other half: a connection that actually goes quiet must still be given up
// on, or one dead fragment would hang the whole assembly forever.
func TestStalledFragmentIsAbandoned(t *testing.T) {
	// Sends 3 pieces then goes silent for good.
	_, frags, _ := dribbleServer(t, 200<<10, 20, 100*time.Millisecond, 3)

	e := New(Config{MaxConns: 1, Timeout: 30 * time.Second, FragmentStall: 2 * time.Second})
	dest := filepath.Join(t.TempDir(), "out.bin")
	start := time.Now()
	err := e.RunFragments(context.Background(), dest, nil, frags, 1, nil)
	el := time.Since(start)
	if err == nil {
		t.Fatal("a fragment whose connection went silent must be given up on")
	}
	if el > 30*time.Second {
		t.Errorf("took %v to give up across 3 attempts", el)
	}
}
