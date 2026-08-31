// Command macdm-nmhost is the native messaging host that Chrome/Edge/Brave/
// Firefox launch when the MacDM extension connects. It does almost nothing: it
// translates between the browser's stdio framing (a 4-byte little-endian length
// prefix followed by a JSON message) and the daemon's loopback HTTP API.
//
// Keeping it a dumb relay means the browser only ever spawns a tiny, fast
// binary; all real work (classification, downloading, muxing) stays in macdmd,
// which the user can restart independently.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"macdm/internal/config"
)

// inbound is what the extension sends us.
type inbound struct {
	Type      string            `json:"type"` // "ping" | "download" | "probe"
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Referer   string            `json:"referer"`
	Title     string            `json:"title"`
	Conns     int               `json:"conns"`
	Fragments []fragment        `json:"fragments"`
}

type fragment struct {
	URL   string `json:"url"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

// outbound is what we send back to the extension.
type outbound struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	// JobID is set only when a job really was created (the fragment path).
	// The proposal path cannot fill it in: the daemon may still be waiting for
	// the user to answer the dialog, and if it auto-accepts the job it creates
	// gets an id of its own. Reporting the proposal's id as a job id was a
	// misnomer — nothing consumed it, but the next reader would have believed
	// it.
	JobID      string          `json:"job_id,omitempty"`
	ProposalID string          `json:"proposal_id,omitempty"`
	Error      string          `json:"error,omitempty"`
	Raw        json.RawMessage `json:"result,omitempty"`
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("macdm-nmhost: ")

	base := "http://" + config.Load().Addr
	hc := &http.Client{Timeout: 25 * time.Second}

	for {
		msg, err := readMessage(os.Stdin)
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("read: %v", err)
			return
		}

		var in inbound
		if err := json.Unmarshal(msg, &in); err != nil {
			_ = writeMessage(os.Stdout, mustJSON(outbound{Type: "error", Error: "bad json"}))
			continue
		}

		var out outbound
		switch in.Type {
		case "ping":
			out = outbound{Type: "pong", OK: pingDaemon(hc, base)}
		case "download":
			out = doProposal(hc, base, in)
		case "probe":
			out = doProbe(hc, base, in)
		default:
			out = outbound{Type: "error", Error: "unknown type " + in.Type}
		}
		if err := writeMessage(os.Stdout, mustJSON(out)); err != nil {
			log.Printf("write: %v", err)
			return
		}
	}
}

func pingDaemon(hc *http.Client, base string) bool {
	resp, err := hc.Get(base + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// doProposal hands the caught URL to the daemon as a *proposal* — the daemon
// probes it and either raises the "New Download" dialog (if the app is running)
// or auto-accepts. Either way the extension's job here is done.
func doProposal(hc *http.Client, base string, in inbound) outbound {
	headers := map[string]string{}
	for k, v := range in.Headers {
		headers[k] = v
	}
	if in.Referer != "" && headers["Referer"] == "" {
		headers["Referer"] = in.Referer
	}

	// Instagram/Facebook video arrives as a set of byte-range fragments with no
	// single addressable URL and no page URL yt-dlp can resolve. Skip the
	// proposal/probe dance and create the assembly job directly.
	if len(in.Fragments) > 0 {
		return doFragmentJob(hc, base, in, headers)
	}

	body := mustJSON(map[string]any{
		"url":     in.URL,
		"headers": headers,
		"referer": in.Referer,
	})
	resp, err := hc.Post(base+"/api/proposals", "application/json", bytes.NewReader(body))
	if err != nil {
		return outbound{Type: "downloadResult", Error: "daemon unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return outbound{Type: "downloadResult", Error: fmt.Sprintf("daemon %s: %s", resp.Status, data)}
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &p)
	return outbound{Type: "downloadResult", OK: true, ProposalID: p.ID}
}

// doFragmentJob creates a fragment-assembly job on the daemon directly. There is
// no useful dialog to raise (the user cannot pick a "quality" for an already-
// segmented progressive MP4), so the job just starts.
func doFragmentJob(hc *http.Client, base string, in inbound, headers map[string]string) outbound {
	frags := make([]map[string]any, len(in.Fragments))
	for i, f := range in.Fragments {
		frags[i] = map[string]any{"url": f.URL, "start": f.Start, "end": f.End}
	}
	name := in.Title
	body := mustJSON(map[string]any{
		"url":       in.URL,
		"headers":   headers,
		"filename":  name,
		"conns":     in.Conns,
		"fragments": frags,
	})
	resp, err := hc.Post(base+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		return outbound{Type: "downloadResult", Error: "daemon unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return outbound{Type: "downloadResult", Error: fmt.Sprintf("daemon %s: %s", resp.Status, data)}
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &p)
	return outbound{Type: "downloadResult", OK: true, JobID: p.ID}
}

func doProbe(hc *http.Client, base string, in inbound) outbound {
	body := mustJSON(map[string]any{"url": in.URL, "headers": in.Headers})
	resp, err := hc.Post(base+"/api/probe", "application/json", bytes.NewReader(body))
	if err != nil {
		return outbound{Type: "probeResult", Error: err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return outbound{Type: "probeResult", OK: resp.StatusCode < 300, Raw: json.RawMessage(data)}
}

// --- Chrome native-messaging stdio framing ---

func readMessage(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n == 0 || n > 64*1024*1024 {
		return nil, fmt.Errorf("message length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeMessage(w io.Writer, payload []byte) error {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error","error":"marshal failed"}`)
	}
	return b
}
