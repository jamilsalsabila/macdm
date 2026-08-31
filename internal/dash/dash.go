// Package dash parses an MPD manifest far enough to download a static (VOD)
// presentation: it picks the best video Representation and the best audio
// Representation, expands their SegmentTemplate (with $Number$/$Time$ and an
// optional SegmentTimeline) into segment URLs, downloads them, and concatenates
// each track's init + media segments into one file per track.
//
// ContentProtection (Widevine/PlayReady/cenc) is detected and refused.
// Live manifests (type="dynamic") are refused too.
package dash

import (
	"context"
	"encoding/xml"
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

type mpd struct {
	XMLName                   xml.Name `xml:"MPD"`
	Type                      string   `xml:"type,attr"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr"`
	Periods                   []period `xml:"Period"`
	BaseURL                   string   `xml:"BaseURL"`
}

type period struct {
	BaseURL        string          `xml:"BaseURL"`
	AdaptationSets []adaptationSet `xml:"AdaptationSet"`
}

type adaptationSet struct {
	MimeType          string           `xml:"mimeType,attr"`
	ContentType       string           `xml:"contentType,attr"`
	BaseURL           string           `xml:"BaseURL"`
	SegmentTemplate   *segmentTemplate `xml:"SegmentTemplate"`
	ContentProtection []struct {
		SchemeIdURI string `xml:"schemeIdUri,attr"`
	} `xml:"ContentProtection"`
	Representations []representation `xml:"Representation"`
}

type representation struct {
	ID                string           `xml:"id,attr"`
	Bandwidth         int              `xml:"bandwidth,attr"`
	Width             int              `xml:"width,attr"`
	Height            int              `xml:"height,attr"`
	Codecs            string           `xml:"codecs,attr"`
	MimeType          string           `xml:"mimeType,attr"`
	BaseURL           string           `xml:"BaseURL"`
	SegmentTemplate   *segmentTemplate `xml:"SegmentTemplate"`
	ContentProtection []struct {
		SchemeIdURI string `xml:"schemeIdUri,attr"`
	} `xml:"ContentProtection"`
}

type segmentTemplate struct {
	Media          string           `xml:"media,attr"`
	Initialization string           `xml:"initialization,attr"`
	StartNumber    *int             `xml:"startNumber,attr"`
	Timescale      int              `xml:"timescale,attr"`
	Duration       int              `xml:"duration,attr"`
	Timeline       *segmentTimeline `xml:"SegmentTimeline"`
}

type segmentTimeline struct {
	S []struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R int    `xml:"r,attr"`
	} `xml:"S"`
}

// Track is one resolved, downloadable stream (video or audio).
type Track struct {
	Kind      string // "video" or "audio"
	InitURL   string
	Segments  []string
	Codecs    string
	Height    int
	Bandwidth int
}

// Manifest is the useful result of parsing an MPD.
type Manifest struct {
	Video *Track
	Audio *Track
}

// Client fetches and parses.
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

// get fetches a small resource (the MPD, an init segment) fully into memory.
// Media segments never go through here — see fetchToFile.
func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// fetchSegment downloads one segment, publishing dst only via an atomic rename
// so dst existing means dst is complete — a retry of a partly-assembled track
// skips what it already has instead of re-fetching everything.
func (c *Client) fetchSegment(ctx context.Context, u, dst string, onBytes func(int)) error {
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		if onBytes != nil {
			onBytes(int(fi.Size())) // keep byte accounting honest across a resume
		}
		return nil
	}
	tmp := dst + ".part"
	if err := c.fetchToFile(ctx, u, tmp, onBytes); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// fetchToFile streams u straight into dst (reporting each chunk via onBytes) so
