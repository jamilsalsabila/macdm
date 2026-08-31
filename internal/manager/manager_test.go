package manager

import (
	"path/filepath"
	"strings"
	"testing"
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
