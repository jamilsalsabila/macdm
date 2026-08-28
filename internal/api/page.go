package api

import (
	_ "embed"
	"net/http"
)

//go:embed page.html
var pageHTML []byte

// page serves a single-file status dashboard. It is a convenience for testing
// and headless use; the primary UI is the menu-bar app, which talks to the same
// REST + SSE endpoints.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(pageHTML)
}
