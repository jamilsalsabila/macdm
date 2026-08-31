package schedule

import (
	"testing"
	"time"
)

// at builds a local time on a known week: 2026-08-31 is a Monday.
func at(t *testing.T, day time.Weekday, hhmm string) time.Time {
	t.Helper()
	mins, err := ParseHM(hhmm)
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 8, 31, 0, 0, 0, 0, time.Local)
	offset := (int(day) - int(time.Monday) + 7) % 7
	return monday.AddDate(0, 0, offset).Add(time.Duration(mins) * time.Minute)
}

// A scheduler nobody configured must never be the reason nothing downloads.
func TestDisabledOrDaylessWindowAlwaysAllows(t *testing.T) {
	var off Window
	if !off.Active(at(t, time.Wednesday, "03:00")) {
		t.Error("the zero window must allow downloads")
	}
	noDays := Window{Enabled: true, Start: 120, Stop: 360}
	if !noDays.Active(at(t, time.Wednesday, "03:00")) {
		t.Error("an enabled window with no weekday selected must allow downloads")
	}
}

func TestOrdinaryWindowWithinOneDay(t *testing.T) {
	w := Everyday(9*60, 17*60) // 09:00–17:00
	cases := []struct {
		hhmm string
		want bool
	}{
		{"08:59", false},
		{"09:00", true}, // inclusive at the start
		{"12:00", true},
		{"16:59", true},
		{"17:00", false}, // exclusive at the end
		{"23:00", false},
		{"00:00", false},
	}
	for _, c := range cases {
		if got := w.Active(at(t, time.Wednesday, c.hhmm)); got != c.want {
			t.Errorf("%s inside 09:00–17:00 = %v, want %v", c.hhmm, got, c.want)
		}
	}
}

// The usual case: 02:00–06:00 is four hours of one night, not twenty of one day.
func TestOvernightWindowCrossesMidnight(t *testing.T) {
	w := Everyday(22*60, 6*60) // 22:00–06:00
	cases := []struct {
		hhmm string
		want bool
	}{
		{"21:59", false},
		{"22:00", true},
		{"23:59", true},
		{"00:00", true}, // still the same night
		{"05:59", true},
		{"06:00", false},
		{"12:00", false},
	}
	for _, c := range cases {
		if got := w.Active(at(t, time.Wednesday, c.hhmm)); got != c.want {
			t.Errorf("%s inside 22:00–06:00 = %v, want %v", c.hhmm, got, c.want)
		}
	}
}

// A window belongs to the day it opened. Selecting only Friday must cover
// Saturday's small hours, and must not open on Saturday evening.
func TestOvernightWindowBelongsToTheDayItOpened(t *testing.T) {
	w := Window{Enabled: true, Start: 22 * 60, Stop: 6 * 60}
	w.Days[int(time.Friday)] = true

	if !w.Active(at(t, time.Friday, "23:00")) {
		t.Error("Friday 23:00 should be inside a Friday-night window")
	}
	if !w.Active(at(t, time.Saturday, "02:00")) {
		t.Error("Saturday 02:00 is still Friday night — the window should be open")
	}
	if w.Active(at(t, time.Saturday, "23:00")) {
		t.Error("Saturday 23:00 must not open a window selected only for Friday")
	}
	if w.Active(at(t, time.Friday, "12:00")) {
		t.Error("Friday noon is outside 22:00–06:00")
	}
	if w.Active(at(t, time.Thursday, "23:00")) {
		t.Error("Thursday night is not selected")
	}
}

func TestWeekdaysOnly(t *testing.T) {
	w := Window{Enabled: true, Start: 9 * 60, Stop: 17 * 60}
	for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
		w.Days[int(d)] = true
	}
	if !w.Active(at(t, time.Wednesday, "10:00")) {
		t.Error("Wednesday 10:00 should be inside a weekday window")
	}
	if w.Active(at(t, time.Saturday, "10:00")) {
		t.Error("Saturday must be outside a weekday-only window")
	}
	if w.Active(at(t, time.Sunday, "10:00")) {
		t.Error("Sunday must be outside a weekday-only window")
	}
}

// Both ends the same reads as "all day", not "never".
func TestEqualEndsCoverTheWholeDay(t *testing.T) {
	w := Everyday(0, 0)
	for _, hhmm := range []string{"00:00", "07:30", "13:00", "23:59"} {
		if !w.Active(at(t, time.Tuesday, hhmm)) {
			t.Errorf("%s should be inside an all-day window", hhmm)
		}
	}
}

func TestParseHM(t *testing.T) {
	good := map[string]int{
		"00:00": 0, "02:00": 120, "09:05": 545, "23:59": 1439, " 7:30 ": 450,
	}
	for in, want := range good {
		got, err := ParseHM(in)
		if err != nil || got != want {
			t.Errorf("ParseHM(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "24:00", "12:60", "-1:00", "abc", "1200", "12:xy"} {
		if _, err := ParseHM(bad); err == nil {
			t.Errorf("ParseHM(%q) should have failed", bad)
		}
	}
}

func TestFormatHM(t *testing.T) {
	cases := map[int]string{0: "00:00", 120: "02:00", 545: "09:05", 1439: "23:59", 1440: "00:00"}
	for in, want := range cases {
		if got := FormatHM(in); got != want {
			t.Errorf("FormatHM(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestString(t *testing.T) {
	var off Window
	if got := off.String(); got != "off" {
		t.Errorf("disabled window described as %q", got)
	}
	if got := Everyday(120, 360).String(); got != "02:00–06:00 every day" {
		t.Errorf("got %q", got)
	}
	w := Window{Enabled: true, Start: 120, Stop: 360}
	w.Days[int(time.Saturday)] = true
	w.Days[int(time.Sunday)] = true
	if got := w.String(); got != "02:00–06:00 Sun Sat" {
		t.Errorf("got %q", got)
	}
}
