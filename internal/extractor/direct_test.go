package extractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeYtDlp writes a script that emits the given JSON on stdout.
func fakeYtDlp(t *testing.T, stdout string) *Extractor {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	body := filepath.Join(dir, "body.json")
	if err := os.WriteFile(body, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat "+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Extractor{argv: []string{bin}}
}

const splitJSON = `{
 "title":"Clip","is_live":false,
 "requested_formats":[
  {"url":"https://cdn/v.mp4","ext":"mp4","protocol":"https","vcodec":"avc1","acodec":"none",
   "filesize":1000,"http_headers":{"User-Agent":"UA","Referer":"https://p/"}},
  {"url":"https://cdn/a.webm","ext":"webm","protocol":"https","vcodec":"none","acodec":"opus",
   "filesize_approx":200,"http_headers":{"User-Agent":"UA"}}
 ],
 "subtitles":{"id":[{"ext":"vtt","url":"https://s/id.vtt"},{"ext":"srt","url":"https://s/id.srt"}],
              "en":[{"ext":"srt","url":"https://s/en.srt"}]},
 "automatic_captions":{"fr":[{"ext":"srt","url":"https://s/fr.srt"}],
                       "id":[{"ext":"srt","url":"https://s/auto-id.srt"}]}}`

func TestResolveDirectSplitStreams(t *testing.T) {
	e := fakeYtDlp(t, splitJSON)
	p, err := e.ResolveDirect(context.Background(), "https://x/watch", DownloadOptions{})
	if err != nil {
		t.Fatalf("ResolveDirect: %v", err)
	}
	if p.Video == nil || p.Audio == nil || p.Muxed != nil {
		t.Fatalf("want split video+audio, got v=%v a=%v m=%v", p.Video, p.Audio, p.Muxed)
	}
	if p.Video.URL != "https://cdn/v.mp4" || p.Video.Size != 1000 {
		t.Errorf("video wrong: %+v", p.Video)
	}
	if p.Audio.Size != 200 { // filesize_approx must be used when filesize is absent
		t.Errorf("audio size not taken from filesize_approx: %+v", p.Audio)
	}
	// Headers matter: googlevideo rejects a request without the right UA.
	if p.Video.Headers["User-Agent"] != "UA" || p.Video.Headers["Referer"] != "https://p/" {
		t.Errorf("headers lost: %+v", p.Video.Headers)
	}
	if p.Title != "Clip" {
		t.Errorf("title lost: %q", p.Title)
	}
}

func TestResolveDirectProgressive(t *testing.T) {
	e := fakeYtDlp(t, `{"title":"P","url":"https://cdn/both.mp4","ext":"mp4","protocol":"https",
	 "vcodec":"avc1","acodec":"mp4a","filesize":50}`)
	p, err := e.ResolveDirect(context.Background(), "https://x/v", DownloadOptions{})
	if err != nil {
		t.Fatalf("ResolveDirect: %v", err)
	}
	if p.Muxed == nil || p.Video != nil || p.Audio != nil {
		t.Fatalf("want a single muxed stream, got %+v", p)
	}
	if p.Muxed.Kind != "muxed" {
		t.Errorf("kind = %q", p.Muxed.Kind)
	}
}

// Everything the engine cannot fetch must be refused so the caller falls back
// to yt-dlp instead of producing a broken file.
func TestResolveDirectRefusals(t *testing.T) {
	cases := map[string]string{
		"hls manifest": `{"title":"t","url":"https://x/i.m3u8","protocol":"m3u8_native",
		  "vcodec":"avc1","acodec":"mp4a"}`,
		"dash segments": `{"title":"t","url":"https://x/i.mpd","protocol":"http_dash_segments",
		  "vcodec":"avc1","acodec":"mp4a"}`,
		"drm":       `{"title":"t","_has_drm":true,"url":"https://x/v.mp4","protocol":"https","vcodec":"avc1","acodec":"mp4a"}`,
		"live":      `{"title":"t","is_live":true,"url":"https://x/v.mp4","protocol":"https","vcodec":"avc1","acodec":"mp4a"}`,
		"no url":    `{"title":"t","protocol":"https","vcodec":"avc1","acodec":"mp4a"}`,
		"no codecs": `{"title":"t","url":"https://x/v.bin","protocol":"https","vcodec":"none","acodec":"none"}`,
		"video only, no audio": `{"title":"t","url":"https://x/v.mp4","protocol":"https",
		  "vcodec":"avc1","acodec":"none"}`,
	}
	for name, js := range cases {
		e := fakeYtDlp(t, js)
		if _, err := e.ResolveDirect(context.Background(), "https://x/v", DownloadOptions{}); err == nil {
			t.Errorf("%s: should be refused so the caller falls back", name)
		}
	}
}

func TestResolveDirectSubtitleSelection(t *testing.T) {
	e := fakeYtDlp(t, splitJSON)

	// srt is preferred over vtt for the same language.
	p, _ := e.ResolveDirect(context.Background(), "https://x/v",
		DownloadOptions{SubLangs: "id"})
	if len(p.Subtitles) != 1 || p.Subtitles[0].Lang != "id" || p.Subtitles[0].Ext != "srt" {
		t.Fatalf("want the id srt, got %+v", p.Subtitles)
	}

	// Automatic captions are excluded unless asked for, and never override a
	// channel-provided track for the same language.
	p, _ = e.ResolveDirect(context.Background(), "https://x/v",
		DownloadOptions{SubLangs: "id,fr"})
	if len(p.Subtitles) != 1 {
		t.Fatalf("auto captions leaked in: %+v", p.Subtitles)
	}
	p, _ = e.ResolveDirect(context.Background(), "https://x/v",
		DownloadOptions{SubLangs: "id,fr", AutoSubs: true})
	if len(p.Subtitles) != 2 {
		t.Fatalf("want id + fr with AutoSubs, got %+v", p.Subtitles)
	}
	for _, s := range p.Subtitles {
		if s.Lang == "id" && strings.Contains(s.URL, "auto") {
			t.Error("automatic caption overrode the channel-provided one")
		}
	}

	// No languages requested => no subtitles.
	p, _ = e.ResolveDirect(context.Background(), "https://x/v", DownloadOptions{})
	if len(p.Subtitles) != 0 {
		t.Fatalf("unexpected subtitles: %+v", p.Subtitles)
	}

	// "all" takes every channel-provided language.
	p, _ = e.ResolveDirect(context.Background(), "https://x/v",
		DownloadOptions{SubLangs: "all"})
	if len(p.Subtitles) != 2 {
		t.Fatalf(`"all" should take both channel languages, got %+v`, p.Subtitles)
	}
}
