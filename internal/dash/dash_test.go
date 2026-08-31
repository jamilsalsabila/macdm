package dash

import (
	"context"
	"fmt"
	"macdm/internal/ratelimit"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// @timescale defaults to 1 per the DASH spec, and the segment count is a
// ceiling — a stream whose duration divides evenly (1h of 2s segments is
// exactly 1800) must not be asked for one past the end. DASH-IF's own
// reference streams are written this way and used to fail outright.
func TestSegmentTemplateDefaultTimescaleAndExactCount(t *testing.T) {
	man, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT1H">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate startNumber="1" initialization="$RepresentationID$/init.mp4"
                       duration="2" media="$RepresentationID$/$Number$.m4s"/>
      <Representation id="V300" codecs="avc1.64001e" bandwidth="300000" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err != nil {
		t.Fatalf("a SegmentTemplate without @timescale is valid: %v", err)
	}
	if man.Video == nil {
		t.Fatal("no video track")
	}
	if n := len(man.Video.Segments); n != 1800 {
		t.Fatalf("want exactly 1800 segments for 1h of 2s, got %d", n)
	}
	last := man.Video.Segments[len(man.Video.Segments)-1]
	if !strings.HasSuffix(last, "/1800.m4s") {
		t.Fatalf("last segment should be 1800, got %s", last)
	}
}

// A duration that does not divide evenly still needs the trailing partial
// segment.
func TestSegmentTemplateCountRoundsUp(t *testing.T) {
	man, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT11S">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate startNumber="1" duration="2" media="$Number$.m4s"/>
      <Representation id="V" bandwidth="1" width="64" height="36"/>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(man.Video.Segments); n != 6 {
		t.Fatalf("11s of 2s segments needs 6 (5 full + 1 partial), got %d", n)
	}
}

// Extracting in-band CEA-608 captions means decoding the whole video, so the
// caller only does it when the MPD declares them. That makes this parse
// load-bearing: lose it and captions silently stop being extracted.
func TestAccessibilityDeclaresCEA608Language(t *testing.T) {
	mpd := func(acc string) string {
		return `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT1M">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">` + acc + `
      <SegmentTemplate startNumber="1" timescale="1" duration="2"
                       initialization="$RepresentationID$/init.mp4"
                       media="$RepresentationID$/$Number$.m4s"/>
      <Representation id="V300" codecs="avc1.64001e" bandwidth="300000" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`
	}
	cases := []struct {
		name string
		acc  string
		want string
	}{
		{
			// The shape the DASH-IF reference stream uses.
			name: "cea-608 with a channel language",
			acc:  `<Accessibility schemeIdUri="urn:scte:dash:cc:cea-608:2015" value="CC1=eng"/>`,
			want: "eng",
		},
		{
			name: "multiple channels take the first",
			acc:  `<Accessibility schemeIdUri="urn:scte:dash:cc:cea-608:2015" value="CC1=eng;CC3=spa"/>`,
			want: "eng",
		},
		{
			name: "an unrelated accessibility scheme declares nothing",
			acc:  `<Accessibility schemeIdUri="urn:mpeg:dash:role:2011" value="description"/>`,
			want: "",
		},
		{
			name: "no accessibility element at all",
			acc:  ``,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			man, err := parseMPD(t, mpd(tc.acc))
			if err != nil {
				t.Fatal(err)
			}
			if man.CaptionLang != tc.want {
				t.Errorf("CaptionLang = %q, want %q", man.CaptionLang, tc.want)
			}
		})
	}
}

