package manager

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"macdm/internal/engine"
	"macdm/internal/schedule"
	"macdm/internal/store"
)

// shutWindow is a window that is certainly closed now: it opens and closes in
// the minute that has just passed.
func shutWindow(t *testing.T) schedule.Window {
	t.Helper()
	now := time.Now()
	mins := now.Hour()*60 + now.Minute()
	w := schedule.Everyday((mins+1440-3)%1440, (mins+1440-2)%1440)
	if w.Active(now) {
		t.Fatalf("test window %s should be closed at %s", w, now.Format("15:04"))
	}
	return w
}

// openWindow is a window covering the whole day.
func openWindow(t *testing.T) schedule.Window {
	t.Helper()
	w := schedule.Everyday(0, 0)
	if !w.Active(time.Now()) {
		t.Fatal("an all-day window should be open")
	}
	return w
}

func slowServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	body := strings.Repeat("x", size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		start := 0
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			p := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
			start, _ = strconv.Atoi(p[0])
		}
		if start > len(body) {
			start = len(body)
		}
		seg := body[start:]
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
		}
		// Dribble it out so the job is still running when the test looks.
		for i := 0; i < len(seg); i += 512 {
			end := i + 512
			if end > len(seg) {
				end = len(seg)
			}
			w.Write([]byte(seg[i:end]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newManager(t *testing.T, w schedule.Window) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	m := New(Config{
		DownloadDir: t.TempDir(), MaxActive: 2, PromptTimeoutSec: 300,
		Schedule: w,
		Engine:   engine.Config{MaxConns: 2, MinChunk: 1 << 20, Timeout: 5 * time.Second},
	}, st)
	t.Cleanup(func() { m.Shutdown(2 * time.Second) })
	return m
}

func waitFor(t *testing.T, m *Manager, id string, cond func(*store.Job) bool, what string) *store.Job {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		if j, err := m.st.Get(id); err == nil && cond(j) {
			return j
		}
		select {
		case <-deadline:
			j, _ := m.st.Get(id)
			t.Fatalf("timed out waiting for %s; status=%q hold=%v err=%q", what, j.Status, j.ScheduledHold, j.Error)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Outside the window a new job waits instead of downloading, and says so.
func TestJobAddedOutsideTheWindowIsHeld(t *testing.T) {
	srv := slowServer(t, 4096)
	m := newManager(t, shutWindow(t))

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	held := waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.ScheduledHold }, "the job to be held")
	if held.Status != store.StatusPaused {
		t.Errorf("held job status = %q, want %q", held.Status, store.StatusPaused)
	}
	if !strings.Contains(held.Error, "download window") {
		t.Errorf("held job should explain itself, got %q", held.Error)
	}
	if held.DoneBytes != 0 {
		t.Errorf("a held job downloaded %d bytes; it should not have started", held.DoneBytes)
	}
}

// When the window opens, what the scheduler put down is picked back up.
func TestOpeningTheWindowReleasesHeldJobs(t *testing.T) {
	srv := slowServer(t, 4096)
	m := newManager(t, shutWindow(t))

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.ScheduledHold }, "the job to be held")

	m.SetSchedule(openWindow(t))
	done := waitFor(t, m, j.ID, func(jj *store.Job) bool {
		return jj.Status == store.StatusCompleted
	}, "the released job to finish")
	if done.ScheduledHold {
		t.Error("a finished job should not still be marked as held")
	}
}

// The property that makes the scheduler safe to leave on: a download the user
// paused on purpose must stay paused when the window opens. Only the
// scheduler's own holds are picked up.
func TestOpeningTheWindowLeavesUserPausedJobsAlone(t *testing.T) {
	srv := slowServer(t, 1<<16) // big enough to still be running when we pause
	m := newManager(t, openWindow(t))

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.Status == store.StatusDownloading }, "the job to start")
	if err := m.Pause(j.ID); err != nil {
		t.Fatal(err)
	}
	paused := waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.Status == store.StatusPaused }, "the user pause")
	if paused.ScheduledHold {
		t.Fatal("a user pause must not be recorded as a scheduler hold")
	}

	// Close the window, then open it again — the full cycle the scheduler runs.
	m.SetSchedule(shutWindow(t))
	m.SetSchedule(openWindow(t))

	time.Sleep(300 * time.Millisecond)
	after, err := m.st.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == store.StatusDownloading || after.Status == store.StatusCompleted {
		t.Errorf("the scheduler restarted a job the user paused (status %q)", after.Status)
	}
}

