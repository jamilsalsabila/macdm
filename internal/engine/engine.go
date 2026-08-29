// Package engine is the plain-HTTP download core: probe a URL, split it into byte
// ranges, fetch the ranges concurrently, and record progress so an interrupted
// download resumes instead of restarting.
//
// This is the mechanism a sniffer or extractor ultimately hands work to: both
// paths end with "here is a media URL plus the headers the browser used" and the
// engine replays exactly that.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config tunes a downloader. The zero value is not useful; use Default.
type Config struct {
	MaxConns  int           // connections per download
	MinChunk  int64         // never split a file into pieces smaller than this
	UserAgent string        // fallback UA when a job carries none
	Timeout   time.Duration // per-request timeout (not whole-download)
}

// DefaultUserAgent is a current Chrome UA. Many CDNs 403 a non-browser
// User-Agent, and the sniffer often can't capture the real one (MV3 hides it on
// some requests), so this is the fallback for a job that carries none.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Default returns sensible values matching IDM-class defaults.
func Default() Config {
	return Config{
		MaxConns:  8,
		MinChunk:  1 << 20, // 1 MiB
		UserAgent: DefaultUserAgent,
		Timeout:   30 * time.Second,
	}
}

// Progress is delivered to the caller's callback roughly twice a second.
type Progress struct {
	DoneBytes  int64
	TotalBytes int64
	SpeedBps   int64
	NumConns   int            // number of connections in use
	Resumable  bool           // server supports Range (download can be paused/resumed)
	Conns      []ConnProgress // per-connection detail (nil for single-stream)
}

