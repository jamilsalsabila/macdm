// Package sniff is the daemon-side capture classifier. It mirrors the heuristic
// in extension/classify.js: the extension uses its copy to decide what to badge
// and offer; the daemon calls Classify* here to decide, authoritatively, how a
// job URL should be fetched.
package sniff

import (
	"net/url"
	"strings"
)

// Kind values match store.Kind* (kept as bare strings to avoid an import cycle).
const (
	KindHTTP    = "http"
	KindHLS     = "hls"
	KindDASH    = "dash"
	KindExtract = "extract"
)

// Category groups a hit for the popup UI.
const (
	CatVideo    = "video"
	CatAudio    = "audio"
	CatArchive  = "archive"
	CatDocument = "document"
	CatOther    = "other"
)

var videoContentTypes = map[string]bool{
	"video/mp4": true, "video/webm": true, "video/x-matroska": true,
	"video/quicktime": true, "video/mp2t": true, "video/mpeg": true,
	"video/3gpp": true, "video/ogg": true, "video/x-flv": true, "video/x-m4v": true,
}
var audioContentTypes = map[string]bool{
	"audio/mpeg": true, "audio/mp4": true, "audio/aac": true, "audio/ogg": true,
	"audio/webm": true, "audio/x-m4a": true, "audio/flac": true, "audio/wav": true,
	"audio/x-wav": true, "audio/opus": true,
}

// downloadContentTypes are non-A/V types that still mean "a file to save".
var downloadContentTypes = map[string]string{ // ct -> category
	"application/zip":                         CatArchive,
	"application/x-zip-compressed":            CatArchive,
	"application/x-rar-compressed":            CatArchive,
	"application/vnd.rar":                     CatArchive,
	"application/x-7z-compressed":             CatArchive,
	"application/x-tar":                       CatArchive,
	"application/gzip":                        CatArchive,
	"application/x-gzip":                      CatArchive,
	"application/x-bzip2":                     CatArchive,
	"application/x-xz":                        CatArchive,
	"application/x-apple-diskimage":           CatArchive,
	"application/x-msdownload":                CatArchive,
	"application/x-msi":                       CatArchive,
	"application/x-iso9660-image":             CatArchive,
	"application/vnd.android.package-archive": CatArchive,
	"application/vnd.debian.binary-package":   CatArchive,
	"application/x-redhat-package-manager":    CatArchive,
	"application/octet-stream":                CatOther,
	"application/pdf":                         CatDocument,
	"application/epub+zip":                    CatDocument,
	"application/rtf":                         CatDocument,
	"application/msword":                      CatDocument,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": CatDocument,
	"application/vnd.ms-excel": CatDocument,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         CatDocument,
	"application/vnd.ms-powerpoint":                                             CatDocument,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": CatDocument,
	"application/x-bittorrent":                                                  CatOther,
}

// excludeContentTypes are the page/asset types we never treat as downloads
// (unless Content-Disposition says attachment).
func excludedType(ct string) bool {
	switch {
	case ct == "text/html", ct == "text/css", ct == "text/plain",
		ct == "application/javascript", ct == "text/javascript",
		ct == "application/json", ct == "application/manifest+json",
		ct == "application/xml", ct == "text/xml",
		ct == "image/svg+xml":
		return true
	case strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "font/"),
		strings.HasPrefix(ct, "text/"):
		return true
	}
	return false
}

