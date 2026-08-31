package engine

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// chunk is one contiguous byte range of the target file. End is inclusive.
// End == -1 is the sentinel for "single stream to EOF" when the server does not
// support ranges; in that case Done still tracks bytes written so progress works,
// but the download cannot resume and restarts from zero.
type chunk struct {
	Index int   `json:"i"`
	Start int64 `json:"s"`
	End   int64 `json:"e"`
	Done  int64 `json:"d"`
	// Status is runtime only (not persisted): idle|connecting|receiving|done|error.
	Status string `json:"-"`
}

func (c *chunk) length() int64 {
	if c.End < 0 {
		return -1
	}
	return c.End - c.Start + 1
}

func (c *chunk) remaining() int64 {
	if c.End < 0 {
		return 1 // unknown; always "has work" until the stream ends
	}
	r := c.length() - c.Done
	if r < 0 {
		return 0
	}
	return r
}

// iv is an inclusive byte interval [A,B].
type iv struct {
	A int64 `json:"a"`
	B int64 `json:"b"`
}

type sidecar struct {
	URL        string  `json:"url"`
	Identity   string  `json:"identity,omitempty"`
	ETag       string  `json:"etag,omitempty"`
	TotalBytes int64   `json:"total_bytes"`
	Conns      int     `json:"conns"`          // planned connection count
	Done       []iv    `json:"done,omitempty"` // completed ranges from earlier plans
	Chunks     []chunk `json:"chunks"`
}

func sidecarPath(dest string) string { return dest + ".macdm" }

func loadSidecar(dest string) *sidecar {
	data, err := os.ReadFile(sidecarPath(dest))
	if err != nil {
		return nil
	}
	var sc sidecar
	if json.Unmarshal(data, &sc) != nil {
		return nil
	}
	return &sc
}

func (sc *sidecar) save(dest string) error {
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := sidecarPath(dest) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, sidecarPath(dest))
}

// matches reports whether a saved sidecar still describes the same resource, so
// resuming onto the existing .part file is safe.
func (sc *sidecar) matches(url, identity string, p *Probe) bool {
	if sc.TotalBytes != p.TotalBytes {
		return false
	}
	// With an identity the URL is expected to differ between runs — that is the
	// whole point. Without one, the URL is all we have to go on.
	if identity != "" || sc.Identity != "" {
		if sc.Identity != identity {
			return false
		}
	} else if sc.URL != url {
		return false
	}
	if sc.ETag != "" && p.ETag != "" && sc.ETag != p.ETag {
		return false
	}
	return true
}

func ivLen(v iv) int64 { return v.B - v.A + 1 }

func (sc *sidecar) completedBytes() int64 {
	var n int64
	for _, v := range sc.Done {
		n += ivLen(v)
	}
	for i := range sc.Chunks {
		n += sc.Chunks[i].Done
	}
	return n
}

func newSidecar(url, identity string, p *Probe, conns int, minChunk int64) *sidecar {
	sc := &sidecar{URL: url, Identity: identity, ETag: p.ETag, TotalBytes: p.TotalBytes, Conns: conns}

	if !p.AcceptRanges || p.TotalBytes <= 0 {
		sc.Chunks = []chunk{{Index: 0, Start: 0, End: -1}}
		return sc
	}
	sc.Chunks = splitRanges([]iv{{0, p.TotalBytes - 1}}, conns, minChunk)
	sc.reserveSteal(conns)
	return sc
}

// reserveSteal grows the Chunks backing array so work-stealing can append
// sub-chunks without reallocating (which would invalidate the *chunk pointers
// the workers hold).
func (sc *sidecar) reserveSteal(conns int) {
	want := len(sc.Chunks) + conns*3
	if cap(sc.Chunks) >= want {
		return
	}
	g := make([]chunk, len(sc.Chunks), want)
	copy(g, sc.Chunks)
	sc.Chunks = g
}

// replanConns folds the current chunks' progress into Done and re-splits the
// still-missing byte ranges into `conns` fresh chunks. Called when the user
// changes the connection count mid-download.
func (sc *sidecar) replanConns(conns int, minChunk int64) {
	if sc.TotalBytes <= 0 || len(sc.Chunks) == 0 || sc.Chunks[0].End < 0 {
		return // single-stream download: nothing to re-split
	}
	for _, c := range sc.Chunks {
		if c.Done > 0 {
			sc.Done = append(sc.Done, iv{c.Start, c.Start + c.Done - 1})
		}
	}
	sc.Done = mergeIv(sc.Done)
	missing := complementIv(sc.Done, sc.TotalBytes)
	sc.Chunks = splitRanges(missing, conns, minChunk)
	sc.Conns = conns
	sc.reserveSteal(conns)
}

// splitRanges divides the given byte intervals into up to `conns` contiguous
// chunks, never spanning a gap between intervals, honouring minChunk.
func splitRanges(ranges []iv, conns int, minChunk int64) []chunk {
	var total int64
	for _, r := range ranges {
		total += ivLen(r)
	}
	if total <= 0 {
		return nil
	}
	n := int64(conns)
	if n < 1 {
		n = 1
	}
	if maxBySize := total / max64(minChunk, 1); maxBySize < n {
		n = maxBySize
	}
	if n < 1 {
		n = 1
	}
	target := total / n // desired bytes per chunk

	var out []chunk
	idx := 0
	for _, r := range ranges {
		start := r.A
		for start <= r.B {
			end := start + target - 1
			if end > r.B || int64(len(out)) == n-1 {
				end = r.B
			}
			// avoid a tiny trailing sliver
			if r.B-end > 0 && r.B-end < minChunk/2 {
				end = r.B
			}
			out = append(out, chunk{Index: idx, Start: start, End: end})
			idx++
			start = end + 1
		}
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func mergeIv(in []iv) []iv {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return in[i].A < in[j].A })
	out := []iv{in[0]}
	for _, v := range in[1:] {
		last := &out[len(out)-1]
		if v.A <= last.B+1 {
			if v.B > last.B {
				last.B = v.B
			}
		} else {
			out = append(out, v)
		}
	}
	return out
}

func complementIv(done []iv, total int64) []iv {
	var out []iv
	var cursor int64
	for _, v := range done {
		if v.A > cursor {
			out = append(out, iv{cursor, v.A - 1})
		}
		if v.B+1 > cursor {
			cursor = v.B + 1
		}
	}
	if cursor <= total-1 {
		out = append(out, iv{cursor, total - 1})
	}
	return out
}

// group is a minimal bounded-concurrency runner: at most n functions in flight,
// first non-nil error is remembered and returned by wait.
type group struct {
	sem chan struct{}
	wg  sync.WaitGroup
	mu  sync.Mutex
	err error
}

func newGroup(n int) *group {
	if n < 1 {
		n = 1
	}
	return &group{sem: make(chan struct{}, n)}
}

func (g *group) go_(fn func() error) {
	g.wg.Add(1)
	g.sem <- struct{}{}
	go func() {
		defer g.wg.Done()
		defer func() { <-g.sem }()
		if err := fn(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
			}
			g.mu.Unlock()
		}
	}()
}

func (g *group) wait() error {
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}
