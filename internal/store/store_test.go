package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Close must not return until the flush loop's final write has landed —
// otherwise macdmd can exit with the last status update only in memory.
func TestCloseFlushesBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(&Job{ID: "a", URL: "http://x/y", Filename: "y", Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	// A progress-style update: dirty, but not terminal, so it rides the 1s timer
	// and is still unwritten when we close.
	if _, err := s.Update("a", func(j *Job) { j.DoneBytes = 4242 }); err != nil {
		t.Fatal(err)
	}
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("store file missing after Close: %v", err)
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].DoneBytes != 4242 {
		t.Fatalf("Close did not persist the pending update: %+v", list)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // must not panic on a double close
}

// One unreadable file must not take the whole app down. Writes are atomic so
// this should not happen, but a full disk or a hand-edit can still produce it,
// and refusing to start leaves the menu-bar app unable to connect with nothing
// on screen to explain why.
func TestOpenSurvivesACorruptJobList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte(`[{"id":"abc","url":"https://x/y.bin"`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a corrupt job list must not stop the store from opening: %v", err)
	}
	defer s.Close()

	if n := len(s.List()); n != 0 {
		t.Errorf("expected an empty list, got %d jobs", n)
	}

	// The bad file is kept for recovery rather than silently destroyed.
	entries, _ := os.ReadDir(dir)
	var kept string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "jobs.json.corrupt-") {
			kept = e.Name()
		}
	}
	if kept == "" {
		t.Error("the unreadable file was not kept for recovery")
	}

	// And the store is usable: a new job saves and reloads.
	s.Put(&Job{ID: "new", URL: "https://example.test/a.bin", Status: StatusQueued})
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.Get("new"); err != nil {
		t.Errorf("a job written after the recovery did not survive a reload: %v", err)
	}
}
