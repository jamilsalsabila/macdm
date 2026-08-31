package store

import (
	"encoding/json"
	"os"
	"path/filepath"
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
