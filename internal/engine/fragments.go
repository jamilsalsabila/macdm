package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"macdm/internal/diskspace"
)

// Fragment is one byte-range slice of a media file, addressed by its own URL
// (Instagram / Facebook bake `bytestart`/`byteend` into the query string rather
// than honouring the Range header). Concatenating every fragment in order
// reproduces the whole file.
type Fragment struct {
	URL   string `json:"url"`
	Start int64  `json:"start"`
	End   int64  `json:"end"` // inclusive
}

type fragSidecar struct {
	Dest  string `json:"dest"`
	Total int64  `json:"total"`
	Done  []bool `json:"done"` // parallel to the sorted fragment list
}

func fragSidecarPath(dest string) string { return dest + ".macdmf" }

// RunFragments downloads every fragment concurrently (up to conns at once),
// writing each at its byte offset, and renames the result to dest on success.
// It resumes: fragments already fetched in a previous run are skipped.
func (e *Engine) RunFragments(ctx context.Context, dest string, headers map[string]string,
	frags []Fragment, conns int, onProgress func(Progress)) error {

	if len(frags) == 0 {
		return errors.New("no fragments")
	}
	sort.Slice(frags, func(i, j int) bool { return frags[i].Start < frags[j].Start })

	var total int64
	for _, f := range frags {
		if f.End+1 > total {
			total = f.End + 1
		}
	}

	// The slices must tile [0,total) with no hole — otherwise the assembled file
	// has uninitialised gaps and is unplayable. (Overlap is fine; WriteAt just
	// rewrites those bytes.) A hole means the browser never fetched some range,
	// e.g. the user seeked past it; the page URL should be extracted instead.
	var cursor int64
	for _, f := range frags {
		if f.Start > cursor {
			return fmt.Errorf("fragments are not contiguous (gap at byte %d) — use \"Extract video from this page\" instead", cursor)
		}
		if f.End+1 > cursor {
			cursor = f.End + 1
		}
	}
	if conns <= 0 {
		conns = e.cfg.MaxConns
	}
	if conns > len(frags) {
		conns = len(frags)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	doneMask := make([]bool, len(frags))
	if sc := loadFragSidecar(dest); sc != nil && sc.Total == total && len(sc.Done) == len(frags) {
		copy(doneMask, sc.Done)
	}

	var doneBytes atomic.Int64
	for i, d := range doneMask {
		if d {
			doneBytes.Add(frags[i].End - frags[i].Start + 1)
		}
	}

	// Same guard as Run, and for the same reason: the Truncate below looks like
	// a reservation but APFS makes the file sparse and holds back nothing. Only
	// the fragments still missing are counted, so a resume is not refused over
	// space its own .part file already occupies.
	if err := diskspace.Check(dest, total-doneBytes.Load()); err != nil {
		return err
	}

	f, err := os.OpenFile(dest+".part", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(total); err != nil {
		return err
	}

	var mu sync.Mutex
	states := make([]ConnProgress, conns)
	for i := range states {
		states[i] = ConnProgress{Index: i, Status: "idle"}
	}
	var emitMu sync.Mutex
	emit := func() {
		if onProgress == nil {
			return
		}
		mu.Lock()
		snap := append([]ConnProgress(nil), states...)
		mu.Unlock()
		emitMu.Lock()
		onProgress(Progress{
			DoneBytes: doneBytes.Load(), TotalBytes: total, NumConns: conns,
			Resumable: true, Conns: snap,
		})
		emitMu.Unlock()
	}

	saveSidecar := func() {
		mu.Lock()
		sc := fragSidecar{Dest: dest, Total: total, Done: append([]bool(nil), doneMask...)}
		mu.Unlock()
		if b, e := json.Marshal(sc); e == nil {
			_ = os.WriteFile(fragSidecarPath(dest), b, 0o644)
		}
	}

	// A worker error cancels this derived context so the remaining workers and
	// the feed loop unwind instead of deadlocking on the unbuffered channel.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	for w := 0; w < conns; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if wctx.Err() != nil {
					return
				}
				fr := frags[idx]
				mu.Lock()
				states[w] = ConnProgress{Index: w, Start: fr.Start,
					Total: fr.End - fr.Start + 1, Status: "receiving"}
				mu.Unlock()
				emit()

				var n int64
				var err error
				for attempt := 0; attempt < 3; attempt++ {
					if wctx.Err() != nil {
						return
					}
					n, err = e.fetchFragment(wctx, f, fr, headers)
					if err == nil {
						break
					}
				}
				if err != nil {
					setErr(fmt.Errorf("fragment %d/%d: %w", idx+1, len(frags), err))
					return
				}
				doneBytes.Add(n)
				mu.Lock()
				doneMask[idx] = true
				states[w] = ConnProgress{Index: w, Status: "idle"}
				mu.Unlock()
				saveSidecar()
				emit()
			}
		}()
	}
	for i := range frags {
		if doneMask[i] {
			continue
		}
		select {
		case jobs <- i:
		case <-wctx.Done():
			close(jobs)
			wg.Wait()
			if firstErr != nil {
				return firstErr
			}
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(dest+".part", dest); err != nil {
		return err
	}
	_ = os.Remove(fragSidecarPath(dest))
	if onProgress != nil {
		onProgress(Progress{DoneBytes: total, TotalBytes: total, NumConns: conns, Resumable: true})
	}
	return nil
}

func (e *Engine) fetchFragment(ctx context.Context, f *os.File, fr Fragment, headers map[string]string) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stall := e.cfg.FragmentStall
	if stall <= 0 {
		stall = defaultFragmentStall
	}

	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := stall / 4
		if tick > 5*time.Second {
			tick = 5 * time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRead.Load())) > stall {
					cancel()
					return
				}
			}
		}
	}()
	r, err := e.req(ctx, http.MethodGet, fr.URL, headers)
	if err != nil {
		return 0, err
	}
	resp, err := e.client.Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("status %s", resp.Status)
	}

	buf := make([]byte, 128*1024)
	off := fr.Start
	maxN := fr.End - fr.Start + 1
	var n int64
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			// Never write past this fragment's slot — an over-long response
			// would otherwise clobber the next fragment's bytes.
			if n+int64(nr) > maxN {
				nr = int(maxN - n)
			}
			if nr <= 0 {
				return n, nil
			}
			lastRead.Store(time.Now().UnixNano())
			if _, ew := f.WriteAt(buf[:nr], off); ew != nil {
				return n, ew
			}
			off += int64(nr)
			n += int64(nr)
			// The same shared ceiling the chunked path obeys. Fragments are a
			// separate download loop, so a limiter wired only into Run would
			// have quietly exempted every Instagram and Facebook video.
			if err := e.cfg.Limiter.Wait(ctx, nr); err != nil {
				return n, err
			}
		}
		if er == io.EOF {
			return n, nil
		}
		if er != nil {
			return n, er
		}
	}
}

func loadFragSidecar(dest string) *fragSidecar {
	b, err := os.ReadFile(fragSidecarPath(dest))
	if err != nil {
		return nil
	}
	var sc fragSidecar
	if json.Unmarshal(b, &sc) != nil {
		return nil
	}
	return &sc
}