var extCategory = map[string]string{
	// video
	".mp4": CatVideo, ".m4v": CatVideo, ".webm": CatVideo, ".mkv": CatVideo,
	".mov": CatVideo, ".flv": CatVideo, ".avi": CatVideo, ".wmv": CatVideo,
	".mpg": CatVideo, ".mpeg": CatVideo, ".3gp": CatVideo, ".ogv": CatVideo,
	// audio
	".mp3": CatAudio, ".m4a": CatAudio, ".aac": CatAudio, ".flac": CatAudio,
	".wav": CatAudio, ".ogg": CatAudio, ".opus": CatAudio, ".wma": CatAudio,
	// archives / installers
	".zip": CatArchive, ".rar": CatArchive, ".7z": CatArchive, ".tar": CatArchive,
	".gz": CatArchive, ".tgz": CatArchive, ".bz2": CatArchive, ".xz": CatArchive,
	".zst": CatArchive, ".dmg": CatArchive, ".pkg": CatArchive, ".exe": CatArchive,
	".msi": CatArchive, ".iso": CatArchive, ".apk": CatArchive, ".deb": CatArchive,
	".rpm": CatArchive, ".appimage": CatArchive,
	// documents
	".pdf": CatDocument, ".epub": CatDocument, ".mobi": CatDocument, ".azw3": CatDocument,
	".doc": CatDocument, ".docx": CatDocument, ".xls": CatDocument, ".xlsx": CatDocument,
	".ppt": CatDocument, ".pptx": CatDocument, ".csv": CatDocument, ".rtf": CatDocument,
	// other big binaries
	".psd": CatOther, ".ai": CatOther, ".blend": CatOther, ".fbx": CatOther,
	".torrent": CatOther,
}

var pageHosts = []string{
	"youtube.com", "youtu.be", "vimeo.com", "twitch.tv", "tiktok.com",
	"instagram.com", "twitter.com", "x.com", "facebook.com", "fb.watch",
	"dailymotion.com", "reddit.com", "redd.it", "bilibili.com", "soundcloud.com",
	"streamable.com", "rumble.com", "vk.com", "vk.ru", "nicovideo.jp",
	"ok.ru", "bitchute.com", "odysee.com", "twitch.tv", "kick.com",
}

// ClassifyURL decides a job kind from the URL alone (the CLI / paste case).
func ClassifyURL(u *url.URL) string {
	p := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(p, ".m3u8"):
		return KindHLS
	case strings.HasSuffix(p, ".mpd"):
		return KindDASH
	case IsPageHost(u.Host):
		return KindExtract
	default:
		return KindHTTP
	}
}

// Hit is a positive classification of an observed response.
type Hit struct {
	URL      string
	Kind     string
	Category string
}

// ClassifyResponse decides whether an observed request/response is a
// downloadable file, given what the sniffer captured. contentLength <= 0 means
// "unknown"; disposition is the raw Content-Disposition header (may be empty).
func ClassifyResponse(rawURL, contentType, disposition string, contentLength int64) (Hit, bool) {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	lower := stripQuery(strings.ToLower(rawURL))
	attachment := strings.Contains(strings.ToLower(disposition), "attachment")

	// Manifests first — always interesting regardless of size.
	switch {
	case strings.HasSuffix(lower, ".mpd") || ct == "application/dash+xml":
		return Hit{rawURL, KindDASH, CatVideo}, true
	case strings.HasSuffix(lower, ".m3u8"),
		ct == "application/vnd.apple.mpegurl",
		ct == "application/x-mpegurl",
		ct == "application/mpegurl":
		return Hit{rawURL, KindHLS, CatVideo}, true
	}

	// Individual adaptive-stream fragments are noise; we want the manifest.
	if hasExt(lower, ".ts", ".m4s") {
		return Hit{}, false
	}

	// Content-Disposition: attachment is an explicit "download me". The real
	// filename often lives only in the header, so categorise from that too.
	if attachment {
		name := dispositionFilename(disposition)
		cat := categorize(lower, ct)
		if cat == CatOther || cat == "" {
			if c := extCategory[extOf(strings.ToLower(name))]; c != "" {
				cat = c
			}
		}
		return Hit{rawURL, KindHTTP, cat}, true
	}

	switch {
	case videoContentTypes[ct]:
		return Hit{rawURL, KindHTTP, CatVideo}, true
	case audioContentTypes[ct]:
		return Hit{rawURL, KindHTTP, CatAudio}, true
	}
	if cat, ok := downloadContentTypes[ct]; ok {
		// octet-stream / torrent: only when big or the extension backs it up.
		if ct == "application/octet-stream" && contentLength >= 0 && contentLength < 512*1024 &&
			extCategory[extOf(lower)] == "" {
			return Hit{}, false
		}
		if c2 := extCategory[extOf(lower)]; c2 != "" {
			cat = c2
		}
		return Hit{rawURL, KindHTTP, cat}, true
	}

	if excludedType(ct) {
		return Hit{}, false
	}

	// Fall back to the URL extension for servers that send a vague/missing type.
	if cat := extCategory[extOf(lower)]; cat != "" {
		big := contentLength <= 0 || contentLength > 512*1024
		if cat == CatDocument || cat == CatAudio || big {
			return Hit{rawURL, KindHTTP, cat}, true
		}
	}
	if strings.Contains(lower, "videoplayback") && (contentLength <= 0 || contentLength > 512*1024) {
		return Hit{rawURL, KindHTTP, CatVideo}, true
	}
	return Hit{}, false
}

