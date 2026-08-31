package diskspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAvailIsPlausible(t *testing.T) {
	got, err := Avail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Fatalf("Avail = %d, want a positive figure on a mounted volume", got)
	}
}

// The destination file does not exist when the check runs — that is the entire
// point of checking early — so the nearest existing directory must be measured.
func TestAvailMeasuresNearestExistingAncestor(t *testing.T) {
	dir := t.TempDir()
	want, err := Avail(dir)
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(dir, "not", "created", "yet", "movie.mp4")
	got, err := Avail(deep)
	if err != nil {
		t.Fatalf("a path that does not exist yet must still be measurable: %v", err)
	}
	if got != want {
		t.Errorf("Avail(%q) = %d, want %d — the same volume", deep, got, want)
	}
}

// The bug this guards against: scratch and output are usually the same disk, so
// two requirements that each fit on their own can still be impossible together.
func TestCheckAllSumsRequirementsOnOneVolume(t *testing.T) {
	dir := t.TempDir()
	avail, err := Avail(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ReserveOn(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Two thirds each: either alone fits, the pair cannot.
	half := (avail - res) * 2 / 3
	if half <= 0 {
		t.Skip("volume too full to construct the case")
	}
	if err := Check(filepath.Join(dir, "a"), half); err != nil {
		t.Fatalf("one requirement of %s alone should fit: %v", HumanBytes(half), err)
	}
	err = CheckAll(
		Need{Path: filepath.Join(dir, "scratch", "seg"), Bytes: half},
		Need{Path: filepath.Join(dir, "out.mp4"), Bytes: half},
	)
	if err == nil {
		t.Fatal("two requirements summing past the free space must be refused")
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("want a *diskspace.Error, got %T", err)
	}
	if de.Need != 2*half {
		t.Errorf("Need = %d, want the sum %d", de.Need, 2*half)
	}
}

// Refusing a download because its size is unknown would be worse than the bug.
func TestCheckIgnoresUnknownSizes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int64{0, -1} {
		if err := Check(filepath.Join(dir, "x"), n); err != nil {
			t.Errorf("Check(%d) = %v, want nil: an unknown size must not block a download", n, err)
		}
	}
}

// Same reasoning for a path we cannot measure at all.
func TestCheckIgnoresUnmeasurablePaths(t *testing.T) {
	if err := Check("/nonexistent-volume-\x00-bad", 1<<20); err != nil {
		t.Errorf("an unmeasurable path must not block a download, got %v", err)
	}
}

func TestReserveIsKeptFree(t *testing.T) {
	dir := t.TempDir()
	avail, err := Avail(dir)
	if err != nil {
		t.Fatal(err)
	}
	Reserve, err := ReserveOn(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly all of it: fits on paper, refused because it would leave nothing.
	if err := Check(filepath.Join(dir, "x"), avail); err == nil {
		t.Error("a request for every free byte must be refused, to leave headroom")
	}
	if err := Check(filepath.Join(dir, "x"), avail-Reserve); err != nil {
		t.Errorf("a request leaving exactly the reserve should pass: %v", err)
	}
}

func TestErrorMessageNamesTheNumbers(t *testing.T) {
	e := &Error{Path: "/Users/x/Downloads", Need: 8 << 30, Avail: 3 << 30, Reserve: MaxReserve}
	msg := e.Error()
	for _, want := range []string{"/Users/x/Downloads", "8.0 GB", "3.0 GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should mention %q", msg, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{-5, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KB"},
		{3 << 30, "3.0 GB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Two volumes must be tallied apart. /dev is its own filesystem on macOS, so it
// stands in for a download whose destination is an external drive.
func TestCheckAllSeparatesVolumes(t *testing.T) {
	if _, err := os.Stat("/dev"); err != nil {
		t.Skip("no /dev to compare against")
	}
	a, errA := volumeID(t.TempDir())
	b, errB := volumeID("/dev")
	if errA != nil || errB != nil {
		t.Skip("cannot identify both volumes")
	}
	if a == b {
		t.Skip("the temp dir and /dev share a volume here")
	}
}

// A flat reserve is nonsense on a small volume: 512 MB held back on a 60 MB
// USB stick or disk image refuses every download to it, including ones that
// plainly fit. Real-world case that reached a user: a 10 MB file was refused
// onto a volume with 58 MB free.
func TestReserveScalesWithTheVolume(t *testing.T) {
	cases := []struct {
		name  string
		total int64
		want  int64
	}{
		{"a 60 MB disk image", 60 << 20, 3 << 20},
		{"a 1 GB USB stick", 1 << 30, (1 << 30) / reserveFraction},
		{"a 113 GB startup disk", 113 << 30, MaxReserve},
		{"an 8 TB archive drive", 8 << 40, MaxReserve},
		{"an unmeasurable volume", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reserveFor(c.total)
			if got != c.want {
				t.Errorf("reserveFor(%s) = %s, want %s",
					HumanBytes(c.total), HumanBytes(got), HumanBytes(c.want))
			}
			if c.total > 0 && got >= c.total {
				t.Errorf("the reserve (%s) swallows the whole volume (%s)",
					HumanBytes(got), HumanBytes(c.total))
			}
		})
	}
}

// The concrete regression: a file that fits comfortably must be allowed even
// when the volume is far smaller than MaxReserve.
func TestSmallFileFitsOnASmallVolume(t *testing.T) {
	const total = 60 << 20
	const avail = 58 << 20
	res := reserveFor(total)
	if need := int64(10 << 20); need+res > avail {
		t.Errorf("10 MB onto a 60 MB volume with 58 MB free was refused (reserve %s)",
			HumanBytes(res))
	}
}
