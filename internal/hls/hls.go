// Package hls parses M3U8 playlists and assembles the segments they describe
// into a single file. It handles the common non-DRM cases: master playlists with
// multiple variants, media playlists with an EXT-X-MAP init segment, and
// AES-128 whole-segment encryption where the key URI is fetchable.
//
// SAMPLE-AES / SAMPLE-AES-CTR (FairPlay and friends) are detected and refused —
// MacDM does not do DRM.
package hls

import (
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

	"macdm/internal/ratelimit"
	"macdm/internal/store"
	"macdm/internal/subs"
)

// Variant is one entry of a master playlist.
type Variant struct {
	URL        string
	Bandwidth  int
	Resolution string
	Codecs     string
	// AudioGroup is the AUDIO="..." attribute. When set, the variant's own
	// stream is usually video-only and the audio lives in a separate
	// #EXT-X-MEDIA rendition carrying the same GROUP-ID.
	AudioGroup string
	// SubtitleGroup is the SUBTITLES="..." attribute, naming the group of
	// #EXT-X-MEDIA subtitle renditions offered with this variant.
	SubtitleGroup string
	// CaptionGroup is CLOSED-CAPTIONS="...", naming a group of in-band
	// CEA-608/708 caption declarations. The literal NONE means the variant
	// carries none, which is worth knowing: it saves a decode pass.
	CaptionGroup string
}

// Rendition is one #EXT-X-MEDIA entry — an alternative audio (or subtitle)
// track offered alongside the variants.
type Rendition struct {
	Type       string // AUDIO, SUBTITLES, VIDEO, CLOSED-CAPTIONS
	GroupID    string
	Name       string
	Language   string
	URI        string // empty => this rendition is muxed into the variant itself
	Default    bool
	Autoselect bool
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
	// ByteRange addressing (#EXT-X-BYTERANGE): several segments share one URL
	// and are distinguished by offset. Length 0 means "whole resource".
	Offset int64
	Length int64
}

// Playlist is either a master (Variants set) or a media playlist (Segments set).
type Playlist struct {
	IsMaster bool
	Live     bool // media playlist with no #EXT-X-ENDLIST
	Variants []Variant
	Media    []Rendition // #EXT-X-MEDIA entries (master playlists)
	Segments []Segment
	InitURL  string // EXT-X-MAP:URI, if any
	// EXT-X-MAP may itself be a byte range of a larger resource.
	InitOffset int64
	InitLength int64
	Key        *Key // playlist-level key (may be overridden per segment)
}

