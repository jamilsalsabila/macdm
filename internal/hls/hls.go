// Package hls parses M3U8 playlists and assembles the segments they describe
// into a single file. It handles the common non-DRM cases: master playlists with
// multiple variants, media playlists with an EXT-X-MAP init segment, and
// AES-128 whole-segment encryption where the key URI is fetchable.
//
// SAMPLE-AES / SAMPLE-AES-CTR (FairPlay and friends) are detected and refused —
// MacDM does not do DRM.
package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"macdm/internal/store"
)

// Variant is one entry of a master playlist.
type Variant struct {
	URL        string
	Bandwidth  int
	Resolution string
	Codecs     string
}

// Key describes segment encryption.
type Key struct {
	Method string // NONE, AES-128, SAMPLE-AES (=> DRM)
	URI    string
	IV     []byte
}

// DRM reports whether this key is a scheme MacDM will not attempt.
func (k *Key) DRM() bool {
	if k == nil {
		return false
	}
	m := strings.ToUpper(k.Method)
	return strings.HasPrefix(m, "SAMPLE-AES")
}

// Segment is one media chunk.
type Segment struct {
	URL      string
	Duration float64
	Key      *Key
	Seq      int
}

// Playlist is either a master (Variants set) or a media playlist (Segments set).
type Playlist struct {
	IsMaster bool
	Live     bool // media playlist with no #EXT-X-ENDLIST
	Variants []Variant
	Segments []Segment
	InitURL  string // EXT-X-MAP:URI, if any
	Key      *Key   // playlist-level key (may be overridden per segment)
}

// HasDRM reports whether any key in the playlist is a DRM scheme.
func (p *Playlist) HasDRM() bool {
	if p.Key.DRM() {
		return true
	}
	for i := range p.Segments {
		if p.Segments[i].Key.DRM() {
			return true
		}
	}
	return false
}

// VariantChoices renders the master playlist's variants as user-facing quality
// options. Choice.ID is the variant's media-playlist URL, so the caller fetches
// exactly that rendition.
func (p *Playlist) VariantChoices() []store.FormatChoice {
	out := make([]store.FormatChoice, 0, len(p.Variants))
	for _, v := range p.Variants {
		h := 0
		if i := strings.LastIndexByte(v.Resolution, 'x'); i >= 0 {
			h = atoiSafe(v.Resolution[i+1:])
		}
		label := v.Resolution
		if h > 0 {
			label = strconv.Itoa(h) + "p"
		}
		if v.Bandwidth > 0 {
			label += fmt.Sprintf(" (%.1f Mbps)", float64(v.Bandwidth)/1e6)
		}
		out = append(out, store.FormatChoice{
			ID: v.URL, Label: label, Height: h, Ext: "mp4", Kind: "video+audio",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	return out
}

// BestVariant returns the highest-bandwidth variant.
func (p *Playlist) BestVariant() (Variant, bool) {
	if len(p.Variants) == 0 {
		return Variant{}, false
	}
	v := append([]Variant(nil), p.Variants...)
	sort.Slice(v, func(i, j int) bool { return v[i].Bandwidth > v[j].Bandwidth })
	return v[0], true
}

// Client fetches and parses playlists.
type Client struct {
	http    *http.Client
	headers map[string]string
}

func NewClient(h *http.Client, headers map[string]string) *Client {
	if h == nil {
		h = http.DefaultClient
	}
	return &Client{http: h, headers: headers}
}

func (c *Client) get(ctx context.Context, rawurl string) ([]byte, error) {
	return c.getStreamed(ctx, rawurl, nil)
}

// getStreamed fetches rawurl, calling onBytes(n) as each chunk arrives so a
// caller can report smooth progress instead of one jump per whole segment.
func (c *Client) getStreamed(ctx context.Context, rawurl string, onBytes func(int)) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawurl, resp.Status)
	}
	var buf bytes.Buffer
	rdr := io.LimitReader(resp.Body, 64<<20)
	tmp := make([]byte, 64*1024)
	for {
		n, rerr := rdr.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if onBytes != nil {
				onBytes(n)
			}
		}
		if rerr == io.EOF {
			return buf.Bytes(), nil
		}
		if rerr != nil {
			return buf.Bytes(), rerr
		}
	}
}

// Parse fetches rawurl and returns the playlist it describes.
func (c *Client) Parse(ctx context.Context, rawurl string) (*Playlist, error) {
	body, err := c.get(ctx, rawurl)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(rawurl)
	return parse(string(body), base)
}

