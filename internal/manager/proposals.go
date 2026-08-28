package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"macdm/internal/sniff"
	"macdm/internal/store"
)

// Proposal is a caught download awaiting the user's decision in the "New
// Download" dialog. If no UI answers within PromptTimeout it is auto-accepted
// with defaults (this is what makes headless / app-closed use still work).
type Proposal struct {
	ID        string               `json:"id"`
	URL       string               `json:"url"`
	Kind      string               `json:"kind"`
	Category  string               `json:"category"`
	Title     string               `json:"title,omitempty"`
	Filename  string               `json:"filename"`
	Size      int64                `json:"size"`
	Resumable bool                 `json:"resumable"`
	DRM       bool                 `json:"drm"`
	Probing   bool                 `json:"probing"` // still resolving quality/size
	Referer   string               `json:"referer,omitempty"`
	Headers   map[string]string    `json:"headers,omitempty"`
	Formats   []store.FormatChoice `json:"formats,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
}

// AcceptOptions are the choices the dialog collects.
type AcceptOptions struct {
	Dest     string // full path or directory; "" => default download dir
	Filename string // override basename
	Conns    int
	FormatID string // selector/variant-url/height token from the chosen FormatChoice
	Quality  string // human label, for display
}

// Notice is a manager-level event (proposals, etc.) delivered over SSE
// alongside store job events.
type Notice struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type proposalHub struct {
	mu        sync.Mutex
	pending   map[string]*Proposal
	subs      map[int]chan Notice
	nextSub   int
	autoTimer map[string]*time.Timer
}

func newProposalHub() *proposalHub {
	return &proposalHub{
		pending:   map[string]*Proposal{},
		subs:      map[int]chan Notice{},
		autoTimer: map[string]*time.Timer{},
	}
}

// Subscribe returns a channel of manager notices and a cancel func.
func (m *Manager) Subscribe() (<-chan Notice, func()) {
	h := m.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextSub
	h.nextSub++
	ch := make(chan Notice, 32)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

func (h *proposalHub) broadcast(n Notice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- n:
		default:
			delete(h.subs, id)
			close(ch)
		}
	}
}

func notice(t string, v any) Notice {
	b, _ := json.Marshal(v)
	return Notice{Type: t, Data: b}
}

// PendingProposals lists proposals still awaiting a decision.
func (m *Manager) PendingProposals() []*Proposal {
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	out := make([]*Proposal, 0, len(m.hub.pending))
	for _, p := range m.hub.pending {
		cp := *p
		out = append(out, &cp)
	}
	return out
}

// Propose stores a Proposal and returns immediately. Probing (which may run
// yt-dlp for many seconds) happens in the background; when it finishes the
// proposal is updated and a second "proposal" notice is emitted so an open
// dialog can fill in its quality menu. An auto-accept fallback is armed for when
// no UI answers.
func (m *Manager) Propose(rawurl string, headers map[string]string, referer string) *Proposal {
	rawurl = strings.TrimSpace(rawurl)
	u, _ := url.Parse(rawurl)
	kind := sniff.KindHTTP
	name := "download"
	if u != nil {
		kind = sniff.ClassifyURL(u)
		if b := baseName(u); b != "" {
			name = b
		}
	}

	m.hub.mu.Lock()
	// De-dupe: a rapid second click (or the sniffer re-firing) must not spawn a
	// second dialog / job for a URL that is already pending or downloading.
	for _, ex := range m.hub.pending {
		if ex.URL == rawurl {
			cp := *ex
			m.hub.mu.Unlock()
			m.hub.broadcast(notice("proposal", &cp)) // resurface the dialog
			return &cp
		}
	}
	m.hub.mu.Unlock()
	if j := m.activeJobForURL(rawurl); j != nil {
		m.hub.broadcast(notice("proposal-resolved", map[string]string{"job_id": j.ID, "duplicate": "true"}))
		return &Proposal{ID: "", URL: rawurl, Kind: kind, Filename: j.Filename}
	}

	p := &Proposal{
		ID:        newID(),
		URL:       rawurl,
		Kind:      kind,
		Filename:  name,
		Referer:   referer,
		Headers:   headers,
		Probing:   kind != sniff.KindHTTP,
		CreatedAt: time.Now(),
	}

	m.hub.mu.Lock()
	m.hub.pending[p.ID] = p
	hasUI := m.hub.hasSubsLocked()
	autoAccept := m.cfg.AutoAccept || !hasUI
	// With a UI connected the dialog waits for the user — only a long safety net
	// fallback (5 min). With no UI, accept quickly once the probe has a real
	// filename/quality.
	var delay time.Duration
	if autoAccept {
		delay = 15 * time.Second
	} else {
		delay = 5 * time.Minute
	}
	m.hub.autoTimer[p.ID] = time.AfterFunc(delay, func() {
		_, _ = m.Accept(p.ID, AcceptOptions{})
	})
	snap := *p // copy for the event — enrichProposal will mutate the map entry
	m.hub.mu.Unlock()

	if !autoAccept {
		m.hub.broadcast(notice("proposal", &snap))
	}

	// Probe in the background; for the no-UI case, accept as soon as it returns.
	if snap.Probing || kind == sniff.KindHTTP {
		go func() {
			m.enrichProposal(p.ID, rawurl, headers)
			if autoAccept {
				_, _ = m.Accept(p.ID, AcceptOptions{})
			}
		}()
	}
	return &snap // never hand out the live map pointer
}

// activeJobForURL returns a job with the same source URL that is still running
// OR finished within the last 20s — so a burst of clicks on the floating button
// doesn't spawn duplicates.
func (m *Manager) activeJobForURL(rawurl string) *store.Job {
	for _, j := range m.st.List() {
		if j.URL != rawurl {
			continue
		}
		if !j.Terminal() || time.Since(j.UpdatedAt) < 20*time.Second {
			return j
		}
	}
	return nil
}

func (m *Manager) enrichProposal(id, rawurl string, headers map[string]string) {
	pr := m.Probe(context.Background(), rawurl, headers)

	m.hub.mu.Lock()
	p, ok := m.hub.pending[id]
	if !ok {
		m.hub.mu.Unlock()
		return
	}
	p.Probing = false
	p.Title = pr.Title
	p.Size = pr.Size
	p.Resumable = pr.Resumable
	p.DRM = pr.DRM
	p.Formats = pr.Formats
	if pr.Filename != "" {
		p.Filename = pr.Filename
	}
	if pr.Kind != "" {
		p.Kind = pr.Kind
	}
	cp := *p
	m.hub.mu.Unlock()

	m.hub.broadcast(notice("proposal", &cp))
}

func (h *proposalHub) hasSubsLocked() bool { return len(h.subs) > 0 }

var errProposalGone = errors.New("proposal already resolved")

// Accept turns a proposal into a running job.
func (m *Manager) Accept(id string, opt AcceptOptions) (*store.Job, error) {
	m.hub.mu.Lock()
	pp, ok := m.hub.pending[id]
	if !ok {
		m.hub.mu.Unlock()
		return nil, errProposalGone
	}
	p := *pp // copy under the lock — enrichProposal may still be mutating pp
	delete(m.hub.pending, id)
	if t := m.hub.autoTimer[id]; t != nil {
		t.Stop()
		delete(m.hub.autoTimer, id)
	}
	m.hub.mu.Unlock()

	name := opt.Filename
	if name == "" {
		name = p.Filename
	}
	dest := opt.Dest
	switch {
	case dest == "":
		dest = filepath.Join(m.cfg.DownloadDir, name)
	case isDir(dest):
		dest = filepath.Join(dest, name)
	}

	j, err := m.Add(p.URL, AddOptions{
		Headers:  p.Headers,
		Filename: name,
		Dest:     dest,
		Conns:    opt.Conns,
		FormatID: opt.FormatID,
		Quality:  opt.Quality,
		Formats:  p.Formats,
	})
	if err != nil {
		return nil, err
	}
	m.hub.broadcast(notice("proposal-resolved", map[string]string{"id": id, "job_id": j.ID}))
	return j, nil
}

// Reject discards a proposal.
func (m *Manager) Reject(id string) error {
	m.hub.mu.Lock()
	_, ok := m.hub.pending[id]
	if ok {
		delete(m.hub.pending, id)
		if t := m.hub.autoTimer[id]; t != nil {
			t.Stop()
			delete(m.hub.autoTimer, id)
		}
	}
	m.hub.mu.Unlock() // must not hold hub.mu across broadcast (it re-locks)

	if !ok {
		return errProposalGone
	}
	m.hub.broadcast(notice("proposal-resolved", map[string]string{"id": id, "rejected": "true"}))
	return nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
