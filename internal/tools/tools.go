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
	"sync"
	"time"

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
	// MacDM-managed copy (auto-updated by update.go — wins over the bundled seed).
	managed := filepath.Join(config.SupportDir(), "bin", name)
	if isExec(managed) {
		return managed
	}
	// Bundled inside MacDM.app (Contents/Resources/bin), next to the daemon.
	if p := nearExecutable(name); p != "" {
		return p
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

// nearExecutable looks for name alongside the running binary and in a sibling
// Resources/bin — i.e. the layout `app/build.sh` produces:
//
//	MacDM.app/Contents/MacOS/{macdmd,macdm-nmhost}
//	MacDM.app/Contents/Resources/bin/{ffmpeg,yt-dlp}
func nearExecutable(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return nearDir(filepath.Dir(exe), name)
}

// nearDir checks for name in dir and in dir/../Resources/bin.
func nearDir(dir, name string) string {
	for _, c := range []string{
		filepath.Join(dir, name),
		filepath.Join(dir, "..", "Resources", "bin", name),
	} {
		if isExec(c) {
			return filepath.Clean(c)
		}
	}
	return ""
}

// YtDlpInvocation returns the argv prefix for running the resolved yt-dlp. The
// `yt-dlp` release is a zipapp with a `#!/usr/bin/env python3` shebang; a
// GUI-launched daemon has a minimal PATH (no /usr/local/bin), so `env` can't
// find python3 — invoke it as `<python3> <zipapp>` with an explicit interpreter.
// The `yt-dlp_macos` PyInstaller build is a normal executable and runs directly.
func YtDlpInvocation(ytdlpPath string) []string {
	if ytdlpPath == "" {
		return nil
	}
	f, err := os.Open(ytdlpPath)
	if err != nil {
		return []string{ytdlpPath}
	}
	head := make([]byte, 64)
	n, _ := f.Read(head)
	f.Close()
	firstLine := string(head[:n])
	if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	// The zipapp release: `#!/usr/bin/env python3` then a PK zip. Needs an
	// explicit interpreter because a GUI-launched daemon's PATH lacks
	// /usr/local/bin, so `env` can't resolve python3.
	if strings.HasPrefix(firstLine, "#!") && strings.Contains(firstLine, "python") {
		if py := python3Locator(); py != "" {
			return []string{py, ytdlpPath}
		}
		// No usable python3. The bundle ships the self-contained build next to
		// the zipapp for exactly this case — a Mac without the Command Line
		// Tools would otherwise fail every extractor download.
		if alt := filepath.Join(filepath.Dir(ytdlpPath), "yt-dlp_macos"); isExec(alt) {
			return []string{alt}
		}
	}
	return []string{ytdlpPath}
}

// python3Locator is findPython3, indirected so a test can simulate a Mac with
// no usable python3 — the case that matters most and cannot otherwise be
// exercised on a developer machine.
var python3Locator = findPython3

var python3Once struct {
	sync.Once
	path string
}

// findPython3 returns a python3 that actually runs, or "".
//
// Being executable is not enough: on a Mac without the Command Line Tools,
// /usr/bin/python3 is a stub that only prompts to install them, and a
// GUI-launched daemon just gets a failure. So each candidate is executed once
// and the answer cached.
func findPython3() string {
	python3Once.Do(func() {
		cands := []string{
			"/usr/local/bin/python3", "/opt/homebrew/bin/python3", "/usr/bin/python3",
		}
		if p, err := exec.LookPath("python3"); err == nil {
			cands = append(cands, p)
		}
		for _, c := range cands {
			if !isExec(c) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := exec.CommandContext(ctx, c, "-c", "import sys").Run()
			cancel()
			if err == nil {
				python3Once.path = c
				return
			}
		}
	})
	return python3Once.path
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
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
