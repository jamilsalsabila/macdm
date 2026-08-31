// Command macdm is the MacDM command-line client. It speaks the same loopback
// HTTP API as the menu-bar app.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"macdm/internal/config"
)

func base() string {
	if a := os.Getenv("MACDM_ADDR"); a != "" {
		return "http://" + a
	}
	return "http://" + config.Load().Addr
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// Every command except `daemon` itself needs a running daemon; start one
	// transparently if the loopback API isn't answering yet.
	if os.Args[1] != "daemon" && os.Args[1] != "help" &&
		os.Args[1] != "-h" && os.Args[1] != "--help" {
		if err := ensureDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "macdm: "+err.Error())
			os.Exit(1)
		}
	}

	var err error
	switch os.Args[1] {
	case "add":
		err = cmdAdd(os.Args[2:])
	case "ls", "list":
		err = cmdList()
	case "pause":
		err = cmdSimple("pause", os.Args[2:])
	case "resume":
		err = cmdSimple("resume", os.Args[2:])
	case "rm", "remove":
		err = cmdRemove(os.Args[2:])
	case "watch":
		err = cmdWatch()
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "restart":
		err = cmdRestart()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "macdm: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`macdm - MacDM download manager client

  macdm add <url> [-o name] [-n conns] [-H "Header: value"]...
  macdm ls
  macdm pause <id>
  macdm resume <id>
  macdm rm <id>
  macdm watch                 stream live progress
  macdm daemon [args...]      run the daemon in the foreground
  macdm restart               stop + restart the daemon (after a rebuild)

Set MACDM_ADDR to override the daemon address (default ` + config.DefaultAddr + `).
`)
}

func cmdAdd(args []string) error {
	body := map[string]any{}
	headers := map[string]string{}
	var url string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "-n", "-H":
			// A trailing flag with no value used to run off the end of args and
			// panic with an index-out-of-range.
			flag := args[i]
			i++
			if i >= len(args) {
				return fmt.Errorf("%s needs a value", flag)
			}
			switch flag {
			case "-o":
				body["filename"] = args[i]
			case "-n":
				var n int
				if _, err := fmt.Sscan(args[i], &n); err != nil || n < 1 || n > 64 {
					return fmt.Errorf("bad -n %q (want 1–64)", args[i])
				}
				body["conns"] = n
			case "-H":
				k, v, ok := strings.Cut(args[i], ":")
				if !ok {
					return fmt.Errorf("bad -H %q (want \"Key: value\")", args[i])
				}
				headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		default:
			url = args[i]
		}
	}
	if url == "" {
		return fmt.Errorf("missing URL")
	}
	body["url"] = url
	if len(headers) > 0 {
		body["headers"] = headers
	}

	var j job
	if err := doJSON("POST", "/api/jobs", body, &j); err != nil {
		return err
	}
	fmt.Printf("%s  %s  [%s]\n", j.ID, j.Filename, j.Status)
	return nil
}

func cmdList() error {
	var jobs []job
	if err := doJSON("GET", "/api/jobs", nil, &jobs); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("(no jobs)")
		return nil
	}
	for _, j := range jobs {
		fmt.Printf("%-16s %-12s %6.1f%%  %10s  %s\n",
			j.ID, j.Status, j.percent(), humanSpeed(j.SpeedBps), j.Filename)
	}
	return nil
}

func cmdSimple(action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: macdm %s <id>", action)
	}
	return doJSON("POST", "/api/jobs/"+args[0]+"/"+action, nil, nil)
}

func cmdRemove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: macdm rm <id>")
	}
	return doJSON("DELETE", "/api/jobs/"+args[0], nil, nil)
}

func cmdWatch() error {
	resp, err := http.Get(base() + "/api/events")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Job  job    `json:"job"`
		}
		if json.Unmarshal([]byte(line[6:]), &ev) != nil {
			continue
		}
		j := ev.Job
		fmt.Printf("\r\033[K%-16s %-12s %6.1f%%  %10s  %s",
			j.ID, j.Status, j.percent(), humanSpeed(j.SpeedBps), j.Filename)
		if ev.Type == "delete" || j.Status == "completed" || j.Status == "error" {
			fmt.Println()
		}
	}
	return sc.Err()
}

// cmdRestart stops any running daemon and starts a fresh one — use after
// rebuilding macdmd.
func cmdRestart() error {
	req, _ := http.NewRequest("POST", base()+"/api/shutdown", nil)
	if resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
		fmt.Fprintln(os.Stderr, "macdm: stopped running daemon")
		time.Sleep(time.Second)
	}
	return ensureDaemon()
}

// ensureDaemon returns once the daemon answers /api/health. If nothing is
// listening it spawns `macdmd` detached (logging to the support dir) and waits
// up to ~6s for it to bind.
func ensureDaemon() error {
	if daemonAlive(300 * time.Millisecond) {
		return nil
	}

	bin, err := findDaemonBinary()
	if err != nil {
		return fmt.Errorf("daemon not running and %w", err)
	}

	logPath := filepath.Join(config.SupportDir(), "macdmd.log")
	logf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)

	cmd := exec.Command(bin)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachAttr() // new session so it outlives this CLI
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start macdmd: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if daemonAlive(400 * time.Millisecond) {
			fmt.Fprintln(os.Stderr, "macdm: started daemon (log: "+logPath+")")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("macdmd did not come up; see %s", logPath)
}

func daemonAlive(timeout time.Duration) bool {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(base() + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func findDaemonBinary() (string, error) {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "macdmd")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("macdmd"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("macdmd binary not found next to macdm or on PATH")
}

func cmdDaemon(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cand := filepath.Join(filepath.Dir(exe), "macdmd")
	if _, err := os.Stat(cand); err != nil {
		if p, e := exec.LookPath("macdmd"); e == nil {
			cand = p
		} else {
			return fmt.Errorf("macdmd binary not found next to macdm or on PATH")
		}
	}
	c := exec.Command(cand, args...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	return c.Run()
}

// --- HTTP helpers ---

func doJSON(method, path string, in, out any) error {
	var rdr io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base()+path, rdr)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%w (is the daemon running? try `macdm daemon`)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type job struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	Status     string `json:"status"`
	TotalBytes int64  `json:"total_bytes"`
	DoneBytes  int64  `json:"done_bytes"`
	SpeedBps   int64  `json:"speed_bps"`
}

func (j job) percent() float64 {
	if j.TotalBytes <= 0 {
		return 0
	}
	return float64(j.DoneBytes) / float64(j.TotalBytes) * 100
}

func humanSpeed(n int64) string {
	if n <= 0 {
		return "-"
	}
	f := float64(n)
	for _, u := range []string{"B/s", "KB/s", "MB/s", "GB/s"} {
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f TB/s", f)
}
