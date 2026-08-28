// Package tools locates the external binaries MacDM shells out to: ffmpeg (for
// muxing assembled streams) and yt-dlp (the extractor for page URLs).
//
// Resolution order for each: an explicit path from config, then MacDM's own
// managed bin dir (~/Library/Application Support/MacDM/bin), then $PATH, then a
// few well-known locations Homebrew and pipx use. The managed yt-dlp copy is
// kept current by update.go (CheckYtDlp / UpdateYtDlp / AutoUpdateLoop).
package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"macdm/internal/config"
)

// Set is the resolved location of each external tool. An empty field means "not
// found"; callers should surface a clear, actionable error.
type Set struct {
	Ffmpeg  string
	Ffprobe string
	YtDlp   string
}

// Resolve finds the tools, preferring explicit config paths.
func Resolve(cfg config.Config) Set {
	return Set{
		Ffmpeg:  find("ffmpeg", cfg.FfmpegPath),
		Ffprobe: find("ffprobe", ""),
		YtDlp:   find("yt-dlp", cfg.YtdlpPath),
	}
}

func find(name, explicit string) string {
	if explicit != "" {
		if isExec(explicit) {
			return explicit
		}
	}
	// MacDM-managed copy.
	managed := filepath.Join(config.SupportDir(), "bin", name)
	if isExec(managed) {
		return managed
	}
	// $PATH.
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	// Common non-PATH spots.
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		"/opt/homebrew/bin/" + name, // Apple Silicon Homebrew
		"/usr/local/bin/" + name,    // Intel Homebrew
		filepath.Join(home, ".local/bin/", name),
		filepath.Join(home, "Library/Python/3.14/bin/", name),
		filepath.Join(home, "Library/Python/3.13/bin/", name),
		filepath.Join(home, "Library/Python/3.12/bin/", name),
	} {
		if isExec(c) {
			return c
		}
	}
	return ""
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// ErrMissing describes which tool could not be found and how to get it.
type ErrMissing struct {
	Tool string
}

func (e ErrMissing) Error() string {
	switch e.Tool {
	case "ffmpeg", "ffprobe":
		return "ffmpeg not found — install with `brew install ffmpeg` or set ffmpeg_path in config.json"
	case "yt-dlp":
		return "yt-dlp not found — install with `brew install yt-dlp` (or `pipx install yt-dlp`) or set ytdlp_path in config.json"
	default:
		return e.Tool + " not found"
	}
}

// RequireFfmpeg / RequireYtDlp return the path or a helpful error.
func (s Set) RequireFfmpeg() (string, error) {
	if s.Ffmpeg == "" {
		return "", ErrMissing{"ffmpeg"}
	}
	return s.Ffmpeg, nil
}

func (s Set) RequireYtDlp() (string, error) {
	if s.YtDlp == "" {
		return "", ErrMissing{"yt-dlp"}
	}
	return s.YtDlp, nil
}

// Version returns the tool's version line for the UI. Tries `--version` (yt-dlp)
// then `-version` (ffmpeg exits non-zero on the double-dash form).
func Version(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	for _, flag := range []string{"--version", "-version"} {
		out, err := exec.CommandContext(ctx, path, flag).Output()
		if err != nil || len(out) == 0 {
			continue
		}
		line := string(out)
		if i := strings.IndexByte(line, '\n'); i > 0 {
			line = line[:i]
		}
		return strings.TrimSpace(line)
	}
	return ""
}
