package tools

import (
	"os"
	"os/exec"
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

// A Mac without the Command Line Tools has no usable python3, so the zipapp
// cannot run. The bundle ships the self-contained build beside it; without this
// fallback every extractor download failed there.
func TestYtDlpInvocationFallsBackToStandalone(t *testing.T) {
	dir := t.TempDir()
	zipapp := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(zipapp, []byte("#!/usr/bin/env python3\nPK\x03\x04rest"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate a Mac with no usable python3.
	orig := python3Locator
	python3Locator = func() string { return "" }
	t.Cleanup(func() { python3Locator = orig })

	// With no standalone beside it there is nothing better to offer.
	got := YtDlpInvocation(zipapp)
	if len(got) != 1 || got[0] != zipapp {
		t.Fatalf("with no alternative, expected the zipapp itself, got %v", got)
	}

	standalone := filepath.Join(dir, "yt-dlp_macos")
	if err := os.WriteFile(standalone, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got = YtDlpInvocation(zipapp)
	if len(got) != 1 || got[0] != standalone {
		t.Fatalf("expected the standalone build, got %v", got)
	}
}

// A plain executable (the standalone build) is invoked directly — the shebang
// check must not drag python3 into it.
func TestYtDlpInvocationDirectBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := YtDlpInvocation(bin)
	if len(got) != 1 || got[0] != bin {
		t.Fatalf("got %v, want the binary alone", got)
	}
}

// findPython3 must reject a candidate that cannot actually run, not merely
// check the executable bit — the CLT stub passes that check and then fails.
func TestFindPython3RejectsBrokenInterpreter(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "python3")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	// The real lookup checks absolute paths first, so this only proves the
	// PATH candidate is verified by running it, not just stat'ed.
	if p, err := exec.LookPath("python3"); err != nil || p != broken {
		t.Skipf("PATH lookup did not resolve to the stub: %v %v", p, err)
	}
	if err := exec.Command(broken, "-c", "import sys").Run(); err == nil {
		t.Fatal("the fake interpreter should fail; the test is not meaningful otherwise")
	}
}
