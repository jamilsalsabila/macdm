package manager

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"video.mp4":                "video.mp4",
		"a/b/c.mp4":                "a_b_c.mp4",
		"":                         "",
		"...":                      "",
		"My Video: Part 1 / 2.mp4": "My Video: Part 1 _ 2.mp4",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}

	// The security property: whatever comes out is a single, non-escaping
	// filename component.
	for _, evil := range []string{
		"../../../../etc/passwd", "..\\..\\windows", "  ..evil..  ",
		"/abs/path", "a/../../b", "\x00null",
	} {
		got := safeName(evil)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") ||
			strings.HasPrefix(got, ".") {
			t.Errorf("safeName(%q) = %q — still unsafe", evil, got)
		}
		if got != "" {
			joined := filepath.Join("/downloads", got)
			if filepath.Dir(joined) != "/downloads" {
				t.Errorf("safeName(%q) = %q escapes: %q", evil, got, joined)
			}
		}
	}
}

// Filenames are cut to a byte budget so they stay clear of the 255-byte limit
// on one path component. The cut has to fall on a character boundary: half a
// character is not valid UTF-8 and Finder renders it as mangled text.
//
// This hides easily. The budgets divide evenly by 3 and 4, so pure CJK or pure
// emoji land on a boundary by luck; one ASCII character in front of them is
// enough to break it, which is exactly what a real video title looks like.
func TestNameTruncationCutsWholeCharacters(t *testing.T) {
	long := map[string]string{
		"1 ASCII then CJK":   "A" + strings.Repeat("あ", 120),
		"2 ASCII then emoji": "AB" + strings.Repeat("🔥", 100),
		"mixed title":        "Trying the best kebab 🇬🇷 " + strings.Repeat("日本", 100),
		"pure CJK":           strings.Repeat("あ", 150),
		"pure emoji":         strings.Repeat("🔥", 120),
	}
	for name, in := range long {
		t.Run(name, func(t *testing.T) {
			for fn, out := range map[string]string{
				"sanitize": sanitize(in),
				"safeName": safeName(in),
			} {
				if !utf8.ValidString(out) {
					t.Errorf("%s produced invalid UTF-8 (%d bytes in, %d out)", fn, len(in), len(out))
				}
				if len(out) > 200 {
					t.Errorf("%s left %d bytes, over the budget", fn, len(out))
				}
			}
		})
	}

	// Short names must survive untouched.
	if got := sanitize("clip.mp4"); got != "clip.mp4" {
		t.Errorf("sanitize mangled a short name: %q", got)
	}
	if got := sanitize("Обзор 🇷🇺.mp4"); got != "Обзор 🇷🇺.mp4" {
		t.Errorf("sanitize mangled a short non-Latin name: %q", got)
	}
}
