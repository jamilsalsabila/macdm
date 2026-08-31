package hls

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestParseMaster(t *testing.T) {
	m := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720
high/index.m3u8
`
	p, err := parse(m, mustURL("https://cdn.example.com/v/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsMaster || len(p.Variants) != 2 {
		t.Fatalf("want master with 2 variants, got %+v", p)
	}
	best, _ := p.BestVariant()
	if best.Bandwidth != 2400000 || best.URL != "https://cdn.example.com/v/high/index.m3u8" {
		t.Fatalf("best variant wrong: %+v", best)
	}
}

func TestParseMediaAndDRM(t *testing.T) {
	m := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://x",KEYFORMAT="com.apple.streamingkeydelivery"
#EXTINF:6.0,
seg0.ts
#EXTINF:6.0,
seg1.ts
`
	p, err := parse(m, mustURL("https://cdn.example.com/v/index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Segments) != 2 {
		t.Fatalf("want 2 segments, got %d", len(p.Segments))
	}
	if !p.HasDRM() {
		t.Fatal("SAMPLE-AES playlist should report DRM")
	}
}

func TestAssembleAES128(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("aaaaaaaaaaaaaaaa")
	plain := [][]byte{[]byte(strings.Repeat("A", 32)), []byte(strings.Repeat("B", 48))}

	enc := func(b []byte) []byte {
		block, _ := aes.NewCipher(key)
		// pkcs7 pad
		pad := 16 - len(b)%16
		for i := 0; i < pad; i++ {
			b = append(b, byte(pad))
		}
		out := make([]byte, len(b))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, b)
		return out
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(enc(append([]byte(nil), plain[0]...))) })
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(enc(append([]byte(nil), plain[1]...))) })
	var playlist string
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, playlist) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	playlist = fmt.Sprintf(`#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-KEY:METHOD=AES-128,URI="%s/key",IV=0x%x
#EXTINF:6.0,
%s/seg0.ts
#EXTINF:6.0,
%s/seg1.ts
#EXT-X-ENDLIST
`, srv.URL, iv, srv.URL, srv.URL)

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	if err := c.Assemble(context.Background(), p, AssembleOptions{Dir: t.TempDir(), OutFile: out, Conns: 2}, nil); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	got, _ := os.ReadFile(out)
	want := append(append([]byte(nil), plain[0]...), plain[1]...)
	if string(got) != string(want) {
		t.Fatalf("decrypted assembly mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestAssembleAES128LargeSegment exercises the streaming CBC decrypt across the
// 1 MiB chunk boundary (segments now decrypt file->file, not in memory).
func TestAssembleAES128LargeSegment(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("aaaaaaaaaaaaaaaa")
	// 2.5 MiB of pseudo-random plaintext — spans three decrypt chunks.
	plain := make([]byte, 2_621_440+123)
	for i := range plain {
		plain[i] = byte((i*7 + 13) % 251)
	}
	block, _ := aes.NewCipher(key)
	padded := append([]byte(nil), plain...)
	pad := 16 - len(padded)%16
	for i := 0; i < pad; i++ {
		padded = append(padded, byte(pad))
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(ct) })
	var playlist string
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, playlist) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	playlist = fmt.Sprintf(`#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-KEY:METHOD=AES-128,URI="%s/key",IV=0x%x
#EXTINF:6.0,
%s/seg0.ts
#EXT-X-ENDLIST
`, srv.URL, iv, srv.URL)

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	if err := c.Assemble(context.Background(), p, AssembleOptions{Dir: t.TempDir(), OutFile: out, Conns: 1}, nil); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	got, _ := os.ReadFile(out)
	if len(got) != len(plain) {
		t.Fatalf("size: got %d want %d", len(got), len(plain))
	}
	for i := range got {
		if got[i] != plain[i] {
			t.Fatalf("byte %d: got %d want %d", i, got[i], plain[i])
		}
	}
}

func TestParseAttrs(t *testing.T) {
	a := parseAttrs(`BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",NAME="Low, quality"`)
	if a["BANDWIDTH"] != "800000" || a["RESOLUTION"] != "640x360" {
		t.Fatalf("bad attrs: %+v", a)
	}
	if a["CODECS"] != "avc1.4d401e,mp4a.40.2" {
		t.Fatalf("quoted comma not handled: %q", a["CODECS"])
	}
	if a["NAME"] != "Low, quality" {
		t.Fatalf("quoted name with comma: %q", a["NAME"])
	}
}

