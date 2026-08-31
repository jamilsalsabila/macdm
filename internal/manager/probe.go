package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"macdm/internal/config"
	"macdm/internal/dash"
	"macdm/internal/extractor"
	"macdm/internal/hls"
	"macdm/internal/sniff"
	"macdm/internal/store"
)

// ProbeResult is what the "New Download" dialog needs to render its fields and
// quality menu before the user commits.
type ProbeResult struct {
	Kind      string               `json:"kind"`
	URL       string               `json:"url"`
	Title     string               `json:"title,omitempty"`
	Filename  string               `json:"filename"`
	Size      int64                `json:"size"`
	Resumable bool                 `json:"resumable"`
	DRM       bool                 `json:"drm"`
	Live      bool                 `json:"live"`
	Formats   []store.FormatChoice `json:"formats,omitempty"`
	// Languages the extractor found: dubbed soundtracks and channel-provided
	// subtitles. Empty when the site offers no choice.
	AudioLangs []string `json:"audio_langs,omitempty"`
	SubLangs   []string `json:"sub_langs,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// Probe inspects a URL without downloading. It is best-effort: network hiccups
// return a minimal result rather than an error so the dialog still opens.
func (m *Manager) Probe(ctx context.Context, rawurl string, headers map[string]string) *ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil {
		return &ProbeResult{Kind: sniff.KindHTTP, URL: rawurl, Filename: "download", Note: "unparseable URL"}
	}
	res := &ProbeResult{Kind: sniff.ClassifyURL(u), URL: rawurl, Filename: baseName(u)}

	switch res.Kind {
	case sniff.KindExtract:
		// Ask yt-dlp for the real format list (resolutions + sizes, matching what
		// YouTube itself offers). If the connection is throttled and it doesn't
		// answer within the budget, fall back to a static ladder so the dialog
		// still opens with a usable quality menu.
		res.Formats = staticQualityLadder()
		ex, e := extractor.New(m.cfg.Tools)
		if e != nil {
			res.Note = e.Error()
			break
		}
		ectx, ecancel := context.WithTimeout(ctx, 25*time.Second)
		info, e := ex.Probe(ectx, rawurl, config.Load().CookiesFrom)
		ecancel()
		if e != nil {
			res.Note = e.Error()
			if strings.Contains(strings.ToLower(e.Error()), "drm") {
				res.DRM = true
			}
			break
		}
		res.Title = info.Title
		res.Live = info.IsLive
		res.AudioLangs = info.AudioLanguages()
		res.SubLangs = info.SubtitleLanguages()
		if fs := info.QualityChoices(); len(fs) > 0 {
			res.Formats = fs
		}
		if info.Title != "" {
			res.Filename = sanitize(info.Title) + ".mp4"
		}

	case sniff.KindHLS:
		return m.probeHLS(ctx, rawurl, headers, res)

	case sniff.KindDASH:
		return m.probeDASH(ctx, rawurl, headers, res)
	default: // plain HTTP
		// A short cap: the dialog only needs size/resume, and a CDN that is
		// going to reject us (TikTok) should not hold "Detecting…" for 10s.
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		defer pcancel()
		pr, err := m.eng.ProbeURL(pctx, rawurl, headers)
		if err != nil {
			res.Note = err.Error()
			return res
		}
		// The path suffix is only a guess. A CDN serving HLS or DASH from an
		// extensionless path would otherwise be offered as a plain file, and
		// the dialog would promise a download that is really playlist text.
		if hit, ok := sniff.ClassifyResponse(rawurl, pr.ContentType, "", pr.TotalBytes); ok &&
			(hit.Kind == sniff.KindHLS || hit.Kind == sniff.KindDASH) {
			return m.probeStream(ctx, rawurl, headers, hit.Kind, res)
		}
		res.Size = pr.TotalBytes
		res.Resumable = pr.AcceptRanges
		if pr.Filename != "" {
			res.Filename = pr.Filename
		}
	}
	return res
}

// probeHLS and probeDASH fill in a manifest's quality list. They are separate
// functions because the kind is not always known from the URL: a CDN serving a
// manifest from an extensionless path is classified as a plain file until the
// response's Content-Type says otherwise, and the plain-file branch then needs
// to run exactly this.
func (m *Manager) probeHLS(ctx context.Context, rawurl string, headers map[string]string, res *ProbeResult) *ProbeResult {
	res.Kind = sniff.KindHLS
	c := hls.NewClient(streamClient(headers), headers)
	pl, err := c.Parse(ctx, rawurl)
	if err != nil {
		res.Note = err.Error()
		return res
	}
	if pl.HasDRM() {
		res.DRM = true
	}
	if pl.IsMaster {
		res.Formats = pl.VariantChoices()
	}
	res.Filename = strings.TrimSuffix(res.Filename, path.Ext(res.Filename)) + ".mp4"
	return res
}

func (m *Manager) probeDASH(ctx context.Context, rawurl string, headers map[string]string, res *ProbeResult) *ProbeResult {
	res.Kind = sniff.KindDASH
	c := dash.NewClient(streamClient(headers), headers)
	if fs, err := c.ListRepresentations(ctx, rawurl); err == nil {
		res.Formats = fs
	} else {
		res.Note = err.Error()
		if strings.Contains(err.Error(), "DRM") {
			res.DRM = true
		}
	}
	res.Filename = strings.TrimSuffix(res.Filename, path.Ext(res.Filename)) + ".mp4"
	return res
}

// probeStream dispatches to the right manifest prober.
func (m *Manager) probeStream(ctx context.Context, rawurl string, headers map[string]string, kind string, res *ProbeResult) *ProbeResult {
	if kind == sniff.KindDASH {
		return m.probeDASH(ctx, rawurl, headers, res)
	}
	return m.probeHLS(ctx, rawurl, headers, res)
}

// sharedTransport is reused by every probe/stream HTTP client so connection
// pools don't multiply (and leak) per call.
var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       60 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

func streamClient(headers map[string]string) *http.Client {
	return &http.Client{Timeout: 20 * time.Second, Transport: sharedTransport}
}

// staticQualityLadder is the quality menu shown for extractor jobs without
// probing. Each ID is a yt-dlp -f expression that degrades gracefully.
func staticQualityLadder() []store.FormatChoice {
	mk := func(label string, h, fps int) store.FormatChoice {
		var sel string
		if fps > 30 {
			sel = fmt.Sprintf("bv*[height<=?%d][fps<=?%d]+ba/b[height<=?%d]/bv*+ba/b", h, fps+5, h)
		} else if h > 0 {
			sel = fmt.Sprintf("bv*[height<=?%d]+ba/b[height<=?%d]/bv*+ba/b", h, h)
		} else {
			sel = "bv*+ba/b"
		}
		return store.FormatChoice{ID: sel, Label: label, Height: h, FPS: fps, Ext: "mp4", Kind: "video+audio"}
	}
	return []store.FormatChoice{
		mk("Best available", 0, 0),
		mk("1080p60", 1080, 60),
		mk("1080p", 1080, 30),
		mk("720p", 720, 30),
		mk("480p", 480, 30),
		mk("360p", 360, 30),
		{ID: "ba/b", Label: "Audio only", Kind: "audio", Ext: "m4a"},
	}
}

func baseName(u *url.URL) string {
	b := path.Base(u.Path)
	if b == "" || b == "/" || b == "." {
		return "download"
	}
	return b
}

// maxNameBytes keeps a filename clear of the 255-byte limit on a single path
// component, with room for the ".part" and ".<lang>.srt" suffixes added later.
const maxNameBytes = 180

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		s = strings.ReplaceAll(s, r, "_")
	}
	if len(s) > maxNameBytes {
		// Cut on a character boundary. Slicing bytes can leave half a character
		// behind, which is not valid UTF-8 and shows up as mangled text in
		// Finder — 180 happens to divide evenly by 3 and 4, so pure CJK or pure
		// emoji land on a boundary and hide this, but one ASCII character in
		// front of them is all it takes to break.
		s = strings.TrimSpace(strings.ToValidUTF8(s[:maxNameBytes], ""))
	}
	if s == "" {
		return "download"
	}
	return s
}
