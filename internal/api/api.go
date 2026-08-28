// Package api exposes the daemon over loopback HTTP: a small REST surface for
// jobs plus a Server-Sent Events stream for live progress.
//
// SSE is used instead of WebSocket because progress is one-directional and SSE
// needs no dependency, no handshake, and reconnects on its own. Commands travel
// as ordinary POSTs.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"macdm/internal/config"
	"macdm/internal/manager"
	"macdm/internal/store"
	"macdm/internal/tools"
)

// Server is an http.Handler serving the API and the bundled status page.
type Server struct {
	mgr *manager.Manager
	mux *http.ServeMux
}

// New builds the Server.
func New(mgr *manager.Manager) *Server {
	s := &Server{mgr: mgr, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Loopback-only guard: reject anything that isn't from 127.0.0.1/::1.
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("POST /api/shutdown", s.shutdown)
	s.mux.HandleFunc("GET /api/jobs", s.listJobs)
	s.mux.HandleFunc("POST /api/jobs", s.createJob)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/pause", s.pauseJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/resume", s.resumeJob)
	s.mux.HandleFunc("POST /api/jobs/{id}/conns", s.setConns)
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.deleteJob)
	s.mux.HandleFunc("POST /api/probe", s.probe)
	s.mux.HandleFunc("GET /api/proposals", s.listProposals)
	s.mux.HandleFunc("POST /api/proposals", s.createProposal)
	s.mux.HandleFunc("POST /api/proposals/{id}/accept", s.acceptProposal)
	s.mux.HandleFunc("POST /api/proposals/{id}/reject", s.rejectProposal)
	s.mux.HandleFunc("GET /api/events", s.events)
	s.mux.HandleFunc("GET /api/tools", s.getTools)
	s.mux.HandleFunc("POST /api/tools/ytdlp/update", s.updateYtDlp)
	s.mux.HandleFunc("POST /api/config", s.patchConfig)
	s.mux.HandleFunc("GET /", s.page)
}

type toolInfo struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func (s *Server) getTools(w http.ResponseWriter, r *http.Request) {
	set := s.mgr.Tools()
	yt, _ := tools.CheckYtDlp(r.Context(), set) // best-effort; local fields still filled
	writeJSON(w, http.StatusOK, map[string]any{
		"ffmpeg": toolInfo{Path: set.Ffmpeg, Version: tools.Version(r.Context(), set.Ffmpeg)},
		"ytdlp": map[string]any{
			"path":             yt.Path,
			"version":          yt.Version,
			"latest":           yt.Latest,
			"update_available": yt.UpdateAvailable,
		},
		"auto_update": config.Load().AutoUpdateYtDlpEnabled(),
	})
}

func (s *Server) updateYtDlp(w http.ResponseWriter, r *http.Request) {
	from, to, err := tools.UpdateYtDlp(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": from, "to": to})
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoUpdateYtDlp *bool   `json:"auto_update_ytdlp"`
		CookiesFrom     *string `json:"cookies_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c := config.Load()
	if req.AutoUpdateYtDlp != nil {
		c.AutoUpdateYtDlp = req.AutoUpdateYtDlp
	}
	if req.CookiesFrom != nil {
		c.CookiesFrom = *req.CookiesFrom
	}
	if err := config.Save(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Probe(r.Context(), req.URL, req.Headers))
}

func (s *Server) listProposals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.PendingProposals())
}

func (s *Server) createProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Referer string            `json:"referer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.URL == "" {
		writeErr(w, http.StatusBadRequest, errBadURL)
		return
	}
	writeJSON(w, http.StatusCreated, s.mgr.Propose(req.URL, req.Headers, req.Referer))
}

func (s *Server) acceptProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dest     string `json:"dest"`
		Filename string `json:"filename"`
		Conns    int    `json:"conns"`
		FormatID string `json:"format_id"`
		Quality  string `json:"quality"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	j, err := s.mgr.Accept(r.PathValue("id"), manager.AcceptOptions{
		Dest: req.Dest, Filename: req.Filename, Conns: req.Conns,
		FormatID: req.FormatID, Quality: req.Quality,
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) rejectProposal(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Reject(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errBadURL = errString("missing url")

type errString string

func (e errString) Error() string { return string(e) }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "time": time.Now(), "version": config.Version,
	})
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	go func() {
		time.Sleep(150 * time.Millisecond)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}()
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Store().List())
}

type createReq struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Filename  string            `json:"filename"`
	Conns     int               `json:"conns"`
	Dest      string            `json:"dest"`
	FormatID  string            `json:"format_id"`
	Quality   string            `json:"quality"`
	Fragments []store.Fragment  `json:"fragments"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	j, err := s.mgr.Add(req.URL, manager.AddOptions{
		Headers:   req.Headers,
		Filename:  req.Filename,
		Conns:     req.Conns,
		Dest:      req.Dest,
		FormatID:  req.FormatID,
		Quality:   req.Quality,
		Fragments: req.Fragments,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.mgr.Store().Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) pauseJob(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Pause(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Resume(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setConns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Conns int `json:"conns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mgr.SetConns(r.PathValue("id"), req.Conns); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Remove(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams store changes as SSE. Each message is a JSON store.Event.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.mgr.Store().Watch()
	defer cancel()
	notices, cancelN := s.mgr.Subscribe()
	defer cancelN()

	// Prime the client with the current state.
	for _, j := range s.mgr.Store().List() {
		writeSSE(w, store.Event{Type: "job", Job: j})
	}
	for _, p := range s.mgr.PendingProposals() {
		writeRawSSE(w, "proposal", p)
	}
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // watcher dropped (slow client) — let it reconnect
			}
			writeSSE(w, ev)
			flusher.Flush()
		case n, ok := <-notices:
			if !ok {
				notices = nil // disable this case; a nil channel never fires (no spin)
				continue
			}
			// n.Data is already JSON; forward it under n.Type.
			fmt.Fprintf(w, "data: {\"type\":%q,\"%s\":%s}\n\n", n.Type, noticeKey(n.Type), n.Data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func noticeKey(t string) string {
	if t == "proposal" {
		return "proposal"
	}
	return "data"
}

func writeRawSSE(w http.ResponseWriter, typ string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: {\"type\":%q,\"%s\":%s}\n\n", typ, noticeKey(typ), b)
}

func statusFor(err error) int {
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeSSE(w http.ResponseWriter, ev store.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if i := strings.LastIndex(remoteAddr, ":"); i >= 0 {
		host = remoteAddr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == ""
}
