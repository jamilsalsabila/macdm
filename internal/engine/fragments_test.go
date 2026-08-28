package engine

import (
	"context"
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
