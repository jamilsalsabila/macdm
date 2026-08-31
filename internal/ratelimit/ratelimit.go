// Package ratelimit paces byte transfers against a ceiling the user sets.
//
// The ceiling is deliberately global rather than per-download: "limit MacDM to
// 2 MB/s" is what someone on a shared line means, and a per-download cap would
// let eight queued downloads use eight times the number they typed. One Bucket
// is therefore shared by every connection of every job.
package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Bucket is a token bucket refilled at Limit bytes per second.
//
// Waiters take their tokens on credit: each subtracts what it needs, sees how
// far into debt that puts the bucket, and sleeps exactly long enough for the
// refill to cover it. Because the debt is claimed under the lock and paid
// afterwards, eight connections queue up behind one another instead of all
// waking to fight over the same tokens, and the total rate comes out right
// however many of them there are.
type Bucket struct {
	// limit is read on the hot path for every block of bytes transferred, so
	// the unlimited case must not touch the mutex at all.
	limit atomic.Int64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// New returns a bucket limited to bytesPerSec. Zero or less means unlimited.
func New(bytesPerSec int64) *Bucket {
	b := &Bucket{}
	b.SetLimit(bytesPerSec)
	return b
}

// SetLimit changes the ceiling while transfers are in flight. Zero or less
// removes it. Callers already sleeping keep their existing wait — at most one
// block of bytes is paced by the old figure before the new one takes over.
func (b *Bucket) SetLimit(bytesPerSec int64) {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	b.mu.Lock()
	// Start the new ceiling from an empty-but-not-indebted bucket: carrying a
	// large stale credit over from "unlimited" would let a burst straight
	// through the moment a limit is set.
	if b.limit.Load() != bytesPerSec {
		b.tokens = 0
		b.last = time.Now()
	}
	b.mu.Unlock()
	b.limit.Store(bytesPerSec)
}

// Limit reports the current ceiling in bytes per second; 0 means unlimited.
func (b *Bucket) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit.Load()
}

// Wait blocks until n bytes may be transferred, or ctx ends. A nil Bucket and a
// limit of zero both return immediately, so the unlimited path stays free.
//
// It returns ctx.Err() on cancellation: pausing a download must not be held up
// waiting for tokens that a slow ceiling would take minutes to produce.
func (b *Bucket) Wait(ctx context.Context, n int) error {
	if b == nil || n <= 0 {
		return nil
	}
	limit := b.limit.Load()
	if limit <= 0 {
		return nil
	}

	b.mu.Lock()
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
	}
	b.tokens += now.Sub(b.last).Seconds() * float64(limit)
	b.last = now
	// One second of credit is the most that may accumulate. Without a cap an
	// idle download would bank an unbounded burst and blow straight past the
	// ceiling the moment it resumed.
	if burst := float64(limit); b.tokens > burst {
		b.tokens = burst
	}
	b.tokens -= float64(n)
	var wait time.Duration
	if b.tokens < 0 {
		wait = time.Duration(-b.tokens / float64(limit) * float64(time.Second))
	}
	b.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
