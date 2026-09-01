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
	"path/filepath"
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
	defer st.Close()
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

// A speed limit is worth having mainly for a download already in flight, so
// setting it must reach the running manager and not just the config file.
func TestSpeedLimitAppliesToTheRunningManager(t *testing.T) {
	// config.Save writes under the support dir, which is derived from $HOME.
	// Redirect it so the test never touches the real settings file.
	t.Setenv("HOME", t.TempDir())

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := manager.New(manager.Config{DownloadDir: t.TempDir()}, st)
	defer mgr.Shutdown(time.Second)
	srv := httptest.NewServer(api.New(mgr))
	defer srv.Close()

	if got := mgr.SpeedLimit(); got != 0 {
		t.Fatalf("a fresh manager starts unlimited, got %d", got)
	}

	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/config", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := post(`{"speed_limit_bps": 262144}`); code != http.StatusNoContent {
		t.Fatalf("POST /api/config = %d, want 204", code)
	}
	if got := mgr.SpeedLimit(); got != 262144 {
		t.Errorf("manager ceiling = %d, want 262144 — the setting never reached the running downloads", got)
	}

	// Zero lifts it again.
	if code := post(`{"speed_limit_bps": 0}`); code != http.StatusNoContent {
		t.Fatalf("POST /api/config = %d, want 204", code)
	}
	if got := mgr.SpeedLimit(); got != 0 {
		t.Errorf("manager ceiling = %d after setting 0, want unlimited", got)
	}

	// A negative ceiling is a mistake, not "unlimited by another name".
	if code := post(`{"speed_limit_bps": -1}`); code != http.StatusBadRequest {
		t.Errorf("a negative limit returned %d, want 400", code)
	}

	// An unrelated patch must not disturb the ceiling.
	if code := post(`{"speed_limit_bps": 131072}`); code != http.StatusNoContent {
		t.Fatal("setup patch failed")
	}
	if code := post(`{"audio_lang": "id"}`); code != http.StatusNoContent {
		t.Fatal("unrelated patch failed")
	}
	if got := mgr.SpeedLimit(); got != 131072 {
		t.Errorf("ceiling = %d after an unrelated patch, want it left at 131072", got)
	}
}

// The schedule must reach the running daemon, and a typo must never become a
// window that silently blocks every download.
func TestSchedulePatchAppliesAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := manager.New(manager.Config{DownloadDir: t.TempDir()}, st)
	defer mgr.Shutdown(time.Second)
	srv := httptest.NewServer(api.New(mgr))
	defer srv.Close()

	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/config", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if w := mgr.Schedule(); w.Enabled {
		t.Fatal("a fresh manager has no schedule")
	}

	code := post(`{"schedule_enabled": true, "schedule_start": "02:00", "schedule_stop": "06:00", "schedule_days": [1,2,3,4,5]}`)
	if code != http.StatusNoContent {
		t.Fatalf("POST /api/config = %d, want 204", code)
	}
	w := mgr.Schedule()
	if !w.Enabled || w.Start != 120 || w.Stop != 360 {
		t.Fatalf("window = %+v, want 02:00-06:00 enabled", w)
	}
	if w.Days[int(time.Saturday)] || !w.Days[int(time.Wednesday)] {
		t.Errorf("weekday selection wrong: %s", w)
	}

	// Bad input is refused, and leaves the working window in place.
	for _, bad := range []string{
		`{"schedule_start": "25:00"}`,
		`{"schedule_stop": "abc"}`,
		`{"schedule_start": "0200"}`,
		`{"schedule_days": [9]}`,
		`{"schedule_days": [-1]}`,
	} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", bad, code)
		}
	}
	if got := mgr.Schedule(); got.Start != 120 || got.Stop != 360 || !got.Enabled {
		t.Errorf("a rejected patch changed the live window: %s", got)
	}

	// Switching it off releases the daemon immediately.
	if code := post(`{"schedule_enabled": false}`); code != http.StatusNoContent {
		t.Fatalf("disabling returned %d", code)
	}
	if mgr.Schedule().Enabled {
		t.Error("schedule still enabled after being switched off")
	}
}

// Switching the scheduler on without usable times used to report success and do
// nothing — the window silently disabled itself. Say so instead.
func TestEnablingScheduleWithoutTimesIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := manager.New(manager.Config{DownloadDir: t.TempDir()}, st)
	defer mgr.Shutdown(time.Second)
	srv := httptest.NewServer(api.New(mgr))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/config", "application/json",
		strings.NewReader(`{"schedule_enabled": true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("enabling with no times returned %d, want 400", resp.StatusCode)
	}
	if mgr.Schedule().Enabled {
		t.Error("a refused patch must not enable the schedule")
	}

	// With times, the same call succeeds.
	resp2, err := http.Post(srv.URL+"/api/config", "application/json",
		strings.NewReader(`{"schedule_enabled": true, "schedule_start": "02:00", "schedule_stop": "06:00"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	io.Copy(io.Discard, resp2.Body)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("a complete schedule returned %d, want 204", resp2.StatusCode)
	}
	if !mgr.Schedule().Enabled {
		t.Error("schedule should be enabled")
	}
}

// The download folder is the one setting that could not reach the daemon at
// all: the window kept it in its own defaults and used it only to prefill the
// New Download dialog, so anything accepted without the dialog still landed in
// the built-in folder and the setting looked ignored.
func TestDownloadDirCanBeChangedAndIsValidated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	original := t.TempDir()
	mgr := manager.New(manager.Config{DownloadDir: original}, st)
	defer mgr.Shutdown(time.Second)
	srv := httptest.NewServer(api.New(mgr))
	defer srv.Close()

	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/config", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := mgr.DownloadDir(); got != original {
		t.Fatalf("DownloadDir = %q, want %q", got, original)
	}

	elsewhere := filepath.Join(t.TempDir(), "External Drive")
	if code := post(`{"download_dir": ` + strconv.Quote(elsewhere) + `}`); code != http.StatusNoContent {
		t.Fatalf("changing the folder returned %d, want 204", code)
	}
	if got := mgr.DownloadDir(); got != elsewhere {
		t.Errorf("DownloadDir = %q, want %q", got, elsewhere)
	}
	// It is created if it does not exist yet, the way picking a fresh folder
	// on a drive would need.
	if fi, err := os.Stat(elsewhere); err != nil || !fi.IsDir() {
		t.Errorf("the chosen folder was not created: %v", err)
	}

	// A folder that cannot be written to is refused while the user is still
	// looking at the picker, rather than failing when a download finishes.
	readonly := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readonly, 0o700) })
	blocked := filepath.Join(readonly, "sub")
	if code := post(`{"download_dir": ` + strconv.Quote(blocked) + `}`); code != http.StatusBadRequest {
		t.Errorf("an unusable folder returned %d, want 400", code)
	}
	if got := mgr.DownloadDir(); got != elsewhere {
		t.Errorf("a refused change moved the folder to %q", got)
	}

	// And an empty value is refused rather than silently meaning "root".
	if code := post(`{"download_dir": ""}`); code != http.StatusBadRequest {
		t.Errorf("an empty folder returned %d, want 400", code)
	}
}
