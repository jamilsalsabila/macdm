package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"macdm/internal/api"
	"macdm/internal/engine"
	"macdm/internal/manager"
	"macdm/internal/store"
)

func fileServer(size int) *httptest.Server {
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := 0, len(body)-1
		code := 200
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.Atoi(p[0])
			if p[1] != "" {
				end, _ = strconv.Atoi(p[1])
			}
			code = 206
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(code)
		w.Write(body[start : end+1])
	}))
}

// TestAPINoDeadlock exercises the control surface concurrently — the goal is to
// prove no mutex/channel deadlock (Reject once held hub.mu across broadcast),
// not to load-test. Kept modest so it stays fast and reliable.
func TestAPINoDeadlock(t *testing.T) {
	fs := fileServer(1 << 20)
	defer fs.Close()

	dlDir, _ := os.MkdirTemp("", "macdm-dl")
	defer os.RemoveAll(dlDir) // best-effort; background writers may linger briefly

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(manager.Config{
		DownloadDir: dlDir, MaxActive: 3, PromptTimeoutSec: 300,
		Engine: engine.Config{MaxConns: 4, MinChunk: 64 << 10, Timeout: 5 * time.Second},
	}, st)
	srv := httptest.NewServer(api.New(mgr))
	defer func() {
		mgr.Shutdown(2 * time.Second) // stop download goroutines before TempDir cleanup
		srv.CloseClientConnections()
		srv.Close()
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, body string) string {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	idOf := func(s string) string {
		if i := strings.Index(s, `"id":"`); i >= 0 && len(s) >= i+22 {
			return s[i+6 : i+22]
		}
		return ""
	}

	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	go func() {
		req, _ := http.NewRequestWithContext(sseCtx, "GET", srv.URL+"/api/events", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond)

	var wg sync.WaitGroup

	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := idOf(call("POST", "/api/jobs", `{"url":"`+fs.URL+`","conns":4}`))
			for k := 0; k < 15; k++ {
				call("POST", "/api/jobs/"+id+"/pause", "")
				call("POST", "/api/jobs/"+id+"/conns", `{"conns":`+strconv.Itoa(2+k%4)+`}`)
				call("POST", "/api/jobs/"+id+"/resume", "")
				call("GET", "/api/jobs", "")
				time.Sleep(3 * time.Millisecond)
			}
		}()
	}

	for g := 0; g < 4; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 12; k++ {
				pid := idOf(call("POST", "/api/proposals",
					`{"url":"`+fs.URL+`/p`+strconv.Itoa(g)+`_`+strconv.Itoa(k)+`"}`))
				if k%2 == 0 {
					call("POST", "/api/proposals/"+pid+"/reject", "")
				} else {
					call("POST", "/api/proposals/"+pid+"/accept", `{"conns":2}`)
				}
				call("GET", "/api/proposals", "")
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("hung — likely a deadlock in the manager/hub locking")
	}

	if got := call("GET", "/api/health", ""); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("daemon unresponsive after churn: %q", got)
	}
}
