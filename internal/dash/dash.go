// Package dash parses an MPD manifest far enough to download a static (VOD)
// presentation: it picks the best video Representation and the best audio
// Representation, expands their SegmentTemplate (with $Number$/$Time$ and an
// optional SegmentTimeline) or their SegmentList into segment URLs, downloads
// them, and concatenates each track's init + media segments into one file per
// track.
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

	"macdm/internal/ratelimit"
	"macdm/internal/store"
	"macdm/internal/subs"
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
	Lang              string           `xml:"lang,attr"`
	BaseURL           string           `xml:"BaseURL"`
	SegmentTemplate   *segmentTemplate `xml:"SegmentTemplate"`
	SegmentList       *segmentList     `xml:"SegmentList"`
	ContentProtection []struct {
		SchemeIdURI string `xml:"schemeIdUri,attr"`
	} `xml:"ContentProtection"`
	Accessibility []struct {
		SchemeIdURI string `xml:"schemeIdUri,attr"`
		Value       string `xml:"value,attr"`
	} `xml:"Accessibility"`
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
	SegmentList       *segmentList     `xml:"SegmentList"`
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

// segmentList is the explicit-list addressing mode: every segment is named by
// its own <SegmentURL>, rather than derived from a template.
type segmentList struct {
	Duration       int `xml:"duration,attr"`
	Timescale      int `xml:"timescale,attr"`
	Initialization *struct {
		SourceURL string `xml:"sourceURL,attr"`
		Range     string `xml:"range,attr"`
	} `xml:"Initialization"`
	SegmentURLs []struct {
		Media      string `xml:"media,attr"`
		MediaRange string `xml:"mediaRange,attr"`
	} `xml:"SegmentURL"`
}

// Track is one resolved, downloadable stream (video or audio).
type Track struct {
	Kind      string // "video", "audio" or "text"
	InitURL   string
	Segments  []string
	Codecs    string
	Height    int
	Bandwidth int
	MimeType  string // set for text tracks, to pick the sidecar extension
	Language  string // BCP-47 tag from AdaptationSet@lang, for naming a sidecar
}

// Manifest is the useful result of parsing an MPD.
type Manifest struct {
	// Video/Audio/Subtitle are the first period's tracks. Kept as the simple
	// case most callers want; a multi-period presentation needs Periods.
	Video *Track
	Audio *Track
	// Subtitle is a text track (WebVTT or TTML), when the MPD offers one.
	// Optional: its absence never fails a download.
	Subtitle *Track

	// CaptionLang is the first period's caption language (see PeriodTracks).
	CaptionLang string

	// Duration is mediaPresentationDuration. Zero when the MPD omits it.
	// Used to size the download before it starts, so it can be refused for
	// lack of disk space rather than dying most of the way through.
	Duration time.Duration

	// Periods holds every period in order. A presentation split across periods
	// (ad insertion is the usual reason) is only complete if all of them are
	// downloaded — using just the first silently truncates the video.
	Periods []PeriodTracks
}

// PeriodTracks are the resolved tracks of one <Period>.
type PeriodTracks struct {
	Video    *Track
	Audio    *Track
	Subtitle *Track
	// CaptionLang is the language of closed captions carried inside the video
	// bitstream, declared by an <Accessibility> element. Empty when the MPD
	// says nothing — captions may still be present, just unlabelled.
	CaptionLang string
}

// Client fetches and parses.
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

// FetchSubtitles downloads a text track and returns its contents plus the file
// extension to save it under. Subtitle tracks are kilobytes, so they are
// fetched sequentially into memory.
//
// Segmented WebVTT is merged properly (see internal/subs); a single-file track
// is returned verbatim. Segmented TTML is refused rather than concatenated —
// gluing XML documents end to end produces a file nothing can parse.
func (c *Client) FetchSubtitles(ctx context.Context, t *Track) ([]byte, string, error) {
	if t == nil || len(t.Segments) == 0 {
		return nil, "", fmt.Errorf("no subtitle segments")
	}
	ext := subtitleExt(t)

	// One file, no init segment: whatever it is, save it as-is.
	if len(t.Segments) == 1 && t.InitURL == "" {
		b, err := c.get(ctx, t.Segments[0])
		return b, ext, err
	}

	if ext != ".vtt" {
		return nil, "", fmt.Errorf("segmented %s subtitles are not supported", strings.TrimPrefix(ext, "."))
	}
	parts := make([][]byte, 0, len(t.Segments))
	for _, u := range t.Segments {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		b, err := c.get(ctx, u)
		if err != nil {
			return nil, "", err
		}
		parts = append(parts, b)
	}
	return subs.MergeVTT(parts), ext, nil
}