// assembly never holds a whole segment in memory — a DASH on-demand "segment"
// can be an entire multi-GB track file.
func (c *Client) fetchToFile(ctx context.Context, u, dst string, onBytes func(int)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
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

// Parse fetches the MPD at rawurl and resolves the best video+audio tracks.
func (c *Client) Parse(ctx context.Context, rawurl string) (*Manifest, error) {
	return c.ParseQuality(ctx, rawurl, 0)
}

// ListRepresentations returns the MPD's video renditions as quality choices.
// Choice.ID is "hNNN" (the target height) which ParseQuality then matches.
func (c *Client) ListRepresentations(ctx context.Context, rawurl string) ([]store.FormatChoice, error) {
	body, err := c.get(ctx, rawurl)
	if err != nil {
		return nil, err
	}
	var m mpd
	if err := xml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse mpd: %w", err)
	}
	if len(m.Periods) == 0 {
		return nil, fmt.Errorf("mpd has no periods")
	}
	seen := map[int]store.FormatChoice{}
	for _, as := range m.Periods[0].AdaptationSets {
		for _, rep := range as.Representations {
			if trackKind(firstNonEmpty(as.MimeType, rep.MimeType), as.ContentType) != "video" || rep.Height == 0 {
				continue
			}
			c := store.FormatChoice{
				ID: fmt.Sprintf("h%d", rep.Height), Label: fmt.Sprintf("%dp", rep.Height),
				Height: rep.Height, Ext: "mp4", Kind: "video+audio",
			}
			if cur, ok := seen[rep.Height]; !ok || rep.Bandwidth > int(cur.SizeBytes) {
				c.SizeBytes = int64(rep.Bandwidth)
				seen[rep.Height] = c
			}
		}
	}
	out := make([]store.FormatChoice, 0, len(seen))
	for _, v := range seen {
		v.SizeBytes = 0
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	return out, nil
}

// ParseQuality is Parse with a preferred max video height (0 = best available).
func (c *Client) ParseQuality(ctx context.Context, rawurl string, preferHeight int) (*Manifest, error) {
	body, err := c.get(ctx, rawurl)
	if err != nil {
		return nil, err
	}
	var m mpd
	if err := xml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse mpd: %w", err)
	}
	if strings.EqualFold(m.Type, "dynamic") {
		return nil, fmt.Errorf("live (dynamic) DASH is not supported")
	}
	if len(m.Periods) == 0 {
		return nil, fmt.Errorf("mpd has no periods")
	}
	mpdDuration.Store(parseISODurationMS(m.MediaPresentationDuration))

	base, _ := url.Parse(rawurl)
	base = resolveBase(base, m.BaseURL)

	out := &Manifest{}
	p := m.Periods[0]
	pbase := resolveBase(base, p.BaseURL)

	for _, as := range p.AdaptationSets {
		if hasProtection(as.ContentProtection) {
			return nil, fmt.Errorf("stream is DRM-protected (ContentProtection present)")
		}
		asbase := resolveBase(pbase, as.BaseURL)

		// choose the best representation in this set. Default: highest bandwidth.
		// With preferHeight: the tallest rep still <= preferHeight wins, else the
		// shortest rep above it.
		reps := append([]representation(nil), as.Representations...)
		sort.Slice(reps, func(i, j int) bool { return reps[i].Bandwidth > reps[j].Bandwidth })
		if preferHeight > 0 {
			sort.SliceStable(reps, func(i, j int) bool {
				hi, hj := reps[i].Height, reps[j].Height
				li, lj := hi <= preferHeight && hi > 0, hj <= preferHeight && hj > 0
				if li != lj {
					return li // reps within budget first
				}
				if li { // both within budget: taller first
					return hi > hj
				}
				return hi < hj // both above budget: shorter first
			})
		}
		for _, rep := range reps {
			// mimeType/contentType may live on the AdaptationSet or on the
			// Representation; check both.
			kind := trackKind(firstNonEmpty(as.MimeType, rep.MimeType), as.ContentType)
			if hasProtection(rep.ContentProtection) {
				return nil, fmt.Errorf("stream is DRM-protected")
			}
			st := rep.SegmentTemplate
			if st == nil {
				st = as.SegmentTemplate
			}

			var t *Track
			switch {
			case st != nil:
				rbase := resolveBase(asbase, rep.BaseURL)
				var err error
				if t, err = expandTemplate(rbase, rep, st); err != nil {
					return nil, err
				}
			case rep.BaseURL != "":
				// isoff-on-demand profile: the whole track is a single file
				// referenced by BaseURL (SegmentBase byte-range indexing is
				// ignored — we just fetch the entire file and let ffmpeg sort
				// out the container).
				full := resolveBase(asbase, rep.BaseURL).String()
				t = &Track{
					Segments:  []string{full},
					Codecs:    rep.Codecs,
					Height:    rep.Height,
					Bandwidth: rep.Bandwidth,
				}
			default:
				continue // SegmentList not supported yet
			}
			t.Kind = kind
			switch kind {
			case "video":
				if out.Video == nil {
					out.Video = t
				}
			case "audio":
				if out.Audio == nil {
					out.Audio = t
				}
			}
			break
		}
	}
	if out.Video == nil && out.Audio == nil {
		return nil, fmt.Errorf("no downloadable tracks (unsupported segment addressing?)")
	}
	return out, nil
}