func TestAssembleWorkerCount(t *testing.T) {
	mux := http.NewServeMux()
	for i := 0; i < 20; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/s%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(15 * time.Millisecond) // keep workers busy so we observe concurrency
			w.Write([]byte(strings.Repeat("x", 500)))
		})
	}
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&pl, "#EXTINF:2.0,\n%s/s%d.ts\n", srv.URL, i)
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	mux.HandleFunc("/i.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, pl.String()) })

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/i.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	var maxWorkers, slotsSeen atomic.Int64
	err = c.Assemble(context.Background(), p, AssembleOptions{Dir: t.TempDir(),
		OutFile: t.TempDir() + "/o.ts", Conns: 5}, func(pr Progress) {
		slotsSeen.Store(int64(len(pr.Workers)))
		active := int64(0)
		for _, w := range pr.Workers {
			if w.Status == "receiving" || w.Status == "connecting" {
				active++
			}
		}
		for {
			cur := maxWorkers.Load()
			if active <= cur || maxWorkers.CompareAndSwap(cur, active) {
				break
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if slotsSeen.Load() != 5 {
		t.Fatalf("want 5 worker slots, saw %d", slotsSeen.Load())
	}
	if maxWorkers.Load() < 3 {
		t.Fatalf("expected concurrent workers, peak active was %d", maxWorkers.Load())
	}
}

// TestAssembleResumesExistingSegments: a second Assemble into the same scratch
// dir must reuse the segments already on disk (so an automatic retry doesn't
// re-download the whole stream) and still produce byte-identical output.
func TestAssembleResumesExistingSegments(t *testing.T) {
	var fetches atomic.Int64
	mux := http.NewServeMux()
	var want []byte
	for i := 0; i < 6; i++ {
		i := i
		payload := []byte(strings.Repeat(string(rune('A'+i)), 1024))
		want = append(want, payload...)
		mux.HandleFunc(fmt.Sprintf("/s%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
			fetches.Add(1)
			w.Write(payload)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&pl, "#EXTINF:2.0,\n%s/s%d.ts\n", srv.URL, i)
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	mux.HandleFunc("/i.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, pl.String()) })

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/i.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	scratch := t.TempDir()
	out1 := filepath.Join(t.TempDir(), "a.ts")
	if err := c.Assemble(context.Background(), p, AssembleOptions{Dir: scratch, OutFile: out1, Conns: 3}, nil); err != nil {
		t.Fatal(err)
	}
	first := fetches.Load()
	if first != 6 {
		t.Fatalf("first pass fetched %d segments, want 6", first)
	}

	// Second pass over the same scratch dir: every segment is already there.
	out2 := filepath.Join(t.TempDir(), "b.ts")
	if err := c.Assemble(context.Background(), p, AssembleOptions{Dir: scratch, OutFile: out2, Conns: 3}, nil); err != nil {
		t.Fatal(err)
	}
	if extra := fetches.Load() - first; extra != 0 {
		t.Fatalf("resume re-downloaded %d segments — reuse is broken", extra)
	}

	got1, _ := os.ReadFile(out1)
	got2, _ := os.ReadFile(out2)
	if string(got1) != string(want) {
		t.Fatalf("first pass content wrong (%d bytes)", len(got1))
	}
	if string(got2) != string(want) {
		t.Fatalf("resumed pass content wrong (%d bytes)", len(got2))
	}
}

// A half-written segment must never be mistaken for a complete one: only an
// atomic rename publishes the final name, so a stray .part is ignored.
func TestAssembleIgnoresPartialSegmentFile(t *testing.T) {
	mux := http.NewServeMux()
	payload := []byte(strings.Repeat("Z", 2048))
	mux.HandleFunc("/s0.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pl := fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n%s/s0.ts\n#EXT-X-ENDLIST\n", srv.URL)
	mux.HandleFunc("/i.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, pl) })

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/i.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	// simulate a crash mid-segment: the temp file exists, the final name does not
	if err := os.WriteFile(filepath.Join(scratch, "seg-000000.part"), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "o.ts")
	if err := c.Assemble(context.Background(), p, AssembleOptions{Dir: scratch, OutFile: out, Conns: 1}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != string(payload) {
		t.Fatalf("partial scratch file corrupted the output: %d bytes", len(got))
	}
}

// --- #EXT-X-MEDIA alternative audio renditions ---

const masterWithAltAudio = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio_en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Spanish",LANGUAGE="es",DEFAULT=NO,AUTOSELECT=YES,URI="audio_es.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",URI="subs_en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud"
v360.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720,CODECS="avc1.4d401f",AUDIO="aud"
v720.m3u8
`

func TestParseAltAudioRenditions(t *testing.T) {
	p, err := parse(masterWithAltAudio, mustURL("https://cdn.example/hls/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsMaster || len(p.Variants) != 2 {
		t.Fatalf("expected a master with 2 variants, got master=%v n=%d", p.IsMaster, len(p.Variants))
	}
	if len(p.Media) != 3 {
		t.Fatalf("expected 3 EXT-X-MEDIA entries, got %d", len(p.Media))
	}
	en := p.Media[0]
	if en.Type != "AUDIO" || en.GroupID != "aud" || en.Name != "English" ||
		en.Language != "en" || !en.Default || !en.Autoselect {
		t.Fatalf("English rendition parsed wrong: %+v", en)
	}
	if en.URI != "https://cdn.example/hls/audio_en.m3u8" {
		t.Fatalf("rendition URI not resolved against the master: %q", en.URI)
	}
	for _, v := range p.Variants {
		if v.AudioGroup != "aud" {
			t.Errorf("variant %s lost its AUDIO group: %+v", v.Resolution, v)
		}
	}
}

func TestAudioForPicksDefault(t *testing.T) {
	p, err := parse(masterWithAltAudio, mustURL("https://cdn.example/hls/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p.BestVariant()
	r := p.AudioFor(v)
	if r == nil {
		t.Fatal("no audio rendition chosen — the download would be silent")
	}
	if r.Name != "English" {
		t.Fatalf("picked %q, want the DEFAULT=YES rendition", r.Name)
	}
	// A subtitle group must never be returned as audio.
	if r.Type != "AUDIO" {
		t.Fatalf("picked a %s rendition", r.Type)
	}
}

func TestAudioForFallsBackToAutoselectThenFirst(t *testing.T) {
	noDefault := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Commentary",DEFAULT=NO,AUTOSELECT=NO,URI="c.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Main",DEFAULT=NO,AUTOSELECT=YES,URI="m.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO="a"
v.m3u8
`
	p, err := parse(noDefault, mustURL("https://x/y/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p.BestVariant()
	r := p.AudioFor(v)
	if r == nil || r.Name != "Main" {
		t.Fatalf("want the AUTOSELECT=YES rendition, got %+v", r)
	}
}

// A rendition with no URI means that audio is already muxed into the variant.
// Fetching "nothing" is the correct answer; returning it would break the mux.
func TestAudioForIgnoresMuxedGroup(t *testing.T) {
	muxedIn := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS="avc1,mp4a",AUDIO="a"
v.m3u8
`
	p, err := parse(muxedIn, mustURL("https://x/y/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p.BestVariant()
	if r := p.AudioFor(v); r != nil {
		t.Fatalf("URI-less rendition should be treated as muxed, got %+v", r)
	}
}

func TestAudioForNoGroupOrNoMatch(t *testing.T) {
	plain := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS="avc1,mp4a"
v.m3u8
`
	p, _ := parse(plain, mustURL("https://x/y/m.m3u8"))
	v, _ := p.BestVariant()
	if r := p.AudioFor(v); r != nil {
		t.Fatalf("variant without AUDIO= should need no rendition, got %+v", r)
	}

	// Group named but absent from the master: nothing to fetch, must not panic.
	dangling := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="other",NAME="X",URI="x.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO="missing"
v.m3u8
`
	p2, _ := parse(dangling, mustURL("https://x/y/m.m3u8"))
	v2, _ := p2.BestVariant()
	if r := p2.AudioFor(v2); r != nil {
		t.Fatalf("dangling group should yield nil, got %+v", r)
	}
}

// EXT-X-MEDIA must not make a media playlist look like a master.
func TestExtXMediaDoesNotForceMaster(t *testing.T) {
	odd := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="X",URI="x.m3u8"
#EXTINF:6.0,
seg0.ts
#EXT-X-ENDLIST
`
	p, err := parse(odd, mustURL("https://x/y/index.m3u8"))
	if err != nil {
		t.Fatalf("a media playlist carrying EXT-X-MEDIA should still parse: %v", err)
	}
	if p.IsMaster {
		t.Fatal("misclassified as a master — its segments would never be downloaded")
	}
	if len(p.Segments) != 1 {
		t.Fatalf("segments lost: %d", len(p.Segments))
	}
}

func TestSubtitlesForPicksDefault(t *testing.T) {
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Spanish",LANGUAGE="es",AUTOSELECT=YES,URI="es.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,URI="aud.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO="aud",SUBTITLES="subs"
v.m3u8
`
	p, err := parse(master, mustURL("https://x/y/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p.BestVariant()
	if v.SubtitleGroup != "subs" {
		t.Fatalf("SUBTITLES attribute lost: %+v", v)
	}
	r := p.SubtitlesFor(v)
	if r == nil || r.Name != "English" {
		t.Fatalf("want the DEFAULT=YES subtitle rendition, got %+v", r)
	}
	if r.Type != "SUBTITLES" {
		t.Fatalf("returned a %s rendition", r.Type)
	}
	// The audio group must not leak into the subtitle choice, or vice versa.
	if a := p.AudioFor(v); a == nil || a.Type != "AUDIO" {
		t.Fatalf("audio selection disturbed: %+v", a)
	}
}

// A URI-less subtitle rendition is in-band CEA-608/708 — there is no file to
// fetch, so it must be skipped rather than returned.
func TestSubtitlesForSkipsURILess(t *testing.T) {
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="CC1",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=1,SUBTITLES="subs"
v.m3u8
`
	p, _ := parse(master, mustURL("https://x/y/master.m3u8"))
	v, _ := p.BestVariant()
	if r := p.SubtitlesFor(v); r != nil {
		t.Fatalf("URI-less rendition should be skipped, got %+v", r)
	}
}

func TestSubtitlesForNoGroup(t *testing.T) {
	p, _ := parse("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n", mustURL("https://x/y/m.m3u8"))
	v, _ := p.BestVariant()
	if r := p.SubtitlesFor(v); r != nil {
		t.Fatalf("no SUBTITLES attribute should mean no rendition, got %+v", r)
	}
}

// --- #EXT-X-BYTERANGE ---

func TestParseByteRangeOffsets(t *testing.T) {
	pl := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXT-X-MAP:URI="all.mp4",BYTERANGE="600@0"
#EXTINF:2.0,
#EXT-X-BYTERANGE:1000@600
all.mp4
#EXTINF:2.0,
#EXT-X-BYTERANGE:500
all.mp4
#EXTINF:2.0,
#EXT-X-BYTERANGE:250@5000
all.mp4
#EXT-X-ENDLIST
`
	p, err := parse(pl, mustURL("https://x/y/i.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if p.InitLength != 600 || p.InitOffset != 0 {
		t.Errorf("EXT-X-MAP BYTERANGE: got %d@%d", p.InitLength, p.InitOffset)
	}
	want := []struct{ off, length int64 }{
		{600, 1000},
		{1600, 500}, // offset omitted => continues from the previous end
		{5000, 250}, // explicit offset resets the run
	}
	if len(p.Segments) != len(want) {
		t.Fatalf("want %d segments, got %d", len(want), len(p.Segments))
	}
	for i, w := range want {
		if p.Segments[i].Offset != w.off || p.Segments[i].Length != w.length {
			t.Errorf("segment %d: got %d@%d, want %d@%d",
				i, p.Segments[i].Length, p.Segments[i].Offset, w.length, w.off)
		}
	}
	// Every segment shares one URL — that is the whole point of the tag.
	for i, s := range p.Segments {
		if s.URL != "https://x/y/all.mp4" {
			t.Errorf("segment %d URL = %s", i, s.URL)
		}
	}
}

func TestParseByteRangeGarbageIsIgnored(t *testing.T) {
	p, err := parse(`#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
#EXT-X-BYTERANGE:notanumber
s.ts
#EXT-X-ENDLIST
`, mustURL("https://x/y/i.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Segments[0].Length != 0 {
		t.Fatalf("garbage byterange should leave the segment whole-file, got %d@%d",
			p.Segments[0].Length, p.Segments[0].Offset)
	}
}

// The real test: assembling a playlist whose segments are ranges of one file
// must reproduce that file, and must send actual Range requests.
func TestAssembleByteRangeSegments(t *testing.T) {
	whole := []byte(strings.Repeat("A", 500) + strings.Repeat("B", 500) + strings.Repeat("C", 300))
	var ranged, plain atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/all.bin", func(w http.ResponseWriter, r *http.Request) {
		rg := r.Header.Get("Range")
		if rg == "" {
			plain.Add(1)
			w.Write(whole)
			return
		}
		ranged.Add(1)
		var start, end int64
		fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
		if end >= int64(len(whole)) {
			end = int64(len(whole)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(whole)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(whole[start : end+1])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/i.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `#EXTM3U
#EXT-X-TARGETDURATION:1
#EXTINF:1.0,
#EXT-X-BYTERANGE:500@0
%s/all.bin
#EXTINF:1.0,
#EXT-X-BYTERANGE:500
%s/all.bin
#EXTINF:1.0,
#EXT-X-BYTERANGE:300
%s/all.bin
#EXT-X-ENDLIST
`, srv.URL, srv.URL, srv.URL)
	})

	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/i.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	if err := c.Assemble(context.Background(), p,
		AssembleOptions{Dir: t.TempDir(), OutFile: out, Conns: 2}, nil); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != string(whole) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(whole))
	}
	if ranged.Load() != 3 {
		t.Errorf("want 3 Range requests, got %d", ranged.Load())
	}
	if plain.Load() != 0 {
		t.Errorf("%d segments were fetched without a Range header", plain.Load())
	}
}

// A server that ignores Range would otherwise write the whole file for every
// segment, tripling the output.
func TestAssembleByteRangeRefusesIgnoredRange(t *testing.T) {
	whole := []byte(strings.Repeat("Z", 900))
	mux := http.NewServeMux()
	mux.HandleFunc("/all.bin", func(w http.ResponseWriter, r *http.Request) { w.Write(whole) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/i.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n#EXT-X-BYTERANGE:300@0\n%s/all.bin\n#EXT-X-ENDLIST\n", srv.URL)
	})
	c := NewClient(srv.Client(), nil)
	p, err := c.Parse(context.Background(), srv.URL+"/i.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Assemble(context.Background(), p,
		AssembleOptions{Dir: t.TempDir(), OutFile: filepath.Join(t.TempDir(), "o.ts"), Conns: 1}, nil)
	if err == nil {
		t.Fatal("a server ignoring Range must be an error, not a corrupt file")
	}
	if !strings.Contains(err.Error(), "Range") {
		t.Fatalf("error should name the cause: %v", err)
	}
}

// #EXT-X-I-FRAME-STREAM-INF advertises a trick-play stream of keyframes only —
// useless as a download. It carries its URI as an attribute rather than on the
// next line, so it must never become a variant.
func TestIFrameVariantsAreNotDownloadable(t *testing.T) {
	m := `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=99000,RESOLUTION=640x360,URI="iframe_360.m3u8"
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=9900000,RESOLUTION=1920x1080,URI="iframe_1080.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
v360.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720
v720.m3u8
`
	p, err := parse(m, mustURL("https://x/y/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Variants) != 2 {
		t.Fatalf("want 2 playable variants, got %d: %+v", len(p.Variants), p.Variants)
	}
	for _, v := range p.Variants {
		if strings.Contains(v.URL, "iframe") {
			t.Fatalf("an I-frame playlist became a variant: %s", v.URL)
		}
	}
	// The 9.9 Mbps I-frame entry must not win on bandwidth.
	best, ok := p.BestVariant()
	if !ok || !strings.HasSuffix(best.URL, "/v720.m3u8") {
		t.Fatalf("best variant = %+v", best)
	}
}

// A master offering nothing but I-frame streams has nothing to download, and
// should say so rather than picking one.
func TestOnlyIFrameVariantsIsAnError(t *testing.T) {
	p, err := parse(`#EXTM3U
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=99000,URI="i.m3u8"
`, mustURL("https://x/y/m.m3u8"))
	if err == nil {
		if _, ok := p.BestVariant(); ok {
			t.Fatal("an I-frame-only master must not yield a variant")
		}
	}
}

// #EXT-X-DATERANGE carries ad markers (SCTE-35). Its segments are ordinary and
// must download like any other; the tag itself is metadata we ignore.
func TestDaterangeMarkersDoNotDisturbSegments(t *testing.T) {
	p, err := parse(`#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-DATERANGE:ID="ad1",START-DATE="2026-01-01T00:00:00Z",DURATION=30,SCTE35-OUT=0xFC30
#EXTINF:6.0,
a.ts
#EXT-X-DATERANGE:ID="ad1",END-DATE="2026-01-01T00:00:30Z"
#EXTINF:6.0,
b.ts
#EXT-X-ENDLIST
`, mustURL("https://x/y/i.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if p.IsMaster || p.Live {
		t.Fatalf("misclassified: master=%v live=%v", p.IsMaster, p.Live)
	}
	if len(p.Segments) != 2 {
		t.Fatalf("want 2 segments, got %d", len(p.Segments))
	}
	if !strings.HasSuffix(p.Segments[0].URL, "/a.ts") || !strings.HasSuffix(p.Segments[1].URL, "/b.ts") {
		t.Fatalf("segment URLs wrong: %+v", p.Segments)
	}
}