func categorize(lowerURL, ct string) string {
	if c := extCategory[extOf(lowerURL)]; c != "" {
		return c
	}
	if c, ok := downloadContentTypes[ct]; ok {
		return c
	}
	switch {
	case videoContentTypes[ct]:
		return CatVideo
	case audioContentTypes[ct]:
		return CatAudio
	}
	return CatOther
}

// IsPageHost reports whether host is a site whose page URLs should go to the
// extractor rather than the raw engine.
// mediaCDNSuffixes are hosts that only ever serve raw media bytes. A URL here is
// a signed stream chunk — useless to the extractor (yt-dlp just 403s and retries
// for a minute). They must NOT classify as KindExtract even though the CDN
// domain often ends in a page-host's name (…​.tiktok.com).
var mediaCDNSuffixes = []string{
	"tiktokcdn.com", "tiktokcdn-us.com", "tiktokv.com", "muscdn.com",
	"byteoversea.com", "ibytedtos.com", "akamaized.net", "fbcdn.net",
	"cdninstagram.com", "googlevideo.com",
}

// isMediaCDNHost matches the suffixes above plus TikTok's numbered stream hosts
// (v16-webapp-prime.tiktok.com, v19-…-useast.tiktok.com, …).
func isMediaCDNHost(host string) bool {
	for _, s := range mediaCDNSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	if strings.HasSuffix(host, ".tiktok.com") {
		first := host[:strings.IndexByte(host, '.')]
		if len(first) >= 2 && first[0] == 'v' && first[1] >= '0' && first[1] <= '9' {
			return true
		}
	}
	return false
}

func IsPageHost(host string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if isMediaCDNHost(host) {
		return false
	}
	for _, h := range pageHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func dispositionFilename(disposition string) string {
	// crude: pull filename= / filename*= value
	for _, part := range strings.Split(disposition, ";") {
		part = strings.TrimSpace(part)
		for _, key := range []string{"filename*=", "filename="} {
			if strings.HasPrefix(strings.ToLower(part), key) {
				v := strings.Trim(part[len(key):], `"'`)
				if i := strings.LastIndex(v, "'"); i >= 0 { // filename*=UTF-8''name
					v = v[i+1:]
				}
				return v
			}
		}
	}
	return ""
}

func stripQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

func extOf(lowerURL string) string {
	lowerURL = stripQuery(lowerURL)
	if i := strings.LastIndexByte(lowerURL, '/'); i >= 0 {
		lowerURL = lowerURL[i:]
	}
	if i := strings.LastIndexByte(lowerURL, '.'); i >= 0 {
		return lowerURL[i:]
	}
	return ""
}

func hasExt(lowerURL string, exts ...string) bool {
	lowerURL = stripQuery(lowerURL)
	for _, e := range exts {
		if strings.HasSuffix(lowerURL, e) {
			return true
		}
	}
	return false
}
