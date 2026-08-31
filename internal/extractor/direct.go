package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Direct download planning.
//
// yt-dlp's own downloader fetches a format over a single connection, and
// YouTube throttles a single connection hard: measured on one file, ~1.3 MB/s
// for yt-dlp versus 6.2 MB/s for the same URL over eight connections. yt-dlp
// already resolves the real media URLs and the headers they need, so the fast
// path uses it purely as a resolver and lets MacDM's own engine do the
// transfer. Anything it cannot resolve falls back to Download().

// DirectMedia is one resolved stream MacDM can fetch itself.
type DirectMedia struct {
	URL     string
	Headers map[string]string
	Ext     string
	Size    int64
	Kind    string // "video", "audio" or "muxed"
}

// DirectSub is a subtitle track available as a plain URL.
type DirectSub struct {
	Lang string
	Ext  string
	URL  string
}

// DirectPlan is everything needed to download a page's media without yt-dlp
// doing the transfer.
type DirectPlan struct {
	Title     string
	Video     *DirectMedia
	Audio     *DirectMedia
	Muxed     *DirectMedia // a progressive format carrying both streams
	Subtitles []DirectSub
}

// ResolveDirect asks yt-dlp which formats it would download and returns their
// URLs, without downloading anything. It reports an error when the result is
// not something the engine can fetch directly — a manifest, a DRM-protected
// entry, a live stream — and the caller should then use Download().
func (e *Extractor) ResolveDirect(ctx context.Context, pageURL string, opt DownloadOptions) (*DirectPlan, error) {
	sel := opt.FormatSelector
	if sel == "" {
		sel = "bv*[height<=?1080]+ba/b[height<=?1080]/bv*+ba/b"
	}
	sel = withAudioLang(sel, opt.AudioLang)

	args := []string{"-J", "--no-warnings", "--no-playlist", "--retries", "1", "-f", sel}
	if opt.CookiesFrom != "" {
		args = append(args, "--cookies-from-browser", opt.CookiesFrom)
	}
	args = append(args, "--", pageURL)

	out, err := e.cmd(ctx, args...).Output()
	if err != nil {
		return nil, wrapYtErr(err)
	}
	var raw struct {
		Title             string                 `json:"title"`
		IsLive            bool                   `json:"is_live"`
		Ext               string                 `json:"ext"`
		URL               string                 `json:"url"`
		Protocol          string                 `json:"protocol"`
		VCodec            string                 `json:"vcodec"`
		ACodec            string                 `json:"acodec"`
		Filesize          int64                  `json:"filesize"`
		FilesizeAppx      int64                  `json:"filesize_approx"`
		HTTPHeaders       map[string]string      `json:"http_headers"`
		RequestedFormats  []directFormat         `json:"requested_formats"`
		Subtitles         map[string][]subFormat `json:"subtitles"`
		AutomaticCaptions map[string][]subFormat `json:"automatic_captions"`
		HasDRMField       bool                   `json:"_has_drm"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse yt-dlp json: %w", err)
	}
	if raw.HasDRMField {
		return nil, fmt.Errorf("stream is DRM-protected")
	}
	if raw.IsLive {
		return nil, fmt.Errorf("live stream")
	}

	plan := &DirectPlan{Title: raw.Title}
	switch {
	case len(raw.RequestedFormats) > 0:
		for _, f := range raw.RequestedFormats {
			m, err := f.toMedia()
			if err != nil {
				return nil, err
			}
			switch m.Kind {
			case "video":
				if plan.Video != nil {
					return nil, fmt.Errorf("more than one video stream selected")
				}
				plan.Video = m
			case "audio":
				if plan.Audio != nil {
					return nil, fmt.Errorf("more than one audio stream selected")
				}
				plan.Audio = m
			default:
				return nil, fmt.Errorf("unexpected muxed stream among requested formats")
			}
		}
	default:
		// A single progressive format: the top level carries it directly.
		f := directFormat{
			URL: raw.URL, Ext: raw.Ext, Protocol: raw.Protocol,
			VCodec: raw.VCodec, ACodec: raw.ACodec,
			Filesize: raw.Filesize, FilesizeAppx: raw.FilesizeAppx,
			HTTPHeaders: raw.HTTPHeaders,
		}
		m, err := f.toMedia()
		if err != nil {
			return nil, err
		}
		if m.Kind != "muxed" {
			return nil, fmt.Errorf("single format has no %s stream", map[string]string{
				"video": "audio", "audio": "video",
			}[m.Kind])
		}
		plan.Muxed = m
	}
	if plan.Video == nil && plan.Audio == nil && plan.Muxed == nil {
		return nil, fmt.Errorf("no downloadable format")
	}

	plan.Subtitles = pickSubs(raw.Subtitles, raw.AutomaticCaptions, opt.SubLangs, opt.AutoSubs)
	return plan, nil
}

type directFormat struct {
	URL          string            `json:"url"`
	Ext          string            `json:"ext"`
	Protocol     string            `json:"protocol"`
	VCodec       string            `json:"vcodec"`
	ACodec       string            `json:"acodec"`
	Filesize     int64             `json:"filesize"`
	FilesizeAppx int64             `json:"filesize_approx"`
	HTTPHeaders  map[string]string `json:"http_headers"`
}

func (f directFormat) toMedia() (*DirectMedia, error) {
	// Only a plain HTTP(S) resource can be handed to the engine. "m3u8_native",
	// "http_dash_segments" and friends are manifests yt-dlp must assemble.
	if p := strings.ToLower(f.Protocol); p != "" && p != "https" && p != "http" {
		return nil, fmt.Errorf("format uses protocol %q", f.Protocol)
	}
	if !strings.HasPrefix(f.URL, "http://") && !strings.HasPrefix(f.URL, "https://") {
		return nil, fmt.Errorf("format has no direct URL")
	}
	hasV := f.VCodec != "" && f.VCodec != "none"
	hasA := f.ACodec != "" && f.ACodec != "none"
	kind := ""
	switch {
	case hasV && hasA:
		kind = "muxed"
	case hasV:
		kind = "video"
	case hasA:
		kind = "audio"
	default:
		return nil, fmt.Errorf("format has neither audio nor video")
	}
	size := f.Filesize
	if size == 0 {
		size = f.FilesizeAppx
	}
	return &DirectMedia{
		URL: f.URL, Headers: f.HTTPHeaders, Ext: f.Ext, Size: size, Kind: kind,
	}, nil
}

// pickSubs chooses one file per requested language, preferring srt then vtt.
// "all" takes every channel-provided language.
func pickSubs(subs, auto map[string][]subFormat, langs string, allowAuto bool) []DirectSub {
	langs = strings.TrimSpace(langs)
	if langs == "" {
		return nil
	}
	want := map[string]bool{}
	all := false
	for _, l := range strings.Split(langs, ",") {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "all" {
			all = true
		} else if l != "" {
			want[l] = true
		}
	}
	pick := func(list []subFormat) (subFormat, bool) {
		for _, ext := range []string{"srt", "vtt"} {
			for _, f := range list {
				if strings.EqualFold(f.Ext, ext) && f.URL != "" {
					return f, true
				}
			}
		}
		return subFormat{}, false
	}
	var out []DirectSub
	seen := map[string]bool{}
	collect := func(src map[string][]subFormat) {
		for lang, list := range src {
			key := strings.ToLower(lang)
			if seen[key] || (!all && !want[key]) {
				continue
			}
			if f, ok := pick(list); ok {
				seen[key] = true
				out = append(out, DirectSub{Lang: lang, Ext: f.Ext, URL: f.URL})
			}
		}
	}
	// Channel-provided subtitles first; automatic captions only fill languages
	// they did not cover, and only when asked for.
	collect(subs)
	if allowAuto {
		collect(auto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lang < out[j].Lang })
	return out
}