func parse(text string, base *url.URL) (*Playlist, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "#EXTM3U") {
		return nil, fmt.Errorf("not an m3u8 playlist")
	}

	p := &Playlist{}
	var (
		pendingDur  float64
		pendingVar  *Variant
		curKey      *Key
		seq         int
		startSeqSet bool
		sawEndList  bool
	)
	resolve := func(ref string) string {
		if u, err := url.Parse(strings.TrimSpace(ref)); err == nil {
			return base.ResolveReference(u).String()
		}
		return ref
	}

	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "" || (strings.HasPrefix(ln, "#") && !strings.HasPrefix(ln, "#EXT")):
			continue

		case strings.HasPrefix(ln, "#EXT-X-MEDIA-SEQUENCE:"):
			seq = atoiSafe(after(ln, ":"))
			startSeqSet = true

		case strings.HasPrefix(ln, "#EXT-X-STREAM-INF:"):
			attrs := parseAttrs(after(ln, ":"))
			pendingVar = &Variant{
				Bandwidth:  atoiSafe(attrs["BANDWIDTH"]),
				Resolution: attrs["RESOLUTION"],
				Codecs:     strings.Trim(attrs["CODECS"], `"`),
			}

		case strings.HasPrefix(ln, "#EXT-X-KEY:"):
			attrs := parseAttrs(after(ln, ":"))
			k := &Key{Method: strings.ToUpper(attrs["METHOD"]), URI: strings.Trim(attrs["URI"], `"`)}
			if k.URI != "" {
				k.URI = resolve(k.URI)
			}
			if ivs := strings.TrimPrefix(strings.ToLower(attrs["IV"]), "0x"); ivs != "" {
				k.IV, _ = hex.DecodeString(ivs)
			}
			if k.Method == "NONE" {
				k = nil
			}
			curKey = k
			if p.Key == nil && len(p.Segments) == 0 {
				p.Key = k
			}

		case strings.HasPrefix(ln, "#EXT-X-MAP:"):
			attrs := parseAttrs(after(ln, ":"))
			if u := strings.Trim(attrs["URI"], `"`); u != "" {
				p.InitURL = resolve(u)
			}

		case ln == "#EXT-X-ENDLIST":
			sawEndList = true

		case strings.HasPrefix(ln, "#EXTINF:"):
			v := after(ln, ":")
			v = strings.SplitN(v, ",", 2)[0]
			pendingDur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)

		case strings.HasPrefix(ln, "#EXT"):
			// other tags: ignore

		default: // a URI line
			if pendingVar != nil {
				pendingVar.URL = resolve(ln)
				p.Variants = append(p.Variants, *pendingVar)
				p.IsMaster = true
				pendingVar = nil
				continue
			}
			s := Segment{URL: resolve(ln), Duration: pendingDur, Key: curKey, Seq: seq}
			p.Segments = append(p.Segments, s)
			pendingDur = 0
			seq++
		}
	}
	_ = startSeqSet
	if !p.IsMaster && len(p.Segments) == 0 {
		return nil, fmt.Errorf("playlist has no segments")
	}
	if !p.IsMaster && !sawEndList {
		p.Live = true
	}
	return p, nil
}

// AssembleOptions configures a media-playlist download.
type AssembleOptions struct {
	Dir     string // working directory for segment files
	OutFile string // path of the assembled (still un-remuxed) output
	Conns   int
}

// WorkerState is one connection's live status, for the IDM-style table.
type WorkerState struct {
	ID      int
	Segment int    // segment index currently being fetched (-1 = idle)
	Status  string // connecting | receiving | idle | done
	Bytes   int64  // cumulative bytes this worker has fetched
}

// Progress is reported during assembly.
type Progress struct {
	Segment   int // segments completed
	Total     int
	DoneBytes int64
	Workers   []WorkerState
}

