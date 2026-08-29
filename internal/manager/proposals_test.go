package manager

import (
	"testing"
	"time"

	"macdm/internal/config"
	"macdm/internal/engine"
	"macdm/internal/store"
)

func testMgr(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return New(Config{
		DownloadDir: t.TempDir(), MaxActive: 2,
		PromptTimeoutSec: 300,
		Engine:           engine.Config{MaxConns: 4, MinChunk: 1 << 20},
	}, st)
}

// Regression: Reject held hub.mu across broadcast() which re-locks it → deadlock.
func TestRejectDoesNotDeadlock(t *testing.T) {
	m := testMgr(t)
	// a subscriber so Propose keeps the proposal pending instead of auto-accepting
	ch, cancel := m.Subscribe()
	defer cancel()
	go func() {
		for range ch {
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			p := m.Propose("https://example.com/thing-"+string(rune('a'+i))+".bin", nil, "")
			if err := m.Reject(p.ID); err != nil {
				t.Errorf("reject: %v", err)
			}
		}
		// the mutex must still be usable
		_ = m.PendingProposals()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: Propose/Reject cycle hung")
	}
}

var _ = config.Version