// mediaPresentationDuration used to be parked in a package-level global between
// Parse and expandTemplate, on the assumption that parsing is never concurrent.
// It is: the manager runs several downloads at once. Two manifests parsed
// together would trade durations, and the loser computed its segment count from
// the other's length — a one-hour video quietly truncated to one minute, with
// no error anywhere. Run under -race this also proves the global is gone.
func TestConcurrentParsesDoNotShareDuration(t *testing.T) {
	mpd := func(dur string) string {
		return `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="` + dur + `">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate startNumber="1" timescale="1" duration="2"
                       initialization="$RepresentationID$/init.mp4"
                       media="$RepresentationID$/$Number$.m4s"/>
      <Representation id="V" codecs="avc1.64001e" bandwidth="300000" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`
	}
	cases := []struct {
		dur      string
		wantSegs int
	}{
		{"PT1H", 1800}, // 3600s / 2s
		{"PT1M", 30},   //   60s / 2s
		{"PT10M", 300},
		{"PT2H", 3600},
	}

	var wg sync.WaitGroup
	errs := make([]error, len(cases))
	got := make([]int, len(cases))
	for i, c := range cases {
		wg.Add(1)
		go func(i int, dur string) {
			defer wg.Done()
			man, err := parseMPD(t, mpd(dur))
			if err != nil {
				errs[i] = err
				return
			}
			got[i] = len(man.Video.Segments)
		}(i, c.dur)
	}
	wg.Wait()

	for i, c := range cases {
		if errs[i] != nil {
			t.Fatalf("%s: %v", c.dur, errs[i])
		}
		if got[i] != c.wantSegs {
			t.Errorf("%s: got %d segments, want %d — a concurrent parse leaked its duration in",
				c.dur, got[i], c.wantSegs)
		}
	}
}

// The duration is also published, so a caller can size the download up front.
func TestManifestReportsDuration(t *testing.T) {
	man, err := parseMPD(t, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT1H30M">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate startNumber="1" timescale="1" duration="2"
                       initialization="$RepresentationID$/init.mp4"
                       media="$RepresentationID$/$Number$.m4s"/>
      <Representation id="V" codecs="avc1.64001e" bandwidth="300000" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`)
	if err != nil {
		t.Fatal(err)
	}
	if want := 90 * time.Minute; man.Duration != want {
		t.Errorf("Duration = %v, want %v", man.Duration, want)
	}
}

// The DASH segment loop is wired to the same shared ceiling as HLS and the
// engine. Wiring that looks right is not proof, so measure it.
func TestAssembleTrackRespectsTheSpeedLimit(t *testing.T) {
	const (
		segments = 16
		segSize  = 32 << 10 // 512 KB in total
		limit    = 512 << 10
	)
	blob := strings.Repeat("x", segSize)
	mux := http.NewServeMux()
	mux.HandleFunc("/v-init.mp4", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "") })
	for i := 1; i <= segments; i++ {
		mux.HandleFunc(fmt.Sprintf("/v-%d.m4s", i), func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, blob)
		})
	}
	mux.HandleFunc("/m.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT%dS">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="v" bandwidth="1200000" width="1280" height="720" codecs="avc1.640028">
        <SegmentTemplate media="v-$Number$.m4s" initialization="v-init.mp4" startNumber="1"
                         timescale="1" duration="1"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, segments)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	run := func(t *testing.T, bps int64) time.Duration {
		t.Helper()
		c := NewClient(srv.Client(), nil)
		c.SetLimiter(ratelimit.New(bps))
		man, err := c.Parse(context.Background(), srv.URL+"/m.mpd")
		if err != nil {
			t.Fatal(err)
		}
		if n := len(man.Video.Segments); n != segments {
			t.Fatalf("got %d segments, want %d", n, segments)
		}
		out := filepath.Join(t.TempDir(), "v.m4s")
		start := time.Now()
		if err := c.AssembleTrack(context.Background(), man.Video, out,
			DownloadOptions{Dir: t.TempDir(), Conns: 6}, nil); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		fi, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != segments*segSize {
			t.Fatalf("assembled %d bytes, want %d", fi.Size(), segments*segSize)
		}
		return elapsed
	}

	limited := run(t, limit)
	if limited < 500*time.Millisecond {
		t.Errorf("512 KB through a 512 KB/s ceiling took %v — the limit never reached AssembleTrack", limited)
	}
	unlimited := run(t, 0)
	if unlimited > limited/2 {
		t.Errorf("unlimited run took %v vs %v limited; the comparison proves nothing", unlimited, limited)
	}
}
