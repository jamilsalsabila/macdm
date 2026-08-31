package subs

import (
	"strings"
	"testing"
)

func seg(s string) []byte { return []byte(strings.TrimLeft(s, "\n")) }

// The basic failure this package exists to prevent: concatenating segments
// naively repeats the WEBVTT header and leaves every segment timed from zero.
func TestMergeStripsRepeatedHeadersAndKeepsOne(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT

00:00:01.000 --> 00:00:02.000
first
`),
		seg(`
WEBVTT

00:00:03.000 --> 00:00:04.000
second
`),
	}))
	if !strings.HasPrefix(out, "WEBVTT\n") {
		t.Fatalf("output must start with a WEBVTT header:\n%s", out)
	}
	if n := strings.Count(out, "WEBVTT"); n != 1 {
		t.Fatalf("want exactly 1 WEBVTT header, got %d:\n%s", n, out)
	}
	for _, want := range []string{"first", "second",
		"00:00:01.000 --> 00:00:02.000", "00:00:03.000 --> 00:00:04.000"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// X-TIMESTAMP-MAP is what puts a segment's local clock on the presentation
// timeline. Ignoring it stacks every segment at the start of the video.
func TestMergeAppliesTimestampMap(t *testing.T) {
	// MPEGTS 900000 / 90000 = 10s; LOCAL 0 => shift +10s.
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT
X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000

00:00:01.500 --> 00:00:03.000
shifted
`),
	}))
	if !strings.Contains(out, "00:00:11.500 --> 00:00:13.000") {
		t.Fatalf("timestamp map not applied:\n%s", out)
	}
}

func TestMergeTimestampMapWithNonZeroLocal(t *testing.T) {
	// MPEGTS 90000 (1s) minus LOCAL 30s => -29s, clamped at zero for the start.
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT
X-TIMESTAMP-MAP=LOCAL:00:00:30.000,MPEGTS:2700000

00:00:31.000 --> 00:00:32.000
one second in
`),
	}))
	// 2700000/90000 = 30s; 30 - 30 = 0 offset.
	if !strings.Contains(out, "00:00:31.000 --> 00:00:32.000") {
		t.Fatalf("offset should be zero here:\n%s", out)
	}
}

func TestMergeClampsNegativeTimestamps(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT
X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:10.000

00:00:01.000 --> 00:00:12.000
straddles zero
`),
	}))
	if !strings.Contains(out, "00:00:00.000 --> 00:00:02.000") {
		t.Fatalf("negative start not clamped:\n%s", out)
	}
}

// A cue spanning a segment boundary appears in both segments.
func TestMergeDeduplicatesBoundaryCues(t *testing.T) {
	dup := `
WEBVTT

00:00:09.000 --> 00:00:11.000
spans the boundary
`
	out := string(MergeVTT([][]byte{seg(dup), seg(dup)}))
	if n := strings.Count(out, "spans the boundary"); n != 1 {
		t.Fatalf("duplicate cue emitted %d times:\n%s", n, out)
	}
}

func TestMergeKeepsCueSettingsAndIdentifiers(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT

cue-42
00:00:01.000 --> 00:00:02.000 align:start position:10%
styled
`),
	}))
	if !strings.Contains(out, "cue-42") {
		t.Errorf("cue identifier dropped:\n%s", out)
	}
	if !strings.Contains(out, "align:start position:10%") {
		t.Errorf("cue settings dropped:\n%s", out)
	}
}

func TestMergeHandlesHourlessAndHourTimestamps(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT

01:02.500 --> 01:04.000
short form
`),
		seg(`
WEBVTT

01:00:00.000 --> 01:00:02.000
long form
`),
	}))
	if !strings.Contains(out, "00:01:02.500 --> 00:01:04.000") {
		t.Errorf("MM:SS.mmm form not normalised:\n%s", out)
	}
	if !strings.Contains(out, "01:00:00.000 --> 01:00:02.000") {
		t.Errorf("HH:MM:SS.mmm form mangled:\n%s", out)
	}
}

func TestMergeSkipsNoteAndStyleBlocks(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg(`
WEBVTT

NOTE this is a comment
that spans lines

STYLE
::cue { color: yellow }

00:00:01.000 --> 00:00:02.000
real cue
`),
	}))
	if strings.Contains(out, "this is a comment") || strings.Contains(out, "::cue") {
		t.Errorf("non-cue blocks should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "real cue") {
		t.Errorf("real cue lost:\n%s", out)
	}
}

func TestMergeToleratesCRLFAndBOM(t *testing.T) {
	crlf := []byte("\ufeffWEBVTT\r\n\r\n00:00:01.000 --> 00:00:02.000\r\nwindows\r\n")
	out := string(MergeVTT([][]byte{crlf}))
	if !strings.Contains(out, "00:00:01.000 --> 00:00:02.000") || !strings.Contains(out, "windows") {
		t.Fatalf("CRLF/BOM segment not parsed:\n%q", out)
	}
	if strings.Contains(out, "\ufeff") {
		t.Errorf("BOM leaked into the output")
	}
}

func TestMergeEmptyAndHeaderOnlySegments(t *testing.T) {
	out := string(MergeVTT([][]byte{
		seg("\nWEBVTT\n"),
		[]byte(""),
		seg(`
WEBVTT

00:00:05.000 --> 00:00:06.000
only cue
`),
	}))
	if n := strings.Count(out, "WEBVTT"); n != 1 {
		t.Fatalf("header count = %d:\n%s", n, out)
	}
	if !strings.Contains(out, "only cue") {
		t.Fatalf("cue lost among empty segments:\n%s", out)
	}
}

func TestMergeNoSegmentsStillValid(t *testing.T) {
	if got := string(MergeVTT(nil)); got != "WEBVTT\n" {
		t.Fatalf("want a bare valid header, got %q", got)
	}
}

func TestParseTimestampRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "abc", "1:2", "00:00:00", "::", "00:xx.000"} {
		if _, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q) should fail", bad)
		}
	}
}
