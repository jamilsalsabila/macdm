package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// serve points the CLI at a fake daemon via MACDM_ADDR and returns the
// requests it receives.
type recorded struct {
	method string
	path   string
	body   map[string]any
}

func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *[]recorded {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.body)
		}
		got = append(got, rec)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MACDM_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	return &got
}

func okJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func TestBaseUsesEnvOverride(t *testing.T) {
	t.Setenv("MACDM_ADDR", "127.0.0.1:9999")
	if got := base(); got != "http://127.0.0.1:9999" {
		t.Fatalf("base() = %q", got)
	}
}

// A flag as the final argument used to run off the end of args and panic.
func TestCmdAddRejectsDanglingFlags(t *testing.T) {
	serve(t, okJSON(`{}`))
	for _, flag := range []string{"-o", "-n", "-H"} {
		err := cmdAdd([]string{"https://example.com/f.bin", flag})
		if err == nil {
			t.Errorf("%s with no value: want an error, got nil", flag)
			continue
		}
		if !strings.Contains(err.Error(), "needs a value") {
			t.Errorf("%s: unhelpful error %q", flag, err)
		}
	}
}

func TestCmdAddValidatesConns(t *testing.T) {
	serve(t, okJSON(`{}`))
	for _, v := range []string{"abc", "0", "-3", "999"} {
		if err := cmdAdd([]string{"https://example.com/f.bin", "-n", v}); err == nil {
			t.Errorf("-n %q should be rejected", v)
		}
	}
}

func TestCmdAddBuildsRequest(t *testing.T) {
	got := serve(t, okJSON(`{"id":"abc123","filename":"out.bin","status":"queued"}`))
	err := cmdAdd([]string{
		"-o", "out.bin",
		"-n", "6",
		"-H", "Referer: https://example.com/page",
		"-H", "Cookie: a=b",
		"https://example.com/f.bin",
	})
	if err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("want 1 request, got %d", len(*got))
	}
	r := (*got)[0]
	if r.method != "POST" || r.path != "/api/jobs" {
		t.Fatalf("got %s %s", r.method, r.path)
	}
	if r.body["url"] != "https://example.com/f.bin" || r.body["filename"] != "out.bin" {
		t.Fatalf("bad body: %v", r.body)
	}
	if n, _ := r.body["conns"].(float64); n != 6 {
		t.Fatalf("conns = %v, want 6", r.body["conns"])
	}
	h, _ := r.body["headers"].(map[string]any)
	if h["Referer"] != "https://example.com/page" || h["Cookie"] != "a=b" {
		t.Fatalf("headers not parsed/trimmed: %v", h)
	}
}

func TestCmdAddRequiresURL(t *testing.T) {
	serve(t, okJSON(`{}`))
	if err := cmdAdd([]string{"-n", "4"}); err == nil || !strings.Contains(err.Error(), "missing URL") {
		t.Fatalf("want a missing-URL error, got %v", err)
	}
}

// The daemon reports failures as {"error": "..."}; the CLI must surface that
// text rather than a bare status line.
func TestDoJSONSurfacesServerError(t *testing.T) {
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"job not found"}`)
	})
	err := doJSON("POST", "/api/jobs/zzz/pause", nil, nil)
	if err == nil || err.Error() != "job not found" {
		t.Fatalf("got %v, want the server's message", err)
	}
}

func TestDoJSONFallsBackToStatus(t *testing.T) {
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	})
	err := doJSON("GET", "/api/jobs", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v, want status + body", err)
	}
}

// With nothing listening the hint must point at `macdm daemon`.
func TestDoJSONUnreachableHint(t *testing.T) {
	t.Setenv("MACDM_ADDR", "127.0.0.1:1") // nothing listens here
	err := doJSON("GET", "/api/jobs", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "macdm daemon") {
		t.Fatalf("got %v, want the daemon hint", err)
	}
}

func TestCmdSimpleAndRemoveRoutes(t *testing.T) {
	got := serve(t, okJSON(``))
	if err := cmdSimple("pause", []string{"job1"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdRemove([]string{"job2"}); err != nil {
		t.Fatal(err)
	}
	if (*got)[0].path != "/api/jobs/job1/pause" || (*got)[0].method != "POST" {
		t.Errorf("pause hit %s %s", (*got)[0].method, (*got)[0].path)
	}
	if (*got)[1].path != "/api/jobs/job2" || (*got)[1].method != "DELETE" {
		t.Errorf("rm hit %s %s", (*got)[1].method, (*got)[1].path)
	}
}

func TestCmdSimpleAndRemoveNeedExactlyOneID(t *testing.T) {
	serve(t, okJSON(``))
	for _, args := range [][]string{{}, {"a", "b"}} {
		if err := cmdSimple("pause", args); err == nil {
			t.Errorf("pause %v should be rejected", args)
		}
		if err := cmdRemove(args); err == nil {
			t.Errorf("rm %v should be rejected", args)
		}
	}
}

func TestCmdListHandlesEmptyAndPopulated(t *testing.T) {
	serve(t, okJSON(`[]`))
	if err := cmdList(); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	serve(t, okJSON(`[{"id":"a","filename":"f.bin","status":"downloading","total_bytes":100,"done_bytes":25,"speed_bps":2048}]`))
	if err := cmdList(); err != nil {
		t.Fatalf("populated list: %v", err)
	}
}

func TestDaemonAlive(t *testing.T) {
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if !daemonAlive(2e9) {
		t.Error("healthy daemon reported dead")
	}
	t.Setenv("MACDM_ADDR", "127.0.0.1:1")
	if daemonAlive(2e8) {
		t.Error("nothing is listening, yet reported alive")
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		total, done int64
		want        float64
	}{
		{0, 0, 0},   // size unknown — must not divide by zero
		{-1, 50, 0}, // nonsense total
		{100, 0, 0},
		{100, 25, 25},
		{100, 100, 100},
	}
	for _, c := range cases {
		j := job{TotalBytes: c.total, DoneBytes: c.done}
		if got := j.percent(); got != c.want {
			t.Errorf("percent(total=%d done=%d) = %v, want %v", c.total, c.done, got, c.want)
		}
	}
}

func TestHumanSpeed(t *testing.T) {
	cases := map[int64]string{
		0:          "-",
		-5:         "-",
		512:        "512.0 B/s",
		1024:       "1.0 KB/s",
		1536:       "1.5 KB/s",
		1048576:    "1.0 MB/s",
		1073741824: "1.0 GB/s",
	}
	for in, want := range cases {
		if got := humanSpeed(in); got != want {
			t.Errorf("humanSpeed(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFindDaemonBinaryPrefersSibling(t *testing.T) {
	// Not found is a clear error, never a panic or an empty path.
	t.Setenv("PATH", t.TempDir())
	p, err := findDaemonBinary()
	if err == nil && p == "" {
		t.Fatal("returned an empty path with no error")
	}
	if err != nil && !strings.Contains(err.Error(), "macdmd") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestUsageWritesSomething(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	usage()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "macdm") {
		t.Fatalf("usage() printed %q", out)
	}
}