// subtitleExt picks the sidecar extension from the track's mime/codecs.
func subtitleExt(t *Track) string {
	s := strings.ToLower(t.MimeType + " " + t.Codecs)
	switch {
	case strings.Contains(s, "ttml"), strings.Contains(s, "stpp"):
		return ".ttml"
	default:
		return ".vtt" // text/vtt, codecs=wvtt, and the common unlabelled case
	}
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
	durationMS := parseISODurationMS(m.MediaPresentationDuration)

	base, _ := url.Parse(rawurl)
	base = resolveBase(base, m.BaseURL)

	out := &Manifest{Duration: time.Duration(durationMS) * time.Millisecond}
	for _, p := range m.Periods {
		pt, err := resolvePeriod(base, p, preferHeight, durationMS)
		if err != nil {
			return nil, err
		}
		if pt.Video == nil && pt.Audio == nil {
			continue // an empty period (or one we cannot address) adds nothing
		}
		out.Periods = append(out.Periods, pt)
	}
	if len(out.Periods) == 0 {
		return nil, fmt.Errorf("no downloadable tracks (unsupported segment addressing?)")
	}
	out.Video, out.Audio, out.Subtitle = out.Periods[0].Video, out.Periods[0].Audio, out.Periods[0].Subtitle
	out.CaptionLang = out.Periods[0].CaptionLang
	return out, nil
}

// resolvePeriod picks the best video/audio/subtitle representation of a single
// <Period>.
func resolvePeriod(base *url.URL, p period, preferHeight int, durationMS int64) (PeriodTracks, error) {
	var out PeriodTracks
	pbase := resolveBase(base, p.BaseURL)

	for _, as := range p.AdaptationSets {
		if hasProtection(as.ContentProtection) {
			return out, fmt.Errorf("stream is DRM-protected (ContentProtection present)")
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
				return out, fmt.Errorf("stream is DRM-protected")
			}
			st := rep.SegmentTemplate
			if st == nil {
				st = as.SegmentTemplate
			}
			sl := rep.SegmentList
			if sl == nil {
				sl = as.SegmentList
			}

			var t *Track
			switch {
			case st != nil:
				rbase := resolveBase(asbase, rep.BaseURL)
				var err error
				if t, err = expandTemplate(rbase, rep, st, durationMS); err != nil {
					return out, err
				}
			case sl != nil:
				rbase := resolveBase(asbase, rep.BaseURL)
				var err error
				if t, err = expandList(rbase, rep, sl); err != nil {
					return out, err
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
				continue // no addressing mode we can resolve
			}
			t.Kind = kind
			switch kind {
			case "video":
				if out.Video == nil {
					out.Video = t
					out.CaptionLang = cea608Lang(as.Accessibility)
				}
			case "audio":
				if out.Audio == nil {
					out.Audio = t
				}
			case "text":
				if out.Subtitle == nil {
					t.MimeType = firstNonEmpty(rep.MimeType, as.MimeType)
					t.Language = as.Lang
					out.Subtitle = t
				}
			}
			break
		}
	}
	return out, nil
}

