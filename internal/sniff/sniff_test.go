package sniff

import (
	"net/url"
	"testing"
)

func TestClassifyResponseHits(t *testing.T) {
	cases := []struct {
		name     string
		url, ct  string
		disp     string
		length   int64
		wantKind string
		wantCat  string
	}{
		{"mp4 by type", "https://cdn/x/v", "video/mp4", "", 5 << 20, KindHTTP, CatVideo},
		{"mp3 by type", "https://cdn/song", "audio/mpeg", "", 4 << 20, KindHTTP, CatAudio},
		{"mp3 by ext, vague type", "https://cdn/song.mp3", "application/octet-stream", "", 100 << 10, KindHTTP, CatAudio},
		{"zip by type", "https://cdn/pack", "application/zip", "", 50 << 20, KindHTTP, CatArchive},
		{"rar by ext", "https://cdn/archive.rar?token=x", "", "", 80 << 20, KindHTTP, CatArchive},
		{"dmg attachment", "https://cdn/App", "application/octet-stream", `attachment; filename="App.dmg"`, 0, KindHTTP, CatArchive},
		{"pdf small ok", "https://cdn/paper.pdf", "application/pdf", "", 40 << 10, KindHTTP, CatDocument},
		{"hls master", "https://cdn/master.m3u8", "application/vnd.apple.mpegurl", "", 0, KindHLS, CatVideo},
		{"dash", "https://cdn/manifest.mpd", "application/dash+xml", "", 0, KindDASH, CatVideo},
		{"googlevideo", "https://r1---sn.googlevideo.com/videoplayback?itag=22", "", "", 0, KindHTTP, CatVideo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, ok := ClassifyResponse(c.url, c.ct, c.disp, c.length)
			if !ok {
				t.Fatalf("expected a hit")
			}
			if h.Kind != c.wantKind || h.Category != c.wantCat {
				t.Fatalf("got kind=%s cat=%s, want kind=%s cat=%s", h.Kind, h.Category, c.wantKind, c.wantCat)
			}
		})
	}
}

func TestClassifyResponseMisses(t *testing.T) {
	cases := []struct{ url, ct string }{
		{"https://site/page", "text/html"},
		{"https://site/app.js", "application/javascript"},
		{"https://site/style.css", "text/css"},
		{"https://site/logo.png", "image/png"},
		{"https://site/font.woff2", "font/woff2"},
		{"https://site/api/data", "application/json"},
		{"https://cdn/seg-00001.ts", "video/mp2t"},
		{"https://cdn/chunk.m4s", "video/mp4"},
		{"https://cdn/tiny.bin", "application/octet-stream"}, // small, no ext hint
	}
	for _, c := range cases {
		if h, ok := ClassifyResponse(c.url, c.ct, "", 10<<10); ok {
			t.Errorf("%s (%s): expected miss, got %+v", c.url, c.ct, h)
		}
	}
}

func TestIsPageHost(t *testing.T) {
	yes := []string{
		"https://www.youtube.com/watch?v=x",
		"https://youtube.com/shorts/abc",
		"https://www.instagram.com/reel/abc/",
		"https://vt.tiktok.com/xyz",
		"https://old.reddit.com/r/x/comments/y",
		"https://soundcloud.com/artist/track",
	}
	for _, s := range yes {
		u, _ := url.Parse(s)
		if !IsPageHost(u.Host) {
			t.Errorf("%s should be a page host", s)
		}
	}
	no := []string{"https://cdn.example.com/a.mp4", "https://files.fast.com/x.zip"}
	for _, s := range no {
		u, _ := url.Parse(s)
		if IsPageHost(u.Host) {
			t.Errorf("%s should NOT be a page host", s)
		}
	}
}

func TestMediaCDNNotPageHost(t *testing.T) {
	cdn := []string{
		"v16-webapp-prime.tiktok.com", "v19-webapp.tiktok.com",
		"p16-sign-va.tiktokcdn.com", "v3-web.tiktokcdn-us.com",
		"api.tiktokv.com", "scontent.cdninstagram.com",
	}
	for _, h := range cdn {
		if IsPageHost(h) {
			t.Errorf("%s should NOT be a page host", h)
		}
	}
	for _, h := range []string{"www.tiktok.com", "tiktok.com", "m.tiktok.com", "www.youtube.com"} {
		if !IsPageHost(h) {
			t.Errorf("%s SHOULD be a page host", h)
		}
	}
}
