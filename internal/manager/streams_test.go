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
