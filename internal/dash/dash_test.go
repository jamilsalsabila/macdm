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
