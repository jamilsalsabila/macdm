// Package subs assembles subtitle segments into one subtitle file.
//
// HLS (and segmented DASH) deliver WebVTT in pieces, and the pieces cannot just
// be concatenated: every segment repeats the `WEBVTT` header, and each carries
// its own X-TIMESTAMP-MAP saying where its local clock sits on the presentation
// timeline. Naive concatenation yields a file players reject, with every cue
// timed from zero.
package subs

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// cueTiming matches a WebVTT timing line, capturing both timestamps and any
// trailing cue settings ("align:start position:10%").
var cueTiming = regexp.MustCompile(
	`^((?:\d{2,}:)?\d{2}:\d{2}\.\d{3})\s*-->\s*((?:\d{2,}:)?\d{2}:\d{2}\.\d{3})(.*)$`)

// timestampMap matches X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000
// (the two fields may appear in either order).
var (
	mpegtsField = regexp.MustCompile(`MPEGTS:(\d+)`)
	localField  = regexp.MustCompile(`LOCAL:((?:\d{2,}:)?\d{2}:\d{2}\.\d{3})`)
)

// MergeVTT joins WebVTT segment payloads, in playback order, into a single
// well-formed file: one header, every cue shifted onto the presentation
// timeline, and cues repeated across a segment boundary emitted once.
func MergeVTT(parts [][]byte) []byte {
	var out bytes.Buffer
	out.WriteString("WEBVTT\n")

	seen := map[string]bool{}
	for _, raw := range parts {
		offset, body := splitHeader(raw)
		for _, block := range splitBlocks(body) {
			cue, ok := shiftBlock(block, offset)
			if !ok {
				continue // not a cue (NOTE/STYLE/stray text) — drop it
			}
			// A cue overlapping a segment boundary is repeated in both
			// segments; emitting it twice makes players show it twice.
			if seen[cue] {
				continue
			}
			seen[cue] = true
			out.WriteString("\n")
			out.WriteString(cue)
			out.WriteString("\n")
		}
	}
	return out.Bytes()
}

// splitHeader consumes the WEBVTT header block, returning the presentation
// offset in milliseconds and the remaining body.
//
// The offset is MPEGTS/90000 (the 90kHz MPEG-TS clock) minus LOCAL: cue times
// are written against the local clock, so this is what puts them where they
// actually belong.
func splitHeader(raw []byte) (offsetMS int64, body string) {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	s = strings.TrimPrefix(s, "\ufeff") // some servers prepend a BOM

	head, rest, found := strings.Cut(s, "\n\n")
	if !found {
		// No blank line: either a header with nothing after it, or a bare body.
		if strings.HasPrefix(strings.TrimSpace(s), "WEBVTT") {
			return 0, ""
		}
		return 0, s
	}
	if !strings.HasPrefix(strings.TrimSpace(head), "WEBVTT") {
		return 0, s // no header at all; treat everything as body
	}

	for _, ln := range strings.Split(head, "\n") {
		if !strings.Contains(ln, "X-TIMESTAMP-MAP") {
			continue
		}
		var mpegts int64
		var local int64
		if m := mpegtsField.FindStringSubmatch(ln); m != nil {
			mpegts, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := localField.FindStringSubmatch(ln); m != nil {
			local, _ = parseTimestamp(m[1])
		}
		offsetMS = mpegts*1000/90000 - local
	}
	return offsetMS, rest
}

// splitBlocks divides a VTT body into cue blocks on blank lines.
func splitBlocks(body string) []string {
	var out []string
	for _, b := range strings.Split(body, "\n\n") {
		if b = strings.Trim(b, "\n"); strings.TrimSpace(b) != "" {
			out = append(out, b)
		}
	}
	return out
}

// shiftBlock rewrites a cue block's timing line by offsetMS. Reports false when
// the block holds no timing line (a NOTE or STYLE section).
func shiftBlock(block string, offsetMS int64) (string, bool) {
	lines := strings.Split(block, "\n")
	for i, ln := range lines {
		m := cueTiming.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		start, err1 := parseTimestamp(m[1])
		end, err2 := parseTimestamp(m[2])
		if err1 != nil || err2 != nil {
			return "", false
		}
		lines[i] = formatTimestamp(start+offsetMS) + " --> " + formatTimestamp(end+offsetMS) + m[3]
		return strings.Join(lines, "\n"), true
	}
	return "", false
}

// parseTimestamp reads HH:MM:SS.mmm or MM:SS.mmm into milliseconds.
func parseTimestamp(s string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad timestamp %q", s)
	}
	var h int64
	if len(parts) == 3 {
		v, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
		h = v
		parts = parts[1:]
	}
	m, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	sec, ms, _ := strings.Cut(parts[1], ".")
	sv, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return 0, err
	}
	mv, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return 0, err
	}
	return ((h*60+m)*60+sv)*1000 + mv, nil
}

// formatTimestamp renders milliseconds as HH:MM:SS.mmm, clamping negatives to
// zero — a timestamp map can otherwise push the first cue before the start.
func formatTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// assOverride matches an ASS override block like {\an7}. ffmpeg's CEA-608
// decoder emits them for caption positioning, but an SRT player has no idea
// what they mean and shows them as literal text.
var assOverride = regexp.MustCompile(`\{\\[^}]*\}`)

// CleanSRT tidies the SRT ffmpeg produces from embedded closed captions:
// positioning codes are dropped, and a cue left empty by that is removed along
// with its now-dangling index. Font tags are kept — players understand them and
// 608 uses colour to tell speakers apart.
func CleanSRT(data []byte) []byte {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var out []string
	n := 0
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		if len(lines) < 3 {
			continue // an SRT cue is index, timing, then at least one line
		}
		var body []string
		for _, ln := range lines[2:] {
			ln = strings.TrimSpace(assOverride.ReplaceAllString(ln, ""))
			if ln != "" {
				body = append(body, ln)
			}
		}
		if len(body) == 0 {
			continue
		}
		n++
		out = append(out, fmt.Sprintf("%d\n%s\n%s", n, lines[1], strings.Join(body, "\n")))
	}
	if len(out) == 0 {
		return nil
	}
	return []byte(strings.Join(out, "\n\n") + "\n")
}
