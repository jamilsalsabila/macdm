// Package api exposes the daemon over loopback HTTP: a small REST surface for
// jobs plus a Server-Sent Events stream for live progress.
//
// SSE is used instead of WebSocket because progress is one-directional and SSE
// needs no dependency, no handshake, and reconnects on its own. Commands travel
// as ordinary POSTs.
package api

import (
	"context"
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
	"macdm/internal/schedule"
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
	cfg := config.Load()
	tctx, tcancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer tcancel()
	yt, _ := tools.CheckYtDlp(tctx, set, cfg.YtDlpChannelName()) // best-effort; local fields always filled
	sched := s.mgr.Schedule()
	schedDays := []int{}
	for i, on := range sched.Days {
		if on {
			schedDays = append(schedDays, i)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ffmpeg": toolInfo{Path: set.Ffmpeg, Version: tools.Version(r.Context(), set.Ffmpeg)},
		"ytdlp": map[string]any{
			"path":             yt.Path,
			"version":          yt.Version,
			"latest":           yt.Latest,
			"channel":          yt.Channel,
			"update_available": yt.UpdateAvailable,
		},
		"auto_update":    cfg.AutoUpdateYtDlpEnabled(),
		"cookies_from":   cfg.CookiesFrom,
		"subtitle_langs": cfg.SubtitleLangs,
		"auto_subs":      cfg.AutoSubs,
		"audio_lang":     cfg.AudioLang,
		// From the manager, not the file: this is the ceiling in force now.
		"speed_limit_bps": s.mgr.SpeedLimit(),
		"schedule": map[string]any{
			"enabled": sched.Enabled,
			"start":   schedule.FormatHM(sched.Start),
			"stop":    schedule.FormatHM(sched.Stop),
			"days":    schedDays,
			"open":    sched.Active(time.Now()),
		},
	})
}

func (s *Server) updateYtDlp(w http.ResponseWriter, r *http.Request) {
	from, to, err := tools.UpdateYtDlp(r.Context(), config.Load().YtDlpChannelName())
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
		YtDlpChannel    *string `json:"ytdlp_channel"`
		SubtitleLangs   *string `json:"subtitle_langs"`
		AutoSubs        *bool   `json:"auto_subs"`
		AudioLang       *string `json:"audio_lang"`
		SpeedLimitBps   *int64  `json:"speed_limit_bps"`
		ScheduleEnabled *bool   `json:"schedule_enabled"`
		ScheduleStart   *string `json:"schedule_start"`
		ScheduleStop    *string `json:"schedule_stop"`
		ScheduleDays    *[]int  `json:"schedule_days"`
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
	if req.SubtitleLangs != nil {
		c.SubtitleLangs = *req.SubtitleLangs
	}
	if req.AutoSubs != nil {
		c.AutoSubs = *req.AutoSubs
	}
	if req.AudioLang != nil {
		c.AudioLang = *req.AudioLang
	}
	if req.YtDlpChannel != nil && (*req.YtDlpChannel == "stable" || *req.YtDlpChannel == "nightly") {
		c.YtDlpChannel = *req.YtDlpChannel
	}
	touchedSpeed := false
	if req.SpeedLimitBps != nil {
		if *req.SpeedLimitBps < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("speed_limit_bps cannot be negative"))
			return
		}
		c.SpeedLimitBps = *req.SpeedLimitBps
		touchedSpeed = true
	}
	touchedSchedule := false
	if req.ScheduleEnabled != nil {
		c.ScheduleEnabled = *req.ScheduleEnabled
		touchedSchedule = true
	}
	// Validate the times before storing them: a window saved from a typo would
	// block every download until someone worked out why.
	for _, f := range []struct {
		in   *string
		out  *string
		name string
	}{
		{req.ScheduleStart, &c.ScheduleStart, "schedule_start"},
		{req.ScheduleStop, &c.ScheduleStop, "schedule_stop"},
	} {
		if f.in == nil {
			continue
		}
		if _, err := schedule.ParseHM(*f.in); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%s: %w", f.name, err))
			return
		}
		*f.out = *f.in
		touchedSchedule = true
	}
	if req.ScheduleDays != nil {
		for _, d := range *req.ScheduleDays {
			if d < 0 || d > 6 {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("schedule_days: %d is not a weekday (0-6)", d))
				return
			}
		}
		c.ScheduleDays = *req.ScheduleDays
		touchedSchedule = true
	}

	// ScheduleWindow silently disables itself on unusable times, which is the
	// right failure for a config file read at startup but the wrong answer to
	// someone switching the scheduler on: it would report success and do
	// nothing at all.
	if c.ScheduleEnabled && !c.ScheduleWindow().Enabled {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("the schedule needs a start and stop time in HH:MM"))
		return
	}

	if err := config.Save(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Only once the file is safely written: reporting a failure while the
	// running daemon had already changed would leave the two disagreeing.
	if touchedSpeed {
		// Immediately, not at the next restart — throttling a download already
		// in flight is the case people actually want this for.
		s.mgr.SetSpeedLimit(c.SpeedLimitBps)
	}
	if touchedSchedule {
		// Likewise: switching the scheduler off gives the downloads back at once.
		s.mgr.SetSchedule(c.ScheduleWindow())
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
		Dest          string `json:"dest"`
		Filename      string `json:"filename"`
		AudioLang     string `json:"audio_lang"`
		SubtitleLangs string `json:"subtitle_langs"`
		Conns         int    `json:"conns"`
		FormatID      string `json:"format_id"`
		Quality       string `json:"quality"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	j, err := s.mgr.Accept(r.PathValue("id"), manager.AcceptOptions{
		Dest: req.Dest, Filename: req.Filename, Conns: req.Conns,
		FormatID: req.FormatID, Quality: req.Quality,
		AudioLang: req.AudioLang, SubLangs: req.SubtitleLangs,
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
	// Extractor-path overrides; empty falls back to the Settings default.
	AudioLang     string `json:"audio_lang"`
	SubtitleLangs string `json:"subtitle_langs"`
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
		AudioLang: req.AudioLang,
		SubLangs:  req.SubtitleLangs,
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

	// write returns false once the socket is dead so we stop instead of spinning
	// on a half-open connection until r.Context() eventually notices.
	write := func(format string, a ...any) bool {
		if _, err := fmt.Fprintf(w, format, a...); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Prime the client with the current state.
	for _, j := range s.mgr.Store().List() {
		if !write("data: %s\n\n", mustJSON(store.Event{Type: "job", Job: j})) {
			return
		}
	}
	for _, p := range s.mgr.PendingProposals() {
		if !write("data: {\"type\":\"proposal\",\"proposal\":%s}\n\n", mustJSON(p)) {
			return
		}
	}

	keepalive := time.NewTicker(10 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // watcher dropped (slow client) — let it reconnect
			}
			if !write("data: %s\n\n", mustJSON(ev)) {
				return
			}
		case n, ok := <-notices:
			if !ok {
				notices = nil // disable this case; a nil channel never fires (no spin)
				continue
			}
			// n.Data is already JSON; forward it under n.Type.
			if !write("data: {\"type\":%q,\"%s\":%s}\n\n", n.Type, noticeKey(n.Type), n.Data) {
				return
			}
		case <-keepalive.C:
			if !write(": keepalive\n\n") {
				return
			}
		}
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func noticeKey(t string) string {
	if t == "proposal" {
		return "proposal"
	}
	return "data"
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

func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if i := strings.LastIndex(remoteAddr, ":"); i >= 0 {
		host = remoteAddr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == ""
}