// ClosedCaptionsFor reports whether v declares in-band CEA-608/708 captions,
// and the language if one is named.
//
// Extracting captions means decoding the whole video (they live in the video
// bitstream, not a track), which costs about a minute per hour of footage — far
// too much to spend on every download when almost none carry them. The manifest
// says so up front, so ask it.
func (p *Playlist) ClosedCaptionsFor(v Variant) (declared bool, lang string) {
	if v.CaptionGroup == "" || strings.EqualFold(v.CaptionGroup, "NONE") {
		return false, ""
	}
	for i := range p.Media {
		r := &p.Media[i]
		if strings.EqualFold(r.Type, "CLOSED-CAPTIONS") && r.GroupID == v.CaptionGroup {
			return true, firstNonEmptyStr(r.Language, r.Name)
		}
	}
	// The group is named but not described. The variant still claims captions.
	return true, ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SubtitlesFor returns the subtitle rendition to download for v, or nil when
// the variant offers none. Same preference order as AudioFor: DEFAULT, then
// AUTOSELECT, then the first with a URI. A rendition without a URI carries no
// separate file (it is in-band CEA-608/708) and is skipped.
func (p *Playlist) SubtitlesFor(v Variant) *Rendition {
	if v.SubtitleGroup == "" {
		return nil
	}
	var first, autoselect *Rendition
	for i := range p.Media {
		r := &p.Media[i]
		if !strings.EqualFold(r.Type, "SUBTITLES") || r.GroupID != v.SubtitleGroup || r.URI == "" {
			continue
		}
		if r.Default {
			return r
		}
		if autoselect == nil && r.Autoselect {
			autoselect = r
		}
		if first == nil {
			first = r
		}
	}
	if autoselect != nil {
		return autoselect
	}
	return first
}

// AudioFor returns the alternative audio rendition that must be downloaded and
// muxed alongside v, or nil when v already carries its own audio.
//
// A variant with AUDIO="grp" is normally video-only. The catch: a rendition in
// that group with NO URI means the audio for it *is* embedded in the variant,
// so fetching nothing is correct — only a rendition with a URI is a separate
// stream. Picks DEFAULT=YES, else AUTOSELECT=YES, else the first with a URI.
func (p *Playlist) AudioFor(v Variant) *Rendition {
	if v.AudioGroup == "" {
		return nil
	}
	var first, autoselect *Rendition
	for i := range p.Media {
		r := &p.Media[i]
		if !strings.EqualFold(r.Type, "AUDIO") || r.GroupID != v.AudioGroup {
			continue
		}
		if r.URI == "" {
			// The group is muxed into the variant — nothing separate to fetch.
			return nil
		}
		if r.Default {
			return r
		}
		if autoselect == nil && r.Autoselect {
			autoselect = r
		}
		if first == nil {
			first = r
		}
	}
	if autoselect != nil {
		return autoselect
	}
	return first
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
	// limiter paces segment transfers. Segments do not go through the download
	// engine, so without this a speed limit would apply to plain files and be
	// ignored by exactly the streams people most want to hold back. nil means
	// no limit.
	limiter *ratelimit.Bucket
}

// SetLimiter shares a transfer ceiling with this client. Pass nil for none.
func (c *Client) SetLimiter(b *ratelimit.Bucket) { c.limiter = b }

func NewClient(h *http.Client, headers map[string]string) *Client {
	if h == nil {
		h = http.DefaultClient
	}
	return &Client{http: h, headers: headers}
}

// get fetches a small resource (a playlist or a 16-byte key) fully into memory.
// Media segments never go through here — see fetchToFile.
func (c *Client) get(ctx context.Context, rawurl string) ([]byte, error) {
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
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// FetchSubtitles downloads every segment of a WebVTT subtitle playlist and
// merges them into one file. Subtitle tracks are tiny (kilobytes), so they are
// fetched sequentially and merged in memory rather than going through the
// worker-pool assembler.
func (c *Client) FetchSubtitles(ctx context.Context, p *Playlist) ([]byte, error) {
	if len(p.Segments) == 0 {
		return nil, fmt.Errorf("subtitle playlist has no segments")
	}
	parts := make([][]byte, 0, len(p.Segments))
	for _, seg := range p.Segments {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		b, err := c.getRange(ctx, seg.URL, seg.Offset, seg.Length)
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
	}
	return subs.MergeVTT(parts), nil
}

// getRange fetches a resource, or one byte range of it when length > 0.
// Used for small pieces (the EXT-X-MAP init segment, subtitle segments);
// media segments stream to disk via fetchToFile.
func (c *Client) getRange(ctx context.Context, rawurl string, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return c.get(ctx, rawurl)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("GET %s: %s (byte range not honoured)", rawurl, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, length))
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
		pendingLen  int64
		pendingOff  int64
		lastEnd     int64 // end of the previous EXT-X-BYTERANGE segment
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

		case strings.HasPrefix(ln, "#EXT-X-MEDIA:"):
			attrs := parseAttrs(after(ln, ":"))
			r := Rendition{
				Type:       strings.ToUpper(attrs["TYPE"]),
				GroupID:    attrs["GROUP-ID"],
				Name:       attrs["NAME"],
				Language:   attrs["LANGUAGE"],
				URI:        attrs["URI"],
				Default:    strings.EqualFold(attrs["DEFAULT"], "YES"),
				Autoselect: strings.EqualFold(attrs["AUTOSELECT"], "YES"),
			}
			if r.URI != "" {
				r.URI = resolve(r.URI)
			}
			// Deliberately does not set IsMaster: the variant URI lines already
			// do, and trusting a stray tag would misclassify a non-compliant
			// media playlist as a master and refuse to download it.
			p.Media = append(p.Media, r)

		case strings.HasPrefix(ln, "#EXT-X-STREAM-INF:"):
			attrs := parseAttrs(after(ln, ":"))
			pendingVar = &Variant{
				Bandwidth:     atoiSafe(attrs["BANDWIDTH"]),
				Resolution:    attrs["RESOLUTION"],
				Codecs:        strings.Trim(attrs["CODECS"], `"`),
				AudioGroup:    attrs["AUDIO"],
				SubtitleGroup: attrs["SUBTITLES"],
				CaptionGroup:  attrs["CLOSED-CAPTIONS"],
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
			if br := strings.Trim(attrs["BYTERANGE"], `"`); br != "" {
				p.InitLength, p.InitOffset = parseByteRange(br, 0)
			}

		case ln == "#EXT-X-ENDLIST":
			sawEndList = true

		case strings.HasPrefix(ln, "#EXTINF:"):
			v := after(ln, ":")
			v = strings.SplitN(v, ",", 2)[0]
			pendingDur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)

		case strings.HasPrefix(ln, "#EXT-X-BYTERANGE:"):
			// "<length>[@<offset>]". With the offset omitted the segment starts
			// where the previous one in the same resource ended.
			pendingLen, pendingOff = parseByteRange(after(ln, ":"), lastEnd)

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
			s := Segment{URL: resolve(ln), Duration: pendingDur, Key: curKey, Seq: seq,
				Offset: pendingOff, Length: pendingLen}
			p.Segments = append(p.Segments, s)
			if pendingLen > 0 {
				lastEnd = pendingOff + pendingLen
			} else {
				lastEnd = 0 // a whole-resource segment resets the running offset
			}
			pendingDur, pendingLen, pendingOff = 0, 0, 0
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
				fn := filepath.Join(opt.Dir, fmt.Sprintf("seg-%06d", i))
				err := c.fetchSegment(ctx, seg, fn, keyCache, func(n int) {
					doneBytes.Add(int64(n))
					mu.Lock()
					states[w].Bytes += int64(n)
					mu.Unlock()
					if now := time.Now(); now.Sub(lastEmit) > 200*time.Millisecond {
						lastEmit = now
						emit()
					}
				})
				if err != nil {
					setErr(fmt.Errorf("segment %d: %w", i, err))
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
		initData, err := c.getRange(ctx, p.InitURL, p.InitOffset, p.InitLength)
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
	// Flush and surface a write error (disk full) instead of returning a
	// truncated file that the muxer would then choke on.
	return out.Close()
}

// fetchSegment downloads one segment to dst, streaming straight to disk so the
// assembler never holds a whole segment (let alone conns×segments) in memory.
// AES-128 segments are fetched to a scratch file and decrypted into dst.
//
// The final file only ever appears via an atomic rename, so dst existing means
// dst is complete: a retry of a partly-assembled stream skips what it already
// has instead of re-downloading the whole thing.
func (c *Client) fetchSegment(ctx context.Context, seg Segment, dst string, cache *sync.Map, onBytes func(int)) error {
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		if onBytes != nil {
			onBytes(int(fi.Size())) // keep byte accounting honest across a resume
		}
		return nil
	}
	tmp := dst + ".part"
	if seg.Key == nil || strings.ToUpper(seg.Key.Method) != "AES-128" {
		if err := c.fetchToFile(ctx, seg.URL, tmp, seg, onBytes); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	}
	enc := dst + ".enc"
	if err := c.fetchToFile(ctx, seg.URL, enc, seg, onBytes); err != nil {
		return err
	}
	defer os.Remove(enc)
	if err := c.decryptFileAES128(ctx, seg, enc, tmp, cache); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// fetchToFile streams rawurl into dst, reporting each chunk via onBytes.
func (c *Client) fetchToFile(ctx context.Context, rawurl, dst string, seg Segment, onBytes func(int)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	// #EXT-X-BYTERANGE: several segments share one URL and are told apart by
	// offset, so this segment is a Range request rather than a whole GET.
	ranged := seg.Length > 0
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.Offset, seg.Offset+seg.Length-1))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case ranged && resp.StatusCode == http.StatusPartialContent:
	case ranged && resp.StatusCode == http.StatusOK:
		// The server ignored Range and sent the whole file. Writing all of it
		// would repeat the entire resource for every segment.
		return fmt.Errorf("GET %s: server ignored the Range header for a byte-range segment", rawurl)
	case !ranged && resp.StatusCode == http.StatusOK:
	default:
		return fmt.Errorf("GET %s: %s", rawurl, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			if _, werr := f.Write(tmp[:n]); werr != nil {
				return werr
			}
			if onBytes != nil {
				onBytes(n)
			}
			if werr := c.limiter.Wait(ctx, n); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return f.Close()
}

func (c *Client) segKey(ctx context.Context, seg Segment, cache *sync.Map) ([]byte, error) {
	if v, ok := cache.Load(seg.Key.URI); ok {
		return v.([]byte), nil
	}
	key, err := c.get(ctx, seg.Key.URI)
	if err != nil {
		return nil, fmt.Errorf("fetch key: %w", err)
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("key is %d bytes, want 16", len(key))
	}
	cache.Store(seg.Key.URI, key)
	return key, nil
}

// decryptFileAES128 CBC-decrypts src into dst in 1 MiB block-aligned chunks and
// strips PKCS#7 padding from the last block.
func (c *Client) decryptFileAES128(ctx context.Context, seg Segment, src, dst string, cache *sync.Map) error {
	key, err := c.segKey(ctx, seg, cache)
	if err != nil {
		return err
	}
	iv := seg.Key.IV
	if len(iv) != 16 {
		iv = make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(seg.Seq))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size == 0 || size%16 != 0 {
		return fmt.Errorf("ciphertext not a multiple of block size")
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	mode := cipher.NewCBCDecrypter(block, iv)
	const chunk = 1 << 20 // block-aligned
	buf := make([]byte, chunk)
	var read int64
	for read < size {
		want := chunk
		if rem := size - read; rem < int64(chunk) {
			want = int(rem)
		}
		if _, err := io.ReadFull(in, buf[:want]); err != nil {
			return err
		}
		mode.CryptBlocks(buf[:want], buf[:want])
		read += int64(want)
		plain := buf[:want]
		if read == size { // final block: drop PKCS#7 padding
			if pad := int(plain[want-1]); pad > 0 && pad <= 16 && pad <= want {
				plain = plain[:want-pad]
			}
		}
		if _, err := out.Write(plain); err != nil {
			return err
		}
	}
	return out.Close()
}

// parseByteRange reads an EXT-X-BYTERANGE value, "<length>[@<offset>]".
// A missing offset continues from prevEnd, per the HLS spec.
func parseByteRange(v string, prevEnd int64) (length, offset int64) {
	lenStr, offStr, hasOff := strings.Cut(strings.TrimSpace(v), "@")
	length, err := strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil || length <= 0 {
		return 0, 0
	}
	if !hasOff {
		return length, prevEnd
	}
	offset, err = strconv.ParseInt(strings.TrimSpace(offStr), 10, 64)
	if err != nil || offset < 0 {
		return length, prevEnd
	}
	return length, offset
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
