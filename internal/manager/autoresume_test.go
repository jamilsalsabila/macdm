package manager

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"macdm/internal/engine"
	"macdm/internal/store"
)

// TestAutoResumeAfterTransientFailures: the host 503s the first two whole-job
// attempts, then serves the file. The manager should retry on its own and the
// job should finish "completed", not "error".
func TestAutoResumeAfterTransientFailures(t *testing.T) {
	old := autoResumeBackoff
	autoResumeBackoff = 150 * time.Millisecond
	defer func() { autoResumeBackoff = old }()

	body := strings.Repeat("payload-", 4096) // 32 KiB
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			http.Error(w, "upstream hiccup", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		start := int64(0)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.ParseInt(p[0], 10, 64)
		}
		seg := body[start:]
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write([]byte(seg))
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	m := New(Config{
		DownloadDir: t.TempDir(), MaxActive: 2, PromptTimeoutSec: 300,
		Engine: engine.Config{MaxConns: 4, MinChunk: 1 << 20, Timeout: 5 * time.Second},
	}, st)

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			cur, _ := m.st.Get(j.ID)
			t.Fatalf("job never completed; status=%q err=%q attempts=%d", cur.Status, cur.Error, attempts.Load())
		case <-time.After(50 * time.Millisecond):
		}
		cur, e := m.st.Get(j.ID)
		if e != nil {
			continue
		}
		if cur.Status == store.StatusCompleted {
			if attempts.Load() < 3 {
				t.Fatalf("completed with only %d attempts — expected retries", attempts.Load())
			}
			return
		}
		if cur.Status == store.StatusError {
			t.Fatalf("job errored instead of auto-resuming: %q", cur.Error)
		}
	}
}

func TestTransientErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"read tcp 1.2.3.4:443: connection reset by peer", true},
		{"chunk 3: connection stalled", true},
		{"unexpected EOF", true},
		{"GET https://x/y: 503 Service Unavailable", true},
		{"unsupported URL: https://example.com/", false},
		{"blocked by the site (HTTP 403)", false},
		{"server returned HTTP 404", false},
		{"stream is DRM-protected", false},
		{"playlist has no segments", false},
	}
	for _, c := range cases {
		if got := transientErr(fmt.Errorf("%s", c.msg)); got != c.want {
			t.Errorf("transientErr(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
