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
