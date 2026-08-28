package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin is a tiny shell script that stands in for the yt-dlp_macos binary:
// `--version` prints the tag so tools.Version() has something to read.
func fakeBin(tag string) []byte {
	return []byte("#!/bin/sh\necho " + tag + "\n")
}

func newReleaseServer(t *testing.T, tag string, bin []byte, tamperSum bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(bin)
	hexsum := hex.EncodeToString(sum[:])
	if tamperSum {
		hexsum = strings.Repeat("0", 64)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/yt-dlp/yt-dlp/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/yt-dlp/yt-dlp/releases/download/"+tag+"/SHA2-256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hexsum, ytDlpAsset)
	})
	mux.HandleFunc("/yt-dlp/yt-dlp/releases/download/"+tag+"/"+ytDlpAsset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(bin)
	})
	return httptest.NewServer(mux)
}

func withServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oa, od := githubAPI, githubDL
	githubAPI, githubDL = srv.URL, srv.URL
	t.Cleanup(func() { githubAPI, githubDL = oa, od })
}

func TestCheckYtDlp(t *testing.T) {
	srv := newReleaseServer(t, "2099.01.01", fakeBin("2099.01.01"), false)
	defer srv.Close()
	withServer(t, srv)

	st, err := CheckYtDlp(context.Background(), Set{}, "stable")
	if err != nil {
		t.Fatalf("CheckYtDlp: %v", err)
	}
	if st.Latest != "2099.01.01" {
		t.Fatalf("latest = %q", st.Latest)
	}
	if !st.UpdateAvailable {
		t.Fatalf("expected update available (no local binary)")
	}
}

func TestUpdateYtDlpInstallsAndVerifies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bin := fakeBin("2099.01.01")
	srv := newReleaseServer(t, "2099.01.01", bin, false)
	defer srv.Close()
	withServer(t, srv)

	from, to, err := UpdateYtDlp(context.Background(), "stable")
	if err != nil {
		t.Fatalf("UpdateYtDlp: %v", err)
	}
	if from != "" || to != "2099.01.01" {
		t.Fatalf("from=%q to=%q", from, to)
	}
	dest := filepath.Join(home, "Library", "Application Support", "MacDM", "bin", "yt-dlp")
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("dest not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind")
	}
}

func TestUpdateYtDlpRejectsBadChecksum(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	srv := newReleaseServer(t, "2099.02.02", fakeBin("2099.02.02"), true) // tampered SUMS
	defer srv.Close()
	withServer(t, srv)

	if _, _, err := UpdateYtDlp(context.Background(), "stable"); err == nil ||
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	dest := filepath.Join(home, "Library", "Application Support", "MacDM", "bin", "yt-dlp")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("bad binary was installed anyway")
	}
}