func expandTemplate(base *url.URL, rep representation, st *segmentTemplate) (*Track, error) {
	subst := func(s string, number int, time int64) string {
		s = strings.ReplaceAll(s, "$RepresentationID$", rep.ID)
		s = strings.ReplaceAll(s, "$Bandwidth$", strconv.Itoa(rep.Bandwidth))
		s = replaceIndexed(s, "Number", number)
		s = replaceIndexed(s, "Time", int(time))
		s = strings.ReplaceAll(s, "$$", "$")
		return s
	}
	resolve := func(ref string) string {
		if u, err := url.Parse(ref); err == nil {
			return base.ResolveReference(u).String()
		}
		return ref
	}

	t := &Track{
		Codecs:    rep.Codecs,
		Height:    rep.Height,
		Bandwidth: rep.Bandwidth,
	}
	if st.Initialization != "" {
		t.InitURL = resolve(subst(st.Initialization, 0, 0))
	}

	start := 1
	if st.StartNumber != nil {
		start = *st.StartNumber
	}

	if st.Timeline != nil {
		num := start
		var tm int64
		for _, s := range st.Timeline.S {
			if s.T != nil {
				tm = *s.T
			}
			reps := s.R
			if reps < 0 {
				return nil, fmt.Errorf("negative repeat in SegmentTimeline not supported")
			}
			for i := 0; i <= reps; i++ {
				t.Segments = append(t.Segments, resolve(subst(st.Media, num, tm)))
				tm += s.D
				num++
			}
		}
		return t, nil
	}

	// duration-based addressing: need total duration
	if st.Duration <= 0 || st.Timescale <= 0 {
		return nil, fmt.Errorf("SegmentTemplate without timeline needs duration+timescale")
	}
	// caller passes total via a package-level hack? Instead derive from MPD dur.
	// We approximate: the manifest duration is parsed by Parse and stored globally.
	total := mpdDuration.Load()
	if total == 0 {
		return nil, fmt.Errorf("cannot determine segment count (no mediaPresentationDuration)")
	}
	segDur := float64(st.Duration) / float64(st.Timescale)
	count := int(float64(total)/1000/segDur) + 1
	for i := 0; i < count; i++ {
		t.Segments = append(t.Segments, resolve(subst(st.Media, start+i, int64(i*st.Duration))))
	}
	return t, nil
}

// mpdDuration carries mediaPresentationDuration (ms) between Parse and
// expandTemplate for the duration-addressing branch. Parse is not concurrent
// per-manifest so this is safe in practice; documented as a known simplification.
var mpdDuration atomic.Int64

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func trackKind(mime, ctype string) string {
	s := strings.ToLower(mime + " " + ctype)
	switch {
	case strings.Contains(s, "video"):
		return "video"
	case strings.Contains(s, "audio"):
		return "audio"
	default:
		return "other"
	}
}

func hasProtection(cp []struct {
	SchemeIdURI string `xml:"schemeIdUri,attr"`
}) bool {
	return len(cp) > 0
}

func resolveBase(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" || base == nil {
		return base
	}
	if u, err := url.Parse(ref); err == nil {
		return base.ResolveReference(u)
	}
	return base
}

func replaceIndexed(s, name string, val int) string {
	// handles $Number$ and $Number%05d$
	plain := "$" + name + "$"
	s = strings.ReplaceAll(s, plain, strconv.Itoa(val))
	for {
		i := strings.Index(s, "$"+name+"%")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "$")
		k := strings.Index(s[i+1:], "$")
		if k < 0 {
			break
		}
		spec := s[i+1+len(name) : i+1+k] // e.g. "%05d"
		s = s[:i] + fmt.Sprintf(spec, val) + s[i+1+k+1:]
		_ = j
	}
	return s
}

// DownloadOptions configures track assembly.
type DownloadOptions struct {
	Dir   string
	Conns int
}

// WorkerState is one connection's live status, for the IDM-style table.
type WorkerState struct {
	ID      int
	Segment int
	Status  string // connecting | receiving | idle
	Bytes   int64
}

// Progress during assembly.
type Progress struct {
	Segment   int
	Total     int
	DoneBytes int64
	Workers   []WorkerState
}

// AssembleTrack downloads a track's init + segments using a fixed pool of
// opt.Conns worker connections and writes them in order to outFile.
func (c *Client) AssembleTrack(ctx context.Context, t *Track, outFile string, opt DownloadOptions, onProgress func(Progress)) error {
	if opt.Conns <= 0 {
		opt.Conns = 6
	}
	if opt.Conns > len(t.Segments) && len(t.Segments) > 0 {
		opt.Conns = len(t.Segments)
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return err
	}
	parts := make([]string, len(t.Segments))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var done, doneBytes atomic.Int64

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
			Segment: int(done.Load()), Total: len(t.Segments),
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
				states[w].Segment, states[w].Status = i, "receiving"
				mu.Unlock()
				emit()
				var lastEmit time.Time
				fn := filepath.Join(opt.Dir, fmt.Sprintf("%s-%06d", t.Kind, i))
				err := c.fetchSegment(ctx, t.Segments[i], fn, func(n int) {
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
	for i := range t.Segments {
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

	out, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer out.Close()
	if t.InitURL != "" {
		initData, err := c.get(ctx, t.InitURL)
		if err != nil {
			return fmt.Errorf("init: %w", err)
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
	return out.Close()
}

// SetManifestDuration is called by Parse to publish mediaPresentationDuration.
func parseISODurationMS(s string) int64 {
	// PT#H#M#S(.fraction)
	s = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "PT")
	var ms float64
	num := strings.Builder{}
	for _, r := range s {
		switch r {
		case 'H':
			v, _ := strconv.ParseFloat(num.String(), 64)
			ms += v * 3600000
			num.Reset()
		case 'M':
			v, _ := strconv.ParseFloat(num.String(), 64)
			ms += v * 60000
			num.Reset()
		case 'S':
			v, _ := strconv.ParseFloat(num.String(), 64)
			ms += v * 1000
			num.Reset()
		default:
			num.WriteRune(r)
		}
	}
	return int64(ms)
}