// ConnProgress is one connection's slice of the file, for the IDM-style
// "download progress by connections" table.
type ConnProgress struct {
	Index      int    `json:"index"`
	Start      int64  `json:"start"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Status     string `json:"status"` // connecting|receiving|done|error|idle
}

// Probe describes what the server will let us do with a URL.
type Probe struct {
	TotalBytes    int64
	AcceptRanges  bool
	Filename      string
	NamedByServer bool // Filename came from Content-Disposition, not the URL path
	ContentType   string
	ETag          string
}

// Engine performs downloads. It is safe for concurrent use.
type Engine struct {
	cfg    Config
	client *http.Client
}

// New builds an Engine. The HTTP client uses a Chrome-disguised TLS handshake
// (see utls.go) and preserves auth headers across same-site redirects.
func New(cfg Config) *Engine {
	return &Engine{
		cfg:    cfg,
		client: newHTTPClient(),
	}
}

func (e *Engine) req(ctx context.Context, method, rawurl string, headers map[string]string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, rawurl, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", e.cfg.UserAgent)
	}
	return r, nil
}

// ProbeURL issues a ranged GET for the first byte to learn size, range support,
// and a filename. It is deliberately a GET, not a HEAD: many CDNs answer HEAD
// differently (or not at all) from the GET the browser actually made.
func (e *Engine) ProbeURL(ctx context.Context, rawurl string, headers map[string]string) (*Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	r, err := e.req(ctx, http.MethodGet, rawurl, headers)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Range", "bytes=0-0")

	resp, err := e.client.Do(r)
	if err != nil {
		return nil, err
	}

	// Some CDNs (TikTok/ByteDance) 403 a 1-byte range probe but serve the full
	// GET fine — retry once without the Range header before giving up.
	if resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		r2, e2 := e.req(ctx, http.MethodGet, rawurl, headers)
		if e2 != nil {
			return nil, e2
		}
		if resp, err = e.client.Do(r2); err != nil {
			return nil, err
		}
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		snip, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		if s := strings.TrimSpace(collapseWS(string(snip))); s != "" {
			return nil, fmt.Errorf("probe: %s — %s", resp.Status, s)
		}
		return nil, fmt.Errorf("probe: unexpected status %s", resp.Status)
	}

	cd := resp.Header.Get("Content-Disposition")
	p := &Probe{
		ContentType:   resp.Header.Get("Content-Type"),
		ETag:          resp.Header.Get("ETag"),
		Filename:      filenameFrom(rawurl, cd),
		NamedByServer: strings.Contains(strings.ToLower(cd), "filename"),
	}

	if resp.StatusCode == http.StatusPartialContent {
		p.AcceptRanges = true
		// Content-Range: bytes 0-0/1234567
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
					p.TotalBytes = n
				}
			}
		}
	} else {
		// 200: server ignored Range. Trust Accept-Ranges only if it says "bytes".
		p.AcceptRanges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
		if n := resp.ContentLength; n > 0 {
			p.TotalBytes = n
		}
	}
	return p, nil
}

// DownloadSpec is the input to Run.
type DownloadSpec struct {
	URL     string
	Dest    string // absolute path of the final file
	Headers map[string]string
	Conns   int // 0 = use engine default
}

// Run downloads spec.URL to spec.Dest, resuming from a sidecar if one is present.
// It blocks until done, ctx is cancelled (treated as pause: sidecar is flushed),
// or an error occurs. onProgress may be nil.
func (e *Engine) Run(ctx context.Context, spec DownloadSpec, onProgress func(Progress)) (*Probe, error) {
	pr, err := e.ProbeURL(ctx, spec.URL, spec.Headers)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(spec.Dest), 0o755); err != nil {
		return pr, err
	}

	conns := spec.Conns
	if conns <= 0 {
		conns = e.cfg.MaxConns
	}
	// Only split when the server supports ranges and the file is big enough to
	// make extra connections worthwhile.
	multi := pr.AcceptRanges && pr.TotalBytes > e.cfg.MinChunk*2
	if !multi {
		conns = 1
	}

	sc := loadSidecar(spec.Dest)
	switch {
	case sc == nil || !sc.matches(spec.URL, pr):
		sc = newSidecar(spec.URL, pr, conns, e.cfg.MinChunk)
	case multi && sc.Conns != conns:
		// connection count changed since last run — re-split the missing bytes
		sc.replanConns(conns, e.cfg.MinChunk)
	}

	f, err := os.OpenFile(spec.Dest+".part", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return pr, err
	}
	defer f.Close()
	if pr.TotalBytes > 0 {
		if err := f.Truncate(pr.TotalBytes); err != nil {
			return pr, err
		}
	}

	var done atomic.Int64
	done.Store(sc.completedBytes())

	var scMu sync.Mutex

	snapshotConns := func() []ConnProgress {
		scMu.Lock()
		defer scMu.Unlock()
		out := make([]ConnProgress, 0, len(sc.Chunks)+len(sc.Done))
		idx := 0
		// bytes finished under an earlier connection-count plan
		for _, v := range sc.Done {
			out = append(out, ConnProgress{
				Index: idx, Start: v.A, Downloaded: ivLen(v), Total: ivLen(v), Status: "done",
			})
			idx++
		}
		for i := range sc.Chunks {
			c := &sc.Chunks[i]
			st := c.Status
			if st == "" {
				if c.remaining() <= 0 {
					st = "done"
				} else {
					st = "idle"
				}
			}
			total := c.length()
			if total < 0 {
				total = pr.TotalBytes
			}
			out = append(out, ConnProgress{Index: idx, Start: c.Start, Downloaded: c.Done, Total: total, Status: st})
			idx++
		}
		return out
	}

	// progress ticker
	progCtx, progStop := context.WithCancel(context.Background())
	defer progStop()
	if onProgress != nil {
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			last := done.Load()
			lastT := time.Now()
			for {
				select {
				case <-progCtx.Done():
					return
				case now := <-t.C:
					cur := done.Load()
					dt := now.Sub(lastT).Seconds()
					var sp int64
					if dt > 0 {
						sp = int64(float64(cur-last) / dt)
					}
					last, lastT = cur, now
					onProgress(Progress{
						DoneBytes: cur, TotalBytes: pr.TotalBytes, SpeedBps: sp,
						NumConns: conns, Resumable: pr.AcceptRanges, Conns: snapshotConns(),
					})
				}
			}
		}()
	}

	// sidecar flusher (scMu declared above)
	flush := func() {
		scMu.Lock()
		_ = sc.save(spec.Dest)
		scMu.Unlock()
	}
	flushCtx, flushStop := context.WithCancel(context.Background())
	defer flushStop()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-t.C:
				flush()
			}
		}
	}()

	// fetch every not-yet-complete chunk, at most `conns` in flight
	g := newGroup(conns)
	for i := range sc.Chunks {
		c := &sc.Chunks[i]
		if c.remaining() <= 0 {
			continue
		}
		g.go_(func() error {
			return e.fetchChunk(ctx, spec, f, c, &scMu, &done)
		})
	}
	err = g.wait()

	progStop()
	flushStop()
	flush()

	if errors.Is(err, errRangeIgnored) {
		// The server ignored Range and sent the full body for chunk 0. If that
		// covered the whole resource, we're done; if not, the URL is only a
		// fragment (Instagram/Facebook slice URLs) — surface that clearly.
		scMu.Lock()
		got := sc.Chunks[0].Done
		scMu.Unlock()
		if pr.TotalBytes > 0 && got >= pr.TotalBytes {
			err = nil
		} else {
			return pr, fmt.Errorf("this URL only serves a byte-range fragment — use \"Extract video from this page\" instead")
		}
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			return pr, context.Canceled // caller reads this as "paused"
		}
		return pr, err
	}

	if err := f.Close(); err != nil {
		return pr, err
	}
	if err := os.Rename(spec.Dest+".part", spec.Dest); err != nil {
		return pr, err
	}
	_ = os.Remove(sidecarPath(spec.Dest))
	if onProgress != nil {
		final := snapshotConns()
		for i := range final {
			final[i].Status = "done"
			final[i].Downloaded = final[i].Total
		}
		onProgress(Progress{
			DoneBytes: pr.TotalBytes, TotalBytes: pr.TotalBytes, SpeedBps: 0,
			NumConns: conns, Conns: final,
		})
	}
	return pr, nil
}

func (e *Engine) fetchChunk(ctx context.Context, spec DownloadSpec, f *os.File, c *chunk, scMu *sync.Mutex, done *atomic.Int64) error {
	const maxTries = 5
	var lastErr error
	for try := 0; try < maxTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try*try) * 500 * time.Millisecond):
			}
		}
		n, err := e.fetchChunkOnce(ctx, spec, f, c, scMu, done)
		_ = n
		if err == nil {
			setChunkStatus(scMu, c, "done")
			return nil
		}
		if errors.Is(err, errStalled) {
			// A dead/stalled connection — retryable, and c.Done kept the bytes
			// we did get, so the retry resumes from there.
			lastErr = err
			setChunkStatus(scMu, c, "error")
			continue
		}
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			setChunkStatus(scMu, c, "idle")
			return context.Canceled
		}
		if errors.Is(err, errRangeIgnored) {
			// Not retryable: the server does not honour Range. Propagate so Run
			// can decide (chunk 0 already has the whole body).
			setChunkStatus(scMu, c, "done")
			return err
		}
		lastErr = err
		setChunkStatus(scMu, c, "error")
	}
	return fmt.Errorf("chunk %d: %w", c.Index, lastErr)
}

func setChunkStatus(scMu *sync.Mutex, c *chunk, s string) {
	scMu.Lock()
	c.Status = s
	scMu.Unlock()
}

// stallTimeout aborts a chunk read that has gone this long without a byte — a
// half-open connection would otherwise hang until the whole job is cancelled.
// A var (not const) so tests can shorten it.
var stallTimeout = 30 * time.Second

// errStalled marks a chunk that timed out mid-transfer (retryable).
var errStalled = errors.New("connection stalled")

func (e *Engine) fetchChunkOnce(ctx context.Context, spec DownloadSpec, f *os.File, c *chunk, scMu *sync.Mutex, done *atomic.Int64) (int64, error) {
	// A watchdog context: cancelled by the parent (pause/shutdown) OR by us when
	// no data has arrived for stallTimeout.
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	stalled := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRead.Load())) > stallTimeout {
					close(stalled)
					rcancel()
					return
				}
			}
		}
	}()
	wasStalled := func() bool {
		select {
		case <-stalled:
			return true
		default:
			return false
		}
	}

	start := c.Start + c.Done
	setChunkStatus(scMu, c, "connecting")
	r, err := e.req(rctx, http.MethodGet, spec.URL, spec.Headers)
	if err != nil {
		return 0, err
	}
	single := c.Start == 0 && c.End < 0
	if !single {
		r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, c.End))
	}

	resp, err := e.client.Do(r)
	if err != nil {
		if wasStalled() {
			return 0, errStalled
		}
		return 0, err
	}
	defer resp.Body.Close()

	if single && resp.StatusCode != http.StatusOK {
		return 0, statusErr(resp)
	}
	// The server ignored our Range header and sent the whole body (some CDNs —
	// Instagram/Facebook — range via query params instead). If this is the
	// first chunk, absorb the full response from offset 0 and let Run mark the
	// rest done. Any other chunk: bail so Run restarts as a single connection.
	fullBody := !single && resp.StatusCode == http.StatusOK
	if fullBody && start != 0 {
		return 0, errRangeIgnored
	}
	if !single && resp.StatusCode != http.StatusPartialContent && !fullBody {
		return 0, statusErr(resp)
	}

	setChunkStatus(scMu, c, "receiving")
	buf := make([]byte, 128*1024)
	var written int64
	off := start
	limit := int64(-1)
	maxAttempt := int64(-1) // bytes this ranged attempt may still write
	if !single && !fullBody {
		limit = c.length()
		maxAttempt = c.End - start + 1
	}
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			// A compliant 206 never over-delivers, but clamp anyway: one wrong
			// byte past c.End would corrupt the neighbouring chunk's region
			// (fatal for archives/disk images, not just video).
			if maxAttempt >= 0 && written+int64(nr) > maxAttempt {
				nr = int(maxAttempt - written)
			}
			if nr <= 0 {
				return written, nil
			}
			lastRead.Store(time.Now().UnixNano())
			if _, ew := f.WriteAt(buf[:nr], off); ew != nil {
				return written, ew
			}
			off += int64(nr)
			written += int64(nr)
			scMu.Lock()
			c.Done += int64(nr)
			scMu.Unlock()
			done.Add(int64(nr))
		}
		if er == io.EOF {
			if fullBody {
				return written, errRangeIgnored // Run: mark siblings done, we have it all
			}
			// A ranged chunk that hit EOF before its last byte is a truncated
			// response (server dropped the connection cleanly). Marking it "done"
			// here would rename a corrupt .part to the final file. Treat it as a
			// stall so fetchChunk retries and resumes from c.Done.
			if limit >= 0 && c.Done < limit {
				return written, errStalled
			}
			return written, nil
		}
		if er != nil {
			if wasStalled() {
				return written, errStalled
			}
			return written, er
		}
		if limit >= 0 && c.Done >= limit {
			return written, nil
		}
	}
}

// errRangeIgnored: the server returned 200 (full body) for a ranged request.
var errRangeIgnored = errors.New("server ignored Range header")

func filenameFrom(rawurl, contentDisposition string) string {
	if contentDisposition != "" {
		if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
			if fn := params["filename"]; fn != "" {
				return sanitizeName(fn)
			}
		}
	}
	if u, err := url.Parse(rawurl); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return sanitizeName(base)
		}
	}
	return "download"
}

// collapseWS turns any run of whitespace into a single space — for squeezing a
// server error page into one readable line.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// statusErr builds an error from a non-2xx response, appending a snippet of the
// body (CDN 403 pages usually say *why* — bad signature, expired, region).
func statusErr(resp *http.Response) error {
	snip, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if s := collapseWS(string(snip)); s != "" {
		return fmt.Errorf("%s — %s", resp.Status, s)
	}
	return fmt.Errorf("unexpected status %s", resp.Status)
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, string(filepath.Separator), "_")
	s = strings.Trim(s, ".")
	if s == "" {
		return "download"
	}
	return s
}
