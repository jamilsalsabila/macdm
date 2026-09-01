package manager

import (
	"path/filepath"
	"testing"

	"errors"
	"fmt"
	"macdm/internal/diskspace"
)

func TestBitrateBytes(t *testing.T) {
	cases := []struct {
		name    string
		seconds float64
		bps     int
		want    int64
	}{
		// 3 Mbps for an hour: 3e6/8 * 3600.
		{"an hour of 3 Mbps", 3600, 3_000_000, 1_350_000_000},
		{"unknown duration", 0, 3_000_000, 0},
		{"unknown bitrate", 3600, 0, 0},
		{"negative duration", -1, 3_000_000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bitrateBytes(c.seconds, c.bps); got != c.want {
				t.Errorf("bitrateBytes(%v, %d) = %d, want %d", c.seconds, c.bps, got, c.want)
			}
		})
	}
}

// A stream needs room for the segments and for the muxed result at once, since
// the scratch is only cleared after the mux succeeds.
func TestPlanSpaceCountsScratchAndOutputTogether(t *testing.T) {
	dir := t.TempDir()
	avail, err := diskspace.Avail(dir)
	if err != nil {
		t.Skip("cannot measure the volume:", err)
	}
	wd := filepath.Join(dir, "work", "job1")
	dest := filepath.Join(dir, "Movie.mp4")

	res, err := diskspace.ReserveOn(dir)
	if err != nil {
		t.Skip("cannot measure the reserve:", err)
	}
	// Two thirds of the budget: fits once, not twice.
	est := (avail - res) * 2 / 3
	if est <= 0 {
		t.Skip("volume too full to construct the case")
	}
	if err := planSpace(wd, dest, est); err == nil {
		t.Errorf("an estimate of %s needs %s in total and must be refused",
			diskspace.HumanBytes(est), diskspace.HumanBytes(2*est))
	}

	// A third fits twice over.
	small := (avail - res) / 3
	if err := planSpace(wd, dest, small); err != nil {
		t.Errorf("an estimate of %s should fit twice over: %v", diskspace.HumanBytes(small), err)
	}
}

// An unknown size must never turn a download away.
func TestPlanSpaceAllowsUnknownEstimate(t *testing.T) {
	dir := t.TempDir()
	for _, est := range []int64{0, -1} {
		if err := planSpace(dir, filepath.Join(dir, "x.mp4"), est); err != nil {
			t.Errorf("planSpace(est=%d) = %v, want nil", est, err)
		}
	}
}

// A full disk stays full: retrying it three times behind "connection lost —
// retrying" would waste the user's attention and misname the problem.
func TestDiskSpaceFailureIsNotRetried(t *testing.T) {
	noSpace := &diskspace.Error{Path: "/Users/x/Downloads", Need: 8 << 30, Avail: 1 << 30}
	if transientErr(noSpace) {
		t.Error("a disk-space failure must not be retried automatically")
	}
	// It must still be recognised once the engine has wrapped it on the way up.
	wrapped := fmt.Errorf("download %q: %w", "movie.mp4", error(noSpace))
	if transientErr(wrapped) {
		t.Error("a wrapped disk-space failure must not be retried either")
	}
	// Sanity check the other direction: a normal network failure still retries.
	if !transientErr(errors.New("connection reset by peer")) {
		t.Error("an ordinary network error should still be retried")
	}
}

// The progress bar is driven by bytes while the text counts segments, so the
// size estimate has to be right or the two visibly disagree. Dividing the
// running byte count by completed segments counts the partial bytes of
// everything still in flight: measured on a 28 MB stream over eight
// connections, that guessed 247 MB, and the old monotonic clamp then locked the
// guess in — at 97% of segments done the bar still read 12%.
func TestEstimateStreamTotalUsesFinishedSegmentsOnly(t *testing.T) {
	// 60 segments of 500 KB each. Eight workers are mid-flight, so the running
	// byte count runs ahead of what has actually completed.
	const segSize = 500 << 10
	sp := streamProg{
		doneSeg:        10,
		totalSeg:       60,
		completedBytes: 10 * segSize,
		bytes:          10*segSize + 8*(segSize/2), // eight half-finished
	}
	got := estimateStreamTotal(sp, 0)
	want := int64(60 * segSize)
	if got != want {
		t.Errorf("estimate = %s, want %s", humanBytes(got), humanBytes(want))
	}
	// The old formula, for contrast: it would have answered far higher.
	naive := sp.bytes * int64(sp.totalSeg) / int64(sp.doneSeg)
	if naive <= want {
		t.Fatalf("the test case does not reproduce the inflation (naive %s)", humanBytes(naive))
	}
}

// A stale estimate must be revisable, not clamped forever.
func TestEstimateStreamTotalIsRevisable(t *testing.T) {
	sp := streamProg{doneSeg: 20, totalSeg: 100, completedBytes: 20 << 20, bytes: 20 << 20}
	got := estimateStreamTotal(sp, 900<<20) // an earlier, wildly high guess
	if want := int64(100 << 20); got != want {
		t.Errorf("estimate = %s, want %s — an old guess must not be locked in",
			humanBytes(got), humanBytes(want))
	}
}

// Whatever it estimates, the bar must never read past 100%.
func TestEstimateStreamTotalNeverBelowBytesInHand(t *testing.T) {
	// A final segment much larger than the average would otherwise leave the
	// estimate under what is already downloaded.
	sp := streamProg{doneSeg: 9, totalSeg: 10, completedBytes: 9 << 20, bytes: 50 << 20}
	if got := estimateStreamTotal(sp, 0); got < sp.bytes {
		t.Errorf("estimate %s is below the %s already downloaded", humanBytes(got), humanBytes(sp.bytes))
	}
}

// Before any segment finishes there is nothing to divide; keep the previous
// value rather than inventing one.
func TestEstimateStreamTotalWithNothingFinished(t *testing.T) {
	sp := streamProg{doneSeg: 0, totalSeg: 60, completedBytes: 0, bytes: 3 << 20}
	if got := estimateStreamTotal(sp, 0); got != sp.bytes {
		t.Errorf("estimate = %s, want the bytes in hand (%s)", humanBytes(got), humanBytes(sp.bytes))
	}
}
