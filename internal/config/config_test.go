package config

import (
	"os"
	"path/filepath"
	"testing"
)

// write a config.json into a throwaway HOME and load it.
func loadWith(t *testing.T, json string) Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "Library", "Application Support", "MacDM")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if json != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(json), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Load()
}

// config.json is hand-editable and drives goroutine and socket counts, so the
// same 1–64 limit /api/jobs/{id}/conns enforces has to apply here too.
func TestLoadClampsCounts(t *testing.T) {
	c := loadWith(t, `{"max_conns": 100000, "max_active": 5000}`)
	if c.MaxConns != 64 {
		t.Errorf("MaxConns = %d, want it clamped to 64", c.MaxConns)
	}
	if c.MaxActive != 32 {
		t.Errorf("MaxActive = %d, want it clamped to 32", c.MaxActive)
	}
}

func TestLoadDefaults(t *testing.T) {
	c := loadWith(t, "")
	if c.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", c.Addr, DefaultAddr)
	}
	if c.MaxConns != 8 || c.MaxActive != 4 || c.PromptTimeoutSec != 8 {
		t.Errorf("bad defaults: conns=%d active=%d prompt=%d",
			c.MaxConns, c.MaxActive, c.PromptTimeoutSec)
	}
	if c.DownloadDir == "" {
		t.Error("DownloadDir should default to ~/Downloads/MacDM")
	}
	if !c.AutoUpdateYtDlpEnabled() {
		t.Error("yt-dlp auto-update should default to on")
	}
	if c.YtDlpChannelName() != "nightly" {
		t.Errorf("channel = %q, want nightly", c.YtDlpChannelName())
	}
}

func TestLoadKeepsSaneValues(t *testing.T) {
	c := loadWith(t, `{"max_conns": 16, "max_active": 2, "ytdlp_channel": "stable"}`)
	if c.MaxConns != 16 || c.MaxActive != 2 {
		t.Errorf("in-range values were altered: conns=%d active=%d", c.MaxConns, c.MaxActive)
	}
	if c.YtDlpChannelName() != "stable" {
		t.Errorf("channel = %q, want stable", c.YtDlpChannelName())
	}
}

func TestSaveRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	on := false
	if err := Save(Config{MaxConns: 12, CookiesFrom: "firefox", AutoUpdateYtDlp: &on}); err != nil {
		t.Fatal(err)
	}
	c := Load()
	if c.MaxConns != 12 || c.CookiesFrom != "firefox" || c.AutoUpdateYtDlpEnabled() {
		t.Fatalf("round-trip lost data: %+v", c)
	}
	if _, err := os.Stat(ConfigPath() + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind by Save")
	}
}
