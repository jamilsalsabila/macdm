package manager

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

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
	Note      string               `json:"note,omitempty"`
}

// Probe inspects a URL without downloading. It is best-effort: network hiccups
// return a minimal result rather than an error so the dialog still opens.
func (m *Manager) Probe(ctx context.Context, rawurl string, headers map[string]string) *ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil {
		return &ProbeResult{Kind: sniff.KindHTTP, URL: rawurl, Filename: "download", Note: "unparseable URL"}
	}
	res := &ProbeResult{Kind: sniff.ClassifyURL(u), URL: rawurl, Filename: baseName(u)}

	switch res.Kind {
	case sniff.KindExtract:
		ex, err := extractor.New(m.cfg.Tools)
		if err != nil {
			res.Note = err.Error()
			return res
		}
		info, err := ex.Probe(ctx, rawurl)
		if err != nil {
			res.Note = err.Error()
			if strings.Contains(strings.ToLower(err.Error()), "drm") {
				res.DRM = true
			}
			return res
		}
		res.Title = info.Title
		res.Live = info.IsLive
		res.Formats = info.QualityChoices()
		if info.Title != "" {
			res.Filename = sanitize(info.Title) + ".mp4"
		}

	case sniff.KindHLS:
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

	case sniff.KindDASH:
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
		res.Size = pr.TotalBytes
		res.Resumable = pr.AcceptRanges
		if pr.Filename != "" {
			res.Filename = pr.Filename
		}
	}
	return res
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

func baseName(u *url.URL) string {
	b := path.Base(u.Path)
	if b == "" || b == "/" || b == "." {
		return "download"
	}
	return b
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		s = strings.ReplaceAll(s, r, "_")
	}
	if len(s) > 180 {
		s = s[:180]
	}
	if s == "" {
		return "download"
	}
	return s
}
