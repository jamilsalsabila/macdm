package extractor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "ba" selects by bitrate, not by language: on a video with dubbed soundtracks
// yt-dlp returns whichever dub encodes highest. The language has to be asked
// for explicitly, with the original selector kept as a fallback.
func TestWithAudioLang(t *testing.T) {
	base := "bv*[height<=?1080]+ba/b[height<=?1080]/bv*+ba/b"
	got := withAudioLang(base, "id")
	if !strings.Contains(got, "ba[language^=id]") {
		t.Fatalf("language filter not applied: %s", got)
	}
	if !strings.HasSuffix(got, "/"+base) {
		t.Fatalf("original selector must remain as a fallback: %s", got)
	}
}

func TestWithAudioLangNoOpCases(t *testing.T) {
	base := "bv*+ba/b"
	if got := withAudioLang(base, ""); got != base {
		t.Errorf("empty language should not change the selector: %s", got)
	}
	if got := withAudioLang(base, "   "); got != base {
		t.Errorf("blank language should not change the selector: %s", got)
	}
	// A selector with no audio component has nothing to filter.
	if got := withAudioLang("bv*[height<=720]", "id"); got != "bv*[height<=720]" {
		t.Errorf("video-only selector should be untouched: %s", got)
	}
}

const multiLangJSON = `{
 "id":"x","title":"t",
 "subtitles":{"id":[{"ext":"vtt"}],"en":[{"ext":"vtt"}]},
 "automatic_captions":{"fr":[{"ext":"vtt"}],"de":[{"ext":"vtt"}]},
 "formats":[
  {"format_id":"v1","vcodec":"avc1","acodec":"none","height":1080},
  {"format_id":"a-en","vcodec":"none","acodec":"mp4a","language":"en"},
  {"format_id":"a-id","vcodec":"none","acodec":"mp4a","language":"id"},
  {"format_id":"a-es","vcodec":"none","acodec":"mp4a","language":"es"},
  {"format_id":"a-dup","vcodec":"none","acodec":"mp4a","language":"id"},
  {"format_id":"muxed","vcodec":"avc1","acodec":"mp4a","language":"en","height":360}
 ]}`

func TestAudioLanguages(t *testing.T) {
	var in Info
	if err := json.Unmarshal([]byte(multiLangJSON), &in); err != nil {
		t.Fatal(err)
	}
	got := in.AudioLanguages()
	want := []string{"en", "es", "id"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted, de-duplicated)", got, want)
		}
	}
}

// A single-soundtrack video offers no choice, so the picker should stay empty
// rather than showing one pointless entry.
func TestAudioLanguagesSingleTrack(t *testing.T) {
	var in Info
	if err := json.Unmarshal([]byte(`{"formats":[
	 {"format_id":"a","vcodec":"none","acodec":"mp4a","language":"en"},
	 {"format_id":"v","vcodec":"avc1","acodec":"none","height":720}]}`), &in); err != nil {
		t.Fatal(err)
	}
	if got := in.AudioLanguages(); got != nil {
		t.Fatalf("one language is not a choice, got %v", got)
	}
}

// Only channel-provided subtitles are offered: automatic captions run to ~150
// machine translations per video and would swamp the list.
func TestSubtitleLanguagesExcludesAutomatic(t *testing.T) {
	var in Info
	if err := json.Unmarshal([]byte(multiLangJSON), &in); err != nil {
		t.Fatal(err)
	}
	got := in.SubtitleLanguages()
	if len(got) != 2 || got[0] != "en" || got[1] != "id" {
		t.Fatalf("got %v, want [en id]", got)
	}
	for _, g := range got {
		if g == "fr" || g == "de" {
			t.Fatalf("automatic caption language %q leaked into the list", g)
		}
	}
}

// The flags must actually reach yt-dlp. A fake binary records its argv, so this
// catches a wiring mistake that unit-testing withAudioLang alone would not.
func TestDownloadPassesLanguageAndSubtitleFlags(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	fake := filepath.Join(dir, "fake-ytdlp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		// yt-dlp's contract with the caller: the final path, printed after move.
		"echo 'MACDM_FILE " + filepath.Join(dir, "out.mp4") + "'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Extractor{argv: []string{fake}}
	_, err := e.Download(context.Background(), "https://example.com/watch?v=x",
		DownloadOptions{
			OutDir:    dir,
			AudioLang: "id",
			SubLangs:  "id,en",
			AutoSubs:  true,
		}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake yt-dlp recorded nothing: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "ba[language^=id]") {
		t.Errorf("audio language missing from the format selector:\n%s", joined)
	}
	// Without this the merged file claims "eng" whatever dub is inside.
	if !strings.Contains(joined, "language=ind") {
		t.Errorf("merged audio is not tagged with its language:\n%s", joined)
	}
	for _, want := range []string{"--write-subs", "--sub-langs", "id,en", "--write-auto-subs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in argv:\n%s", want, joined)
		}
	}
	// The URL must stay behind "--" so one starting with "-" is not read as a flag.
	for i, a := range args {
		if a == "--" {
			if i+1 >= len(args) || !strings.HasPrefix(args[i+1], "https://") {
				t.Errorf("URL is not the argument after --: %v", args)
			}
			break
		}
	}
}

// Without the options, none of the flags appear — the default download must not
// suddenly start pulling subtitles for everyone.
func TestDownloadOmitsFlagsWhenUnset(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	fake := filepath.Join(dir, "fake-ytdlp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo 'MACDM_FILE " + filepath.Join(dir, "out.mp4") + "'\n"
	os.WriteFile(fake, []byte(script), 0o755)
	os.WriteFile(filepath.Join(dir, "out.mp4"), []byte("v"), 0o644)

	e := &Extractor{argv: []string{fake}}
	if _, err := e.Download(context.Background(), "https://example.com/v",
		DownloadOptions{OutDir: dir}, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	raw, _ := os.ReadFile(argvFile)
	joined := string(raw)
	for _, unwanted := range []string{"--write-subs", "--write-auto-subs", "language^=", "--postprocessor-args"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("unexpected %q with no options set:\n%s", unwanted, joined)
		}
	}
}

// An MP4 language field takes ISO 639-2 three-letter codes; yt-dlp reports
// two-letter ones, so "id" written verbatim leaves the track untagged.
func TestISO6392(t *testing.T) {
	cases := map[string]string{
		"id": "ind", "en": "eng", "es": "spa", "ja": "jpn",
		"id-ID": "ind", "PT_BR": "por",
		"ind": "ind", // already three-letter
		"":    "",
		"xx":  "xx", // unknown: pass through rather than guess
	}
	for in, want := range cases {
		if got := iso6392(in); got != want {
			t.Errorf("iso6392(%q) = %q, want %q", in, got, want)
		}
	}
}
