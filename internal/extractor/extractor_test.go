package extractor

import "testing"

func TestQualityChoices(t *testing.T) {
	in := &Info{
		Title: "Clip",
		Formats: []Format{
			{ID: "sb", Height: 0, VCodec: "none", ACodec: "none"},                 // storyboard, ignored
			{ID: "140", ACodec: "mp4a.40.2", VCodec: "none", Filesize: 3_000_000}, // audio only
			{ID: "137", Height: 1080, FPS: 30, VCodec: "avc1", ACodec: "none", Filesize: 90_000_000},
			{ID: "248", Height: 1080, FPS: 30, VCodec: "vp9", ACodec: "none", Filesize: 70_000_000},
			{ID: "299", Height: 1080, FPS: 60, VCodec: "avc1", ACodec: "none", Filesize: 120_000_000},
			{ID: "136", Height: 720, FPS: 30, VCodec: "avc1", ACodec: "none", Filesize: 45_000_000},
			{ID: "18", Height: 360, FPS: 30, VCodec: "avc1", ACodec: "mp4a", Filesize: 10_000_000}, // progressive
		},
	}
	got := in.QualityChoices()

	// Expect 1080p60, 1080p, 720p, 360p, Audio only — in that order.
	labels := make([]string, len(got))
	for i, c := range got {
		labels[i] = c.Label
	}
	want := []string{"1080p60", "1080p", "720p", "360p", "Audio only"}
	if len(labels) != len(want) {
		t.Fatalf("got %v, want %v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("choice %d = %q, want %q (all: %v)", i, labels[i], want[i], labels)
		}
	}
	// video choices carry a yt-dlp selector; audio row carries ba/bestaudio
	for _, c := range got {
		if c.Kind == "audio" {
			if c.ID != "ba/bestaudio" {
				t.Errorf("audio selector = %q", c.ID)
			}
			continue
		}
		if !contains(c.ID, "height<=") {
			t.Errorf("%s selector missing height cap: %q", c.Label, c.ID)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