// expandList resolves SegmentList addressing: an explicit <SegmentURL> per
// segment instead of a template. Each media attribute is resolved against the
// Representation's base, and <Initialization sourceURL> gives the init segment.
func expandList(base *url.URL, rep representation, sl *segmentList) (*Track, error) {
	t := &Track{Codecs: rep.Codecs, Height: rep.Height, Bandwidth: rep.Bandwidth}

	if sl.Initialization != nil {
		switch {
		case sl.Initialization.SourceURL != "":
			t.InitURL = resolveRef(base, sl.Initialization.SourceURL)
		case sl.Initialization.Range != "":
			// The init segment is a byte range of BaseURL. Track carries plain
			// URLs with no ranges, so this would silently fetch the whole file
			// as "init" and corrupt the concatenation.
			return nil, fmt.Errorf("SegmentList with a byte-range Initialization is not supported")
		}
	}

	for _, su := range sl.SegmentURLs {
		if su.Media == "" {
			// mediaRange-only: every segment is a byte range of one file. Same
			// problem as above — refuse rather than assemble garbage.
			return nil, fmt.Errorf("SegmentList addressed by mediaRange is not supported")
		}
		t.Segments = append(t.Segments, resolveRef(base, su.Media))
	}
	if len(t.Segments) == 0 {
		return nil, fmt.Errorf("SegmentList has no SegmentURL entries")
	}
	return t, nil
}

// resolveRef resolves a possibly-relative reference against base, leaving it
// untouched when it will not parse.
func resolveRef(base *url.URL, ref string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	if base == nil {
		return u.String()
	}
	return base.ResolveReference(u).String()
}

func expandTemplate(base *url.URL, rep representation, st *segmentTemplate, durationMS int64) (*Track, error) {
	subst := func(s string, number int, time int64) string {
		s = strings.ReplaceAll(s, "$RepresentationID$", rep.ID)
		s = strings.ReplaceAll(s, "$Bandwidth$", strconv.Itoa(rep.Bandwidth))
		s = replaceIndexed(s, "Number", number)
		s = replaceIndexed(s, "Time", int(time))
		s = strings.ReplaceAll(s, "$$", "$")
		return s
	}
	resolve := func(ref string) string { return resolveRef(base, ref) }

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

	// duration-based addressing: need a segment duration and a total.
	if st.Duration <= 0 {
		return nil, fmt.Errorf("SegmentTemplate has neither a timeline nor a duration")
	}
	// @timescale defaults to 1 per the DASH spec — requiring it rejected
	// perfectly valid manifests, including DASH-IF's own reference streams.
	timescale := st.Timescale
	if timescale <= 0 {
		timescale = 1
	}
	total := durationMS
	if total == 0 {
		return nil, fmt.Errorf("cannot determine segment count (no mediaPresentationDuration)")
	}
	// Integer ceiling, not float+1: a stream whose duration divides evenly by
	// the segment duration (a 1h stream of 2s segments is exactly 1800) would
	// otherwise be asked for one segment past the end and 404 at the finish
	// line. Float arithmetic here also risks landing just above a whole number.
	segMS := int64(st.Duration) * 1000 / int64(timescale)
	if segMS <= 0 {
		return nil, fmt.Errorf("SegmentTemplate duration is too small to address")
	}
	count := int((total + segMS - 1) / segMS)
	for i := 0; i < count; i++ {
		t.Segments = append(t.Segments, resolve(subst(st.Media, start+i, int64(i*st.Duration))))
	}
	return t, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// cea608Lang reads the first language out of an SCTE CEA-608 accessibility
// declaration, e.g. value="CC1=eng;CC3=swe" -> "eng".
func cea608Lang(acc []struct {
	SchemeIdURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}) string {
	for _, a := range acc {
		if !strings.Contains(strings.ToLower(a.SchemeIdURI), "cea-608") {
			continue
		}
		for _, part := range strings.Split(a.Value, ";") {
			if _, lang, ok := strings.Cut(strings.TrimSpace(part), "="); ok && lang != "" {
				return lang
			}
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
	// text/vtt, application/ttml+xml, contentType="text", and the
	// application/mp4 + codecs=wvtt segmented form all land here.
	case strings.Contains(s, "text") || strings.Contains(s, "ttml") ||
		strings.Contains(s, "vtt") || strings.Contains(s, "subtitle"):
		return "text"
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
