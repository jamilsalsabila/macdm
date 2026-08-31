package dash

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndAssembleTimeline(t *testing.T) {
	mux := http.NewServeMux()
	seg := func(name, body string) {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	}
	seg("v-init.mp4", "VINIT")
	seg("v-1.m4s", "V1")
	seg("v-2.m4s", "V2")
	seg("v-3.m4s", "V3")
	seg("a-init.mp4", "AINIT")
	seg("a-1.m4s", "A1")
	seg("a-2.m4s", "A2")
	seg("a-3.m4s", "A3")

	var manifest string
	mux.HandleFunc("/m.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, manifest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	manifest = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT18S">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="v" bandwidth="1200000" width="1280" height="720" codecs="avc1.640028">
        <SegmentTemplate media="v-$Number$.m4s" initialization="v-init.mp4" startNumber="1">
          <SegmentTimeline>
            <S t="0" d="6" r="2"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="a" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentTemplate media="a-$Number$.m4s" initialization="a-init.mp4" startNumber="1">
          <SegmentTimeline>
            <S t="0" d="6" r="2"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	c := NewClient(srv.Client(), nil)
	man, err := c.Parse(context.Background(), srv.URL+"/m.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if man.Video == nil || man.Audio == nil {
		t.Fatalf("expected both tracks, got %+v", man)
	}
	if len(man.Video.Segments) != 3 || man.Video.Height != 720 {
		t.Fatalf("video track wrong: %+v", man.Video)
	}
	if !strings.HasSuffix(man.Video.Segments[0], "/v-1.m4s") {
		t.Fatalf("segment url wrong: %s", man.Video.Segments[0])
	}

	vf := filepath.Join(t.TempDir(), "v.m4s")
	if err := c.AssembleTrack(context.Background(), man.Video, vf, DownloadOptions{Dir: t.TempDir(), Conns: 3}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(vf)
	if string(got) != "VINITV1V2V3" {
		t.Fatalf("assembled video = %q, want VINITV1V2V3", got)
	}
}

func TestParseRefusesDRM(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT10S">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>
      <Representation id="v" bandwidth="1000000">
        <SegmentTemplate media="v-$Number$.m4s" initialization="i.mp4" startNumber="1" duration="2" timescale="1"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, mpd) }))
	defer srv.Close()

	c := NewClient(srv.Client(), nil)
	_, err := c.Parse(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "DRM") {
		t.Fatalf("expected DRM refusal, got %v", err)
	}
}

func TestParseRefusesLive(t *testing.T) {
	mpd := `<?xml version="1.0"?><MPD type="dynamic"><Period/></MPD>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, mpd) }))
	defer srv.Close()
	c := NewClient(srv.Client(), nil)
	if _, err := c.Parse(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "dynamic") {
		t.Fatalf("expected live refusal, got %v", err)
	}
}

func TestISODuration(t *testing.T) {
	cases := map[string]int64{
		"PT18S":        18000,
		"PT1M30S":      90000,
		"PT1H2M3S":     3723000,
		"PT0H0M6.006S": 6006,
	}
	for in, want := range cases {
		if got := parseISODurationMS(in); got != want {
			t.Errorf("%s: got %d want %d", in, got, want)
		}
	}
}

// --- SegmentList addressing ---

// parseTracks is the shared setup: serve an MPD and resolve it.
func parseMPD(t *testing.T, body string) (*Manifest, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.Client(), nil).Parse(context.Background(), srv.URL+"/media/manifest.mpd")
}

const segListMPD = `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT6S">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" width="640" height="360" codecs="avc1.4d401e">
        <SegmentList duration="2" timescale="1">
          <Initialization sourceURL="v_init.mp4"/>
          <SegmentURL media="v_1.m4s"/>
          <SegmentURL media="v_2.m4s"/>
          <SegmentURL media="v_3.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" contentType="audio">
      <Representation id="a0" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList duration="2" timescale="1">
          <Initialization sourceURL="a_init.mp4"/>
          <SegmentURL media="a_1.m4s"/>
          <SegmentURL media="a_2.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestSegmentListResolvesTracks(t *testing.T) {
	man, err := parseMPD(t, segListMPD)
	if err != nil {
		t.Fatalf("SegmentList should now parse: %v", err)
	}
	if man.Video == nil || man.Audio == nil {
		t.Fatalf("want both tracks, got video=%v audio=%v", man.Video != nil, man.Audio != nil)
	}
	if len(man.Video.Segments) != 3 || len(man.Audio.Segments) != 2 {
		t.Fatalf("segment counts wrong: video=%d audio=%d",
			len(man.Video.Segments), len(man.Audio.Segments))
	}
	// URLs must resolve against the manifest's directory, not the server root.
	if !strings.HasSuffix(man.Video.InitURL, "/media/v_init.mp4") {
		t.Errorf("init not resolved relative to the MPD: %s", man.Video.InitURL)
	}
	if !strings.HasSuffix(man.Video.Segments[0], "/media/v_1.m4s") {
		t.Errorf("segment not resolved relative to the MPD: %s", man.Video.Segments[0])
	}
	if man.Video.Height != 360 || man.Video.Codecs != "avc1.4d401e" {
		t.Errorf("representation metadata lost: %+v", man.Video)
	}
}

// A SegmentList on the AdaptationSet applies to its Representations.
func TestSegmentListInheritedFromAdaptationSet(t *testing.T) {
	man, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <SegmentList duration="2">
        <Initialization sourceURL="i.mp4"/>
        <SegmentURL media="s1.m4s"/>
        <SegmentURL media="s2.m4s"/>
      </SegmentList>
      <Representation id="v" bandwidth="1" width="320" height="240"/>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err != nil {
		t.Fatalf("AdaptationSet-level SegmentList: %v", err)
	}
	if man.Video == nil || len(man.Video.Segments) != 2 {
		t.Fatalf("inheritance failed: %+v", man.Video)
	}
}

// BaseURL inside the Representation shifts where segments live.
func TestSegmentListHonoursBaseURL(t *testing.T) {
	man, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="1" width="320" height="240">
        <BaseURL>chunks/</BaseURL>
        <SegmentList>
          <Initialization sourceURL="i.mp4"/>
          <SegmentURL media="s1.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(man.Video.Segments[0], "/media/chunks/s1.m4s") {
		t.Fatalf("BaseURL ignored: %s", man.Video.Segments[0])
	}
}

// Byte-range addressing cannot be expressed by Track (plain URLs, no ranges).
// Refusing loudly is required — silently fetching whole files would concatenate
// the same file repeatedly and produce a corrupt output.
func TestSegmentListByteRangeIsRefused(t *testing.T) {
	_, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="1" width="320" height="240">
        <BaseURL>all.mp4</BaseURL>
        <SegmentList>
          <Initialization range="0-999"/>
          <SegmentURL mediaRange="1000-1999"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err == nil {
		t.Fatal("byte-range SegmentList must be refused, not silently mis-assembled")
	}
	if !strings.Contains(err.Error(), "byte-range") && !strings.Contains(err.Error(), "mediaRange") {
		t.Fatalf("error should name the cause, got: %v", err)
	}
}

func TestSegmentListEmptyIsRefused(t *testing.T) {
	_, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="v" bandwidth="1" width="320" height="240">
        <SegmentList><Initialization sourceURL="i.mp4"/></SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err == nil {
		t.Fatal("a SegmentList with no SegmentURL entries must be an error")
	}
}
