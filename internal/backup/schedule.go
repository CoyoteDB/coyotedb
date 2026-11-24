package backup

import (
	"fmt"
	"time"
)

// FirstDelay: with no full backup yet, the first one starts this long
// after backups are on (lets a new cluster settle; D63).
const FirstDelay = 10 * time.Minute

// Schedule is when full backups run: at At (HH:MM, local time) and every
// Every after it.
type Schedule struct {
	Every time.Duration
	At    string
}

func (s Schedule) clock() (h, m int, err error) {
	t, err := time.Parse("15:04", s.At)
	if err != nil {
		return 0, 0, fmt.Errorf("backup time %q: %w", s.At, err)
	}
	return t.Hour(), t.Minute(), nil
}

// LastSlot is the most recent scheduled moment at or before now (in now's
// location). Whole-day intervals follow the calendar (03:00 stays 03:00
// across daylight saving changes); shorter ones count from At each day.
func (s Schedule) LastSlot(now time.Time) time.Time {
	h, m, err := s.clock()
	if err != nil || s.Every <= 0 {
		return time.Time{}
	}
	loc := now.Location()
	day := func(t time.Time, d int) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day()+d, h, m, 0, 0, loc)
	}
	today := day(now, 0)
	if s.Every%(24*time.Hour) == 0 {
		n := int(s.Every / (24 * time.Hour))
		slot := today
		if slot.After(now) {
			slot = day(now, -1)
		}
		// Align multi-day intervals to a fixed calendar anchor.
		anchor := time.Date(2000, 1, 3, h, m, 0, 0, loc)
		days := int(slot.Sub(anchor).Round(24*time.Hour) / (24 * time.Hour))
		return day(slot, -(((days % n) + n) % n))
	}
	start := today
	if start.After(now) {
		start = day(now, -1)
	}
	k := now.Sub(start) / s.Every
	return start.Add(k * s.Every)
}

// NextSlot is the first scheduled moment after now.
func (s Schedule) NextSlot(now time.Time) time.Time {
	last := s.LastSlot(now)
	if last.IsZero() {
		return last
	}
	if s.Every%(24*time.Hour) == 0 {
		n := int(s.Every / (24 * time.Hour))
		return time.Date(last.Year(), last.Month(), last.Day()+n, last.Hour(), last.Minute(), 0, 0, last.Location())
	}
	next := last.Add(s.Every)
	// A sub-day schedule restarts at At each day.
	if h, m, err := s.clock(); err == nil {
		tomorrow := time.Date(last.Year(), last.Month(), last.Day()+1, h, m, 0, 0, last.Location())
		if next.After(tomorrow) {
			next = tomorrow
		}
	}
	return next
}

// DueSince says whether a full backup is due at now and since when.
// lastStart is when the newest finished full backup started (zero: none);
// firstAllowed is when a first one may start.
func (s Schedule) DueSince(now, lastStart, firstAllowed time.Time) (time.Time, bool) {
	if lastStart.IsZero() {
		return firstAllowed, !now.Before(firstAllowed)
	}
	slot := s.LastSlot(now)
	if slot.IsZero() || !lastStart.Before(slot) {
		return time.Time{}, false
	}
	return slot, true
}
