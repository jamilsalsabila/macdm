// Package schedule decides whether downloads are allowed to run right now.
//
// The point is to move traffic to hours when nobody minds it: "download between
// 2am and 6am on weeknights". The awkward part is that such a window normally
// crosses midnight, so "is now inside it" cannot be a simple comparison, and
// the day a window belongs to is the day it *started*, not the day it ends.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a recurring time-of-day window on selected weekdays. The zero value
// is disabled, which means downloads run whenever they like.
type Window struct {
	Enabled bool
	// Start and Stop are minutes since midnight, local time. Stop <= Start
	// describes a window that crosses midnight; Stop == Start covers the whole
	// day, which is what someone setting both ends the same is asking for.
	Start int
	Stop  int
	// Days selects the weekdays the window may *begin* on, indexed by
	// time.Weekday (Sunday = 0). An overnight window that opens on Friday runs
	// into Saturday morning whether or not Saturday is selected.
	Days [7]bool
}

// Everyday returns a window covering the given hours on all seven days.
func Everyday(start, stop int) Window {
	w := Window{Enabled: true, Start: start, Stop: stop}
	for i := range w.Days {
		w.Days[i] = true
	}
	return w
}

// AnyDay reports whether at least one weekday is selected. A window with none
// could never open, so callers treat that as "not really configured".
func (w Window) AnyDay() bool {
	for _, d := range w.Days {
		if d {
			return true
		}
	}
	return false
}

// Active reports whether downloads may run at the given moment. A disabled
// window — or one with no day selected — always says yes: a scheduler that has
// not been set up must never be the reason nothing downloads.
func (w Window) Active(now time.Time) bool {
	if !w.Enabled || !w.AnyDay() {
		return true
	}
	mins := now.Hour()*60 + now.Minute()
	today := w.Days[int(now.Weekday())]

	if w.Start < w.Stop {
		// An ordinary window, opening and closing on the same day.
		return today && mins >= w.Start && mins < w.Stop
	}
	// Crosses midnight (or covers the whole day when Start == Stop). Either it
	// opened today and has not reached midnight, or it opened yesterday and has
	// not yet reached its closing time this morning.
	yesterday := w.Days[int(now.AddDate(0, 0, -1).Weekday())]
	return (today && mins >= w.Start) || (yesterday && mins < w.Stop)
}

// ParseHM reads "HH:MM" into minutes since midnight.
func ParseHM(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	hh, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("hour out of range in %q", s)
	}
	mm, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("minute out of range in %q", s)
	}
	return hh*60 + mm, nil
}

// FormatHM renders minutes since midnight as "HH:MM".
func FormatHM(mins int) string {
	mins = ((mins % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", mins/60, mins%60)
}

// String describes the window the way it is shown to a person.
func (w Window) String() string {
	if !w.Enabled {
		return "off"
	}
	return fmt.Sprintf("%s–%s %s", FormatHM(w.Start), FormatHM(w.Stop), w.dayNames())
}

func (w Window) dayNames() string {
	names := [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var on []string
	for i, d := range w.Days {
		if d {
			on = append(on, names[i])
		}
	}
	switch len(on) {
	case 0:
		return "(no days)"
	case 7:
		return "every day"
	default:
		return strings.Join(on, " ")
	}
}
