package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// macdmd is all wiring inside main(), so it is exercised as a real process.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func daemonBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "macdmd-test")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "macdmd")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// safeBuf collects the child's stdout/stderr. os/exec writes to it from its own
// goroutines while the test reads it, so it needs a lock.
type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type daemon struct {
	cmd    *exec.Cmd
	log    *safeBuf
	addr   string
	exited chan struct{} // closed once Wait returns
	once   sync.Once
}

// start launches macdmd against an isolated HOME and waits for /api/health.
func start(t *testing.T, home, addr string) *daemon {
	t.Helper()
	cmd := exec.Command(daemonBinary(t), "-addr", addr)
	cmd.Env = append(os.Environ(), "HOME="+home)
	log := &safeBuf{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cmd: cmd, log: log, addr: addr, exited: make(chan struct{})}
	// A single owner of Wait: calling it twice, or mixing it with
	// Process.Wait, races os/exec's own bookkeeping.
	go func() { _ = cmd.Wait(); close(d.exited) }()
	t.Cleanup(d.stop)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if get(addr, "/api/health") != nil {
			return d
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon never became healthy; log:\n%s", log.String())
	return nil
}

// stop sends SIGTERM and waits — the shutdown path main() implements.
func (d *daemon) stop() {
	d.once.Do(func() {
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Signal(syscall.SIGTERM)
		}
	})
	select {
	case <-d.exited:
	case <-time.After(15 * time.Second):
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		<-d.exited
	}
}

func get(addr, path string) []byte {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestDaemonServesHealth(t *testing.T) {
	home := t.TempDir()
	addr := freePort(t)
	start(t, home, addr)

	var h struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(get(addr, "/api/health"), &h); err != nil {
		t.Fatal(err)
	}
	if !h.OK || h.Version == "" {
		t.Fatalf("bad health payload: %+v", h)
	}
}

func TestDaemonIsLoopbackOnly(t *testing.T) {
	home := t.TempDir()
	addr := freePort(t)
	start(t, home, addr)
	// Binding to an explicit 127.0.0.1 address is itself the guarantee; confirm
	// it is not also listening on a routable interface.
	host, port, _ := net.SplitHostPort(addr)
	if host != "127.0.0.1" {
		t.Skipf("test addr is not loopback: %s", addr)
	}
	if outbound := outboundIP(); outbound != "" {
		c := &http.Client{Timeout: time.Second}
		if resp, err := c.Get("http://" + net.JoinHostPort(outbound, port) + "/api/health"); err == nil {
			resp.Body.Close()
			t.Errorf("daemon is reachable on %s — it must be loopback only", outbound)
		}
	}
}

func outboundIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// Regression: macdmd used to resume every unfinished job before discovering its
// port was taken, racing the running daemon on the same .part files and
// jobs.json, then dying via log.Fatalf (which skips the store's final flush).
func TestSecondDaemonExitsBeforeTouchingAnything(t *testing.T) {
	home := t.TempDir()
	addr := freePort(t)
	start(t, home, addr)

	cmd := exec.Command(daemonBinary(t), "-addr", addr)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("second daemon on a taken port should have failed")
	}
	s := string(out)
	if !strings.Contains(s, "already running") {
		t.Errorf("error should name the cause, got: %s", s)
	}
	// "tools:" is logged immediately after the store opens and before
	// ResumeAll — seeing it means the second process got that far.
	if strings.Contains(s, "tools:") {
		t.Errorf("second daemon reached setup before failing:\n%s", s)
	}
}

// End-to-end cover for store.Close()'s blocking final flush: a job added just
// before SIGTERM must survive a restart.
func TestJobsSurviveShutdown(t *testing.T) {
	home := t.TempDir()
	addr := freePort(t)
	d := start(t, home, addr)

	body := strings.NewReader(`{"url":"http://127.0.0.1:1/never.bin"}`)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).
		Post("http://"+addr+"/api/jobs", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(b, &created); err != nil || created.ID == "" {
		t.Fatalf("job not created: %s", b)
	}

	d.stop()

	storePath := filepath.Join(home, "Library", "Application Support", "MacDM", "jobs.json")
	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("store missing after shutdown: %v", err)
	}
	if !strings.Contains(string(raw), created.ID) {
		t.Fatalf("job %s was lost on shutdown", created.ID)
	}

	// And it comes back on restart.
	addr2 := freePort(t)
	start(t, home, addr2)
	var jobs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(get(addr2, "/api/jobs"), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == created.ID {
			return
		}
	}
	t.Fatalf("job %s not restored after restart", created.ID)
}

func TestDownloadDirFlag(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "elsewhere")
	addr := freePort(t)

	cmd := exec.Command(daemonBinary(t), "-addr", addr, "-dir", custom)
	cmd.Env = append(os.Environ(), "HOME="+home)
	log := &safeBuf{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-exited
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && get(addr, "/api/health") == nil {
		time.Sleep(100 * time.Millisecond)
	}
	if get(addr, "/api/health") == nil {
		t.Fatalf("daemon never came up; log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), custom) {
		t.Errorf("-dir was ignored; log:\n%s", log.String())
	}
}
