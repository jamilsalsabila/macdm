package manager

import (
	"os"
	"path/filepath"
	"testing"

	"macdm/internal/engine"
	"macdm/internal/store"
)

// Scratch dirs of unfinished jobs must survive a daemon restart — that is what
// lets a stream/extract resume continue instead of re-downloading from zero.
// Orphans and finished jobs' dirs must still be pruned.
func TestPruneWorkDirsKeepsUnfinishedJobs(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	st, err := store.Open(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	for _, j := range []*store.Job{
		{ID: "paused-job", URL: "http://x/a", Filename: "a", Status: store.StatusPaused},
		{ID: "errored-job", URL: "http://x/b", Filename: "b", Status: store.StatusError},
		{ID: "done-job", URL: "http://x/c", Filename: "c", Status: store.StatusCompleted},
	} {
		if err := st.Put(j); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"paused-job", "errored-job", "done-job", "orphan-job"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name, "seg-000000"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	New(Config{DownloadDir: dir, WorkDir: work, MaxActive: 2,
		Engine: engine.Config{MaxConns: 4, MinChunk: 1 << 20}}, st)

	for _, name := range []string{"paused-job", "errored-job"} {
		if _, err := os.Stat(filepath.Join(work, name, "seg-000000")); err != nil {
			t.Errorf("scratch for unfinished job %q was wiped — resume would restart from zero", name)
		}
	}
	for _, name := range []string{"done-job", "orphan-job"} {
		if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Errorf("scratch for %q should have been pruned", name)
		}
	}
}
