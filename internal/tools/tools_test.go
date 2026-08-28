package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNearDir(t *testing.T) {
	root := t.TempDir()
	macos := filepath.Join(root, "Contents", "MacOS")
	resbin := filepath.Join(root, "Contents", "Resources", "bin")
	for _, d := range []string{macos, resbin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(macos, "macdm-nmhost")) // sibling of the daemon
	write(filepath.Join(resbin, "ffmpeg"))      // Contents/Resources/bin

	if got := nearDir(macos, "macdm-nmhost"); got != filepath.Join(macos, "macdm-nmhost") {
		t.Errorf("sibling lookup: %q", got)
	}
	if got := nearDir(macos, "ffmpeg"); got != filepath.Clean(filepath.Join(macos, "..", "Resources", "bin", "ffmpeg")) {
		t.Errorf("Resources/bin lookup: %q", got)
	}
	if got := nearDir(macos, "yt-dlp"); got != "" {
		t.Errorf("missing tool should be empty, got %q", got)
	}
}
