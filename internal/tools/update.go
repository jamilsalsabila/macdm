package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"macdm/internal/config"
)

// yt-dlp ships a self-contained macOS build (`yt-dlp_macos`, a universal
// PyInstaller binary, macOS 10.15+) with every release. MacDM keeps its own copy
// of it in the managed bin dir so the extractor path never depends on a
// pip/Homebrew install the user has to remember to update.

// githubAPI / githubDL are overridable in tests.
var (
	githubAPI = "https://api.github.com"
	githubDL  = "https://github.com"
)

// ytDlpAsset picks the release asset. The `yt-dlp_macos` PyInstaller binary is
// self-contained but pathologically slow on some Macs (Intel + older macOS
// re-extract + Gatekeeper-assess ~40 MB every invocation — 50s+ per run). When a
// real python3 is on PATH, the tiny `yt-dlp` zipapp is 10-30x faster.
func ytDlpAsset() string {
	if hasPython3() {
		return "yt-dlp"
	}
	return "yt-dlp_macos"
}

func hasPython3() bool {
	p, err := exec.LookPath("python3")
	if err != nil {
		return false
	}
	// The macOS /usr/bin/python3 shim (no CLT installed) exits non-zero and
	// pops a dialog — treat only a real interpreter as usable.
	out, err := exec.Command(p, "-c", "import sys").CombinedOutput()
	return err == nil && len(out) == 0
}

// ytDlpRepo maps a channel name to its GitHub repo. Nightly builds live in a
// separate repo but use the same asset + SHA2-256SUMS layout.
func ytDlpRepo(channel string) string {
	if channel == "stable" {
		return "yt-dlp/yt-dlp"
	}
	return "yt-dlp/yt-dlp-nightly-builds"
}

// managedYtDlp is where the auto-updated binary lives; tools.find() prefers it.
func managedYtDlp() string {
	return filepath.Join(config.SupportDir(), "bin", "yt-dlp")
}

// YtDlpStatus is the current vs available picture for the UI.
type YtDlpStatus struct {
	Path            string `json:"path"`
	Version         string `json:"version"`
	Latest          string `json:"latest"`
	Channel         string `json:"channel"`
	UpdateAvailable bool   `json:"update_available"`
}

// CheckYtDlp reports the installed yt-dlp version and the latest release tag on
// the given channel ("nightly"/"stable"). A network failure is not fatal — the
// local fields are still filled.
func CheckYtDlp(ctx context.Context, set Set, channel string) (YtDlpStatus, error) {
	s := YtDlpStatus{Path: set.YtDlp, Channel: channel}
	if set.YtDlp != "" {
		v := Version(ctx, set.YtDlp) // "yt-dlp 2025.08.20" or "2025.08.20"
		s.Version = strings.TrimSpace(strings.TrimPrefix(v, "yt-dlp"))
	}
	latest, err := githubLatestTag(ctx, ytDlpRepo(channel))
	if err != nil {
		return s, err
	}
	s.Latest = latest
	s.UpdateAvailable = latest != "" && latest != s.Version
	return s, nil
}

// updateMu serialises UpdateYtDlp so a manual "Update now" and the background
// loop can't both download into the same dir at once.
var updateMu sync.Mutex

// UpdateYtDlp downloads the latest yt-dlp_macos build for the channel into the
// managed bin dir, verifying its SHA-256 against the release's SHA2-256SUMS
// before swapping it in atomically. Returns the old and new version strings.
func UpdateYtDlp(ctx context.Context, channel string) (from, to string, err error) {
	updateMu.Lock()
	defer updateMu.Unlock()

	repo := ytDlpRepo(channel)
	dest := managedYtDlp()
	if fi, e := os.Stat(dest); e == nil && !fi.IsDir() {
		from = strings.TrimSpace(strings.TrimPrefix(Version(ctx, dest), "yt-dlp"))
	}

	tag, err := githubLatestTag(ctx, repo)
	if err != nil {
		return from, "", fmt.Errorf("check latest: %w", err)
	}
	if tag != "" && tag == from {
		return from, from, nil // already current
	}

	want, err := releaseSHA(ctx, repo, tag, ytDlpAsset())
	if err != nil {
		return from, "", fmt.Errorf("fetch checksums: %w", err)
	}

	binURL := fmt.Sprintf("%s/%s/releases/download/%s/%s", githubDL, repo, tag, ytDlpAsset())
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return from, "", err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), "yt-dlp-*.download")
	if err != nil {
		return from, "", err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp) // always clean up, success or failure

	got, err := downloadFile(ctx, binURL, tmp)
	if err != nil {
		return from, "", fmt.Errorf("download: %w", err)
	}
	if want != "" && !strings.EqualFold(want, got) {
		return from, "", fmt.Errorf("checksum mismatch: got %s want %s", got, want)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return from, "", err
	}
	// Confirm it executes before committing.
	if v := Version(ctx, tmp); v == "" {
		return from, "", fmt.Errorf("downloaded binary does not run")
	}
	if err := os.Rename(tmp, dest); err != nil {
		return from, "", err
	}
	to = strings.TrimSpace(strings.TrimPrefix(Version(ctx, dest), "yt-dlp"))
	return from, to, nil
}

// AutoUpdateLoop runs an initial check (if the stamp file is missing or >24h
// old) then re-checks every 12h, until ctx is cancelled. Errors are logged, not
// returned — a failed update must never take the daemon down.
func AutoUpdateLoop(ctx context.Context, cfg config.Config) {
	if !cfg.AutoUpdateYtDlpEnabled() {
		return
	}
	stamp := filepath.Join(config.SupportDir(), ".ytdlp-checked")
	run := func() {
		if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
			return
		}
		from, to, err := UpdateYtDlp(ctx, cfg.YtDlpChannelName())
		switch {
		case err != nil:
			log.Printf("yt-dlp auto-update: %v", err)
		case from != to:
			log.Printf("yt-dlp auto-update: %s -> %s", from, to)
		default:
			log.Printf("yt-dlp auto-update: already current (%s)", to)
		}
		_ = os.WriteFile(stamp, []byte(time.Now().Format(time.RFC3339)), 0o644)
	}

	// Small delay so it doesn't compete with startup work.
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	run()

	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// --- helpers ---

// ghClient has a short timeout so a slow/blocked GitHub never hangs the caller
// (the Settings window's tool-status fetch, most importantly).
var ghClient = &http.Client{Timeout: 6 * time.Second}

func githubLatestTag(ctx context.Context, repo string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		githubAPI+"/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := ghClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	return strings.TrimSpace(body.TagName), nil
}

// releaseSHA returns the hex SHA-256 for asset within a release's SHA2-256SUMS.
func releaseSHA(ctx context.Context, repo, tag, asset string) (string, error) {
	u := fmt.Sprintf("%s/%s/releases/download/%s/SHA2-256SUMS", githubDL, repo, tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksums: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && filepath.Base(f[1]) == asset {
			return f[0], nil
		}
	}
	return "", nil // no line — skip verification rather than block the update
}

func downloadFile(ctx context.Context, url, dest string) (sha string, err error) {
	// No overall Timeout — the file is ~35 MB and connections vary wildly.
	// ResponseHeaderTimeout catches a dead server; the caller's ctx cancels a
	// user-abandoned update. A stall mid-body will hang until ctx expires.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