// Closing the window stops what is running and marks it for later.
func TestClosingTheWindowPausesRunningJobs(t *testing.T) {
	srv := slowServer(t, 1<<16)
	m := newManager(t, openWindow(t))

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.Status == store.StatusDownloading }, "the job to start")

	m.SetSchedule(shutWindow(t))
	held := waitFor(t, m, j.ID, func(jj *store.Job) bool {
		return jj.ScheduledHold && jj.Status == store.StatusPaused
	}, "the running job to be held")
	if !strings.Contains(held.Error, "download window") {
		t.Errorf("held job should explain itself, got %q", held.Error)
	}
}

// Pausing a held job opts it out: the next open window must not pick it up.
func TestPausingAHeldJobClearsTheHold(t *testing.T) {
	srv := slowServer(t, 4096)
	m := newManager(t, shutWindow(t))

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.ScheduledHold }, "the job to be held")

	if err := m.Pause(j.ID); err != nil {
		t.Fatalf("pausing a held job should be allowed: %v", err)
	}
	after, _ := m.st.Get(j.ID)
	if after.ScheduledHold {
		t.Error("pausing a held job must clear the hold")
	}

	m.SetSchedule(openWindow(t))
	time.Sleep(300 * time.Millisecond)
	final, _ := m.st.Get(j.ID)
	if final.Status == store.StatusDownloading || final.Status == store.StatusCompleted {
		t.Errorf("an opted-out job was started anyway (status %q)", final.Status)
	}
}

// With no schedule configured nothing changes: downloads run immediately.
func TestNoScheduleMeansNoInterference(t *testing.T) {
	srv := slowServer(t, 4096)
	m := newManager(t, schedule.Window{})

	j, err := m.Add(srv.URL+"/file.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := waitFor(t, m, j.ID, func(jj *store.Job) bool {
		return jj.Status == store.StatusCompleted
	}, "the job to finish with no schedule")
	if done.ScheduledHold {
		t.Error("no schedule should never mark a hold")
	}
}

// A job can finish in the moment between the scheduler listing it and pausing
// it. Dragging a completed download back out of "completed" would be a lie
// about work that really did finish.
func TestCompletedJobSurvivesAStrayHold(t *testing.T) {
	srv := slowServer(t, 2048)
	m := newManager(t, openWindow(t))
	j, err := m.Add(srv.URL+"/f.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.Status == store.StatusCompleted }, "completion")

	// Exactly what the close path does to a job it believes is still running.
	_, _ = m.st.Update(j.ID, func(jj *store.Job) {
		jj.ScheduledHold = true
		jj.Error = "waiting for the download window (test)"
	})
	_ = m.Pause(j.ID)

	after, _ := m.st.Get(j.ID)
	if after.Status != store.StatusCompleted {
		t.Errorf("a completed job became %q after a stray scheduler hold", after.Status)
	}
}

// Releasing holds must unmark a finished job, not download it a second time.
func TestOpeningTheWindowDoesNotRestartCompletedJobs(t *testing.T) {
	srv := slowServer(t, 2048)
	m := newManager(t, openWindow(t))
	j, err := m.Add(srv.URL+"/f.bin", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, func(jj *store.Job) bool { return jj.Status == store.StatusCompleted }, "completion")

	_, _ = m.st.Update(j.ID, func(jj *store.Job) { jj.ScheduledHold = true })
	m.applySchedule()
	time.Sleep(250 * time.Millisecond)

	after, _ := m.st.Get(j.ID)
	if after.Status != store.StatusCompleted {
		t.Errorf("a completed job was restarted by the scheduler: status %q", after.Status)
	}
	if after.ScheduledHold {
		t.Error("a completed job should not stay marked as held")
	}
}
