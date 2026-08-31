package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The unlimited path runs for every block of every download, so it must not
// block and must tolerate a nil bucket.
func TestUnlimitedNeverWaits(t *testing.T) {
	for _, b := range []*Bucket{nil, New(0), New(-1)} {
		start := time.Now()
		for i := 0; i < 1000; i++ {
			if err := b.Wait(context.Background(), 1<<20); err != nil {
				t.Fatalf("unlimited Wait returned %v", err)
			}
		}
		if el := time.Since(start); el > 100*time.Millisecond {
			t.Errorf("1000 unlimited waits took %v, want ~0", el)
		}
	}
}

// The point of the whole package: eight connections sharing one ceiling must
// add up to that ceiling, not to eight times it.
func TestAggregateRateAcrossConnections(t *testing.T) {
	const (
		limit   = 4 << 20 // 4 MB/s
		workers = 8
		block   = 32 << 10
		total   = 4 << 20 // one second's worth
	)
	b := New(limit)
	blocksEach := total / block / workers

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < blocksEach; j++ {
				if err := b.Wait(context.Background(), block); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	rate := float64(total) / elapsed.Seconds()
	// Generous bounds: this is a wall-clock test on a loaded machine. It still
	// fails decisively if the limiter is per-worker (8x) or absent (very fast).
	if rate > limit*1.5 {
		t.Errorf("measured %.2f MB/s through a %d MB/s ceiling (%v for %d MB) — the limit is not shared",
			rate/(1<<20), limit>>20, elapsed, total>>20)
	}
	if rate < limit*0.4 {
		t.Errorf("measured only %.2f MB/s through a %d MB/s ceiling — far slower than asked",
			rate/(1<<20), limit>>20)
	}
}

// An idle download must not bank credit and then blow past the ceiling.
func TestBurstIsCappedAtOneSecond(t *testing.T) {
	const limit = 1 << 20
	b := New(limit)
	// Let it sit far longer than a second's worth of refill.
	b.mu.Lock()
	b.last = time.Now().Add(-30 * time.Second)
	b.mu.Unlock()

	// Two seconds' worth: the first second may come from the capped burst, the
	// second has to be earned.
	start := time.Now()
	if err := b.Wait(context.Background(), 2*limit); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 700*time.Millisecond {
		t.Errorf("2 MB through a 1 MB/s ceiling took %v; a 30s idle banked more than one second of burst", el)
	}
}

// Pausing a download must not wait out a long token debt.
func TestWaitIsCancellable(t *testing.T) {
	b := New(1 << 10) // 1 KB/s: 10 MB would take hours
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := b.Wait(ctx, 10<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("cancellation took %v; a paused download must stop promptly", el)
	}
}

// The user drags the slider mid-download; it has to take effect without a
// restart, in both directions.
func TestSetLimitAppliesImmediately(t *testing.T) {
	b := New(1 << 10) // painfully slow
	if got := b.Limit(); got != 1<<10 {
		t.Fatalf("Limit() = %d, want %d", got, 1<<10)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = b.Wait(context.Background(), 1<<20) // ~17 minutes at 1 KB/s
		done <- time.Since(start)
	}()

	// Lifting the ceiling releases work queued after the change.
	time.Sleep(20 * time.Millisecond)
	b.SetLimit(0)
	if got := b.Limit(); got != 0 {
		t.Errorf("Limit() = %d after SetLimit(0), want 0 (unlimited)", got)
	}
	start := time.Now()
	if err := b.Wait(context.Background(), 8<<20); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("a wait after SetLimit(0) took %v, want ~0", el)
	}

	// The already-sleeping caller keeps its old wait; that is the documented
	// behaviour, so just make sure the test does not leak into other tests.
	select {
	case <-done:
	default:
	}
}

func TestNegativeLimitIsUnlimited(t *testing.T) {
	b := New(-5)
	if b.Limit() != 0 {
		t.Errorf("Limit() = %d, want 0", b.Limit())
	}
}

func TestZeroBytesIsFree(t *testing.T) {
	b := New(1) // 1 byte/s
	start := time.Now()
	for _, n := range []int{0, -1} {
		if err := b.Wait(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("waiting for no bytes took %v", el)
	}
}
