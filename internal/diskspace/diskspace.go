// Package diskspace answers the question a download manager has to ask before
// it starts rather than after: is there room for this?
//
// The obvious guard does not work. Creating the destination at its final size
// with ftruncate looks like a reservation, but APFS makes the file sparse and
// reserves nothing — a 27 GB download begins happily on a 7 GB disk and dies
// most of the way through, having spent the bandwidth for nothing.
package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// MaxReserve is the most headroom ever held back.
//
// Some headroom matters: filling a startup disk to the last byte wedges the
// whole machine, and no filesystem enjoys being completely full. But a flat
// figure is nonsense on a small volume — 512 MB held back on a 60 MB USB stick
// or disk image refuses every download to it, including ones that plainly fit.
// So the reserve is a fraction of the volume, capped here.
const MaxReserve = 512 << 20 // 512 MB

// reserveFraction of a volume is kept free, up to MaxReserve.
const reserveFraction = 20 // 5%

func reserveFor(total int64) int64 {
	if total <= 0 {
		return 0
	}
	if r := total / reserveFraction; r < MaxReserve {
		return r
	}
	return MaxReserve
}

// ReserveOn reports the headroom kept free on the volume holding path.
func ReserveOn(path string) (int64, error) {
	_, total, err := statVolume(path)
	if err != nil {
		return 0, err
	}
	return reserveFor(total), nil
}

// Need is one requirement: Bytes wanted somewhere under Path. Path does not
// have to exist yet.
type Need struct {
	Path  string
	Bytes int64
}

// Error reports a requirement that does not fit.
type Error struct {
	Path    string // the directory actually measured
	Need    int64
	Avail   int64
	Reserve int64 // headroom held back on that particular volume
}

func (e *Error) Error() string {
	return fmt.Sprintf("not enough free space in %s: needs %s, only %s free (%s is kept in reserve)",
		e.Path, HumanBytes(e.Need), HumanBytes(e.Avail), HumanBytes(e.Reserve))
}

// Avail reports the bytes available to this user on the volume holding path.
// path need not exist: the nearest existing ancestor is measured, which is the
// normal case here because the destination file has not been created yet.
func Avail(path string) (int64, error) {
	avail, _, err := statVolume(path)
	return avail, err
}

// statVolume measures the volume holding path: bytes available to this user,
// and the volume's total size (which sets the reserve).
func statVolume(path string) (avail, total int64, err error) {
	dir, err := existingAncestor(path)
	if err != nil {
		return 0, 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bavail, not Bfree: Bfree also counts the blocks only root may spend.
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}

// Check verifies a single requirement.
func Check(path string, need int64) error {
	return CheckAll(Need{Path: path, Bytes: need})
}

// CheckAll verifies every requirement at once, adding together the ones that
// land on the same volume.
//
// Checking them one at a time would wave through a job needing 4 GB of scratch
// and 4 GB of output on a disk with 5 GB free: each half fits, the pair does
// not. Scratch and destination are usually the same disk, so this is the common
// case, not a corner one — but not always, since a destination can be an
// external drive, which is why they are not simply assumed to be one volume.
//
// A requirement of zero or fewer bytes is skipped: an unknown size is not a
// reason to refuse a download. Anything we cannot measure is skipped for the
// same reason — this may only turn away a download it is sure will not fit.
func CheckAll(needs ...Need) error {
	type vol struct {
		dir   string
		bytes int64
	}
	byVolume := map[uint64]*vol{}
	var order []uint64

	for _, n := range needs {
		if n.Bytes <= 0 {
			continue
		}
		dir, err := existingAncestor(n.Path)
		if err != nil {
			continue
		}
		id, err := volumeID(dir)
		if err != nil {
			continue
		}
		v, seen := byVolume[id]
		if !seen {
			v = &vol{dir: dir}
			byVolume[id] = v
			order = append(order, id)
		}
		v.bytes += n.Bytes
	}

	for _, id := range order {
		v := byVolume[id]
		avail, total, err := statVolume(v.dir)
		if err != nil {
			continue
		}
		reserve := reserveFor(total)
		if v.bytes+reserve > avail {
			return &Error{Path: v.dir, Need: v.bytes, Avail: avail, Reserve: reserve}
		}
	}
	return nil
}

// volumeID identifies the filesystem holding dir, so that requirements on the
// same disk can be summed instead of each passing on its own.
func volumeID(dir string) (uint64, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot identify the volume holding %s", dir)
	}
	return uint64(st.Dev), nil
}

// existingAncestor walks up from path until it finds something that exists.
func existingAncestor(path string) (string, error) {
	p := path
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	for {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("nothing above %s exists", path)
		}
		p = parent
	}
}

// HumanBytes formats a byte count for a person reading an error message.
func HumanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1024 {
			if u == "B" {
				return fmt.Sprintf("%.0f %s", f, u)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f PB", f)
}