// Assemble downloads every segment of a media playlist using a fixed pool of
// opt.Conns worker connections, decrypts AES-128 segments, and concatenates
// them in order into opt.OutFile.
func (c *Client) Assemble(ctx context.Context, p *Playlist, opt AssembleOptions, onProgress func(Progress)) error {
	if p.HasDRM() {
		return fmt.Errorf("playlist is DRM-protected (SAMPLE-AES)")
	}
	if opt.Conns <= 0 {
		opt.Conns = 6
	}
	if opt.Conns > len(p.Segments) && len(p.Segments) > 0 {
		opt.Conns = len(p.Segments)
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return err
	}

	parts := make([]string, len(p.Segments))
	var done, doneBytes atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	keyCache := &sync.Map{}

	states := make([]WorkerState, opt.Conns)
	for i := range states {
		states[i] = WorkerState{ID: i, Segment: -1, Status: "idle"}
	}
	var emitMu sync.Mutex // serialise onProgress calls across workers
	emit := func() {
		if onProgress == nil {
			return
		}
		mu.Lock()
		ws := append([]WorkerState(nil), states...)
		mu.Unlock()
		emitMu.Lock()
		onProgress(Progress{
			Segment: int(done.Load()), Total: len(p.Segments),
			DoneBytes: doneBytes.Load(), Workers: ws,
		})
		emitMu.Unlock()
	}
	setErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		mu.Unlock()
	}

	jobs := make(chan int)
	for w := 0; w < opt.Conns; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				states[w].Segment, states[w].Status = i, "connecting"
				mu.Unlock()
				emit()

				seg := p.Segments[i]
				mu.Lock()
				states[w].Status = "receiving"
				mu.Unlock()
				emit()
				var lastEmit time.Time
				data, err := c.getStreamed(ctx, seg.URL, func(n int) {
					doneBytes.Add(int64(n))
					mu.Lock()
					states[w].Bytes += int64(n)
					mu.Unlock()
					if now := time.Now(); now.Sub(lastEmit) > 200*time.Millisecond {
						lastEmit = now
						emit()
					}
				})
				if err == nil && seg.Key != nil && strings.ToUpper(seg.Key.Method) == "AES-128" {
					data, err = c.decryptAES128(ctx, seg, keyCache)
				}
				if err != nil {
					setErr(fmt.Errorf("segment %d: %w", i, err))
					return
				}
				fn := filepath.Join(opt.Dir, fmt.Sprintf("seg-%06d", i))
				if err := os.WriteFile(fn, data, 0o644); err != nil {
					setErr(err)
					return
				}
				parts[i] = fn
				done.Add(1)
				mu.Lock()
				states[w].Segment, states[w].Status = -1, "idle"
				mu.Unlock()
				emit()
			}
		}()
	}
	for i := range p.Segments {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	out, err := os.Create(opt.OutFile)
	if err != nil {
		return err
	}
	defer out.Close()

	if p.InitURL != "" {
		initData, err := c.get(ctx, p.InitURL)
		if err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
		if _, err := out.Write(initData); err != nil {
			return err
		}
	}
	for _, fn := range parts {
		f, err := os.Open(fn)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) decryptAES128(ctx context.Context, seg Segment, cache *sync.Map) ([]byte, error) {
	ct, err := c.get(ctx, seg.URL)
	if err != nil {
		return nil, err
	}
	var key []byte
	if v, ok := cache.Load(seg.Key.URI); ok {
		key = v.([]byte)
	} else {
		key, err = c.get(ctx, seg.Key.URI)
		if err != nil {
			return nil, fmt.Errorf("fetch key: %w", err)
		}
		if len(key) != 16 {
			return nil, fmt.Errorf("key is %d bytes, want 16", len(key))
		}
		cache.Store(seg.Key.URI, key)
	}
	iv := seg.Key.IV
	if len(iv) != 16 {
		iv = make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(seg.Seq))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct)%16 != 0 {
		return nil, fmt.Errorf("ciphertext not a multiple of block size")
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	// strip PKCS#7 padding
	if n := len(pt); n > 0 {
		pad := int(pt[n-1])
		if pad > 0 && pad <= 16 && pad <= n {
			pt = pt[:n-pad]
		}
	}
	return pt, nil
}

// --- tiny attribute-list parser for #EXT-X-* lines ---

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	var key, val strings.Builder
	inKey, inQuote := true, false
	flush := func() {
		if key.Len() > 0 {
			out[strings.ToUpper(strings.TrimSpace(key.String()))] = strings.TrimSpace(val.String())
		}
		key.Reset()
		val.Reset()
		inKey = true
	}
	for _, r := range s {
		switch {
		case inKey && r == '=':
			inKey = false
		case r == '"':
			inQuote = !inQuote
			val.WriteRune(r)
		case r == ',' && !inQuote:
			flush()
		case inKey:
			key.WriteRune(r)
		default:
			val.WriteRune(r)
		}
	}
	flush()
	// unquote
	for k, v := range out {
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

func after(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[i+len(sep):]
	}
	return ""
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
