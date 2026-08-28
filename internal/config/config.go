// Package config resolves the standard macOS locations MacDM uses and loads an
// optional user config file.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultAddr is the loopback address the daemon listens on.
const DefaultAddr = "127.0.0.1:7345"

// Version is bumped whenever the daemon's wire behaviour changes. The menu-bar
// app compares it against its own build and restarts a mismatched daemon so a
// stale background macdmd never lingers after a rebuild.
const Version = "0.3.2"

// Config is the on-disk settings file (config.json in the support dir). Every
// field has a working default, so the file is optional.
type Config struct {
	Addr        string `json:"addr"`
	DownloadDir string `json:"download_dir"`
	MaxConns    int    `json:"max_conns"`
	MaxActive   int    `json:"max_active"`
	// FfmpegPath / YtdlpPath are consumed in phase 2; parsed now so upgrading
	// does not require touching the file.
	FfmpegPath string `json:"ffmpeg_path"`
	YtdlpPath  string `json:"ytdlp_path"`
	// CookiesFrom names a browser for yt-dlp's --cookies-from-browser when the
	// extractor path needs an authenticated session (e.g. "chrome", "firefox",
	// "safari", "brave", "edge"). Empty disables it.
	CookiesFrom string `json:"cookies_from"`

	// AutoAccept starts caught downloads immediately instead of raising the
	// "New Download" dialog. PromptTimeoutSec is how long a proposal waits for
	// the dialog before auto-accepting anyway.
	AutoAccept       bool `json:"auto_accept"`
	PromptTimeoutSec int  `json:"prompt_timeout_sec"`
}

// SupportDir is ~/Library/Application Support/MacDM, created on demand.
func SupportDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, "Library", "Application Support", "MacDM")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// StorePath is where the job list is persisted.
func StorePath() string { return filepath.Join(SupportDir(), "jobs.json") }

// ConfigPath is the settings file location.
func ConfigPath() string { return filepath.Join(SupportDir(), "config.json") }

// Load reads ConfigPath if present and fills in defaults for everything missing.
func Load() Config {
	c := Config{}
	if data, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.DownloadDir == "" {
		home, _ := os.UserHomeDir()
		c.DownloadDir = filepath.Join(home, "Downloads", "MacDM")
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 8
	}
	if c.MaxActive <= 0 {
		c.MaxActive = 4
	}
	if c.PromptTimeoutSec <= 0 {
		c.PromptTimeoutSec = 8
	}
	_ = os.MkdirAll(c.DownloadDir, 0o755)
	return c
}
