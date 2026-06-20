package scheduler

import (
	"errors"
	"sort"
	"strings"
	"time"

	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

const (
	slotBoundary       = 15 * time.Minute
	minStartLead       = 30 * time.Minute
	postBusyBuffer     = 10 * time.Minute
	lunchStartHour     = 12
	lunchEndHour       = 13
	alternativeHorizon = 7
)

type BusyInterval struct {
	Start  time.Time
	End    time.Time
	AllDay bool
}

func SelectSlot(windows []Interval, events []calendarread.Event, now time.Time, duration time.Duration) (Slot, bool) {
	return SelectSlotWithBusy(windows, events, nil, now, duration)
}

func SelectSlotWithBusy(windows []Interval, events []calendarread.Event, extraBusy []BusyInterval, now time.Time, duration time.Duration) (Slot, bool) {
	loc := loadLocation()
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(loc)
	busy := busyIntervals(events, loc)
	busy = append(busy, extraBusy...)
	sortBusy(busy)

	windows = append([]Interval{}, windows...)
	sortIntervals(windows)
	minStart := now.Add(minStartLead)
	for _, w := range windows {
		start := roundUpToBoundary(maxTime(w.Start.In(loc), minStart.In(loc)), slotBoundary)
		end := w.End.In(loc)
		for !start.Add(duration).After(end) {
			candidateEnd := start.Add(duration)
			if candidateAllowed(start, candidateEnd, busy, loc) {
				return Slot{Start: formatLA(start), End: formatLA(candidateEnd)}, true
			}
			start = start.Add(slotBoundary)
		}
	}
	return Slot{}, false
}

// SelectSlotWithTravel is the travel-aware variant of SelectSlotWithBusy for
// offsite schedule-requests (SCHEDULER_TRAVEL_SPEC §5). blockFor computes the
// travel-block duration for a CANDIDATE slot start (origin can differ across
// candidates spanning the §1.5 boundaries, so it is recomputed per
// candidate). A blockFor error (no_origin) aborts selection: the booking
// path must reply estimate_unavailable with zero writes, not silently book
// without travel protection.
func SelectSlotWithTravel(windows []Interval, events []calendarread.Event, extraBusy []BusyInterval, now time.Time, duration time.Duration, blockFor func(start time.Time) (int, error)) (Slot, int, bool, error) {
	loc := loadLocation()
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(loc)
	busy := busyIntervals(events, loc)
	busy = append(busy, extraBusy...)
	sortBusy(busy)

	windows = append([]Interval{}, windows...)
	sortIntervals(windows)
	minStart := now.Add(minStartLead)
	for _, w := range windows {
		start := roundUpToBoundary(maxTime(w.Start.In(loc), minStart.In(loc)), slotBoundary)
		end := w.End.In(loc)
		for !start.Add(duration).After(end) {
			candidateEnd := start.Add(duration)
			blockMinutes, err := blockFor(start)
			if err != nil {
				return Slot{}, 0, false, err
			}
			if candidateAllowedWithTravel(start, candidateEnd, blockMinutes, busy, loc, now) {
				return Slot{Start: formatLA(start), End: formatLA(candidateEnd)}, blockMinutes, true, nil
			}
			start = start.Add(slotBoundary)
		}
	}
	return Slot{}, 0, false, nil
}

// candidateAllowedWithTravel extends candidateAllowed (which is NOT modified)
// for offsite bookings: the hold's flanking travel blocks must also fit.
//   - [start-block, start) and [end, end+block) must not overlap any busy
//     interval (checked as one contiguous interval — hold + blocks abut).
//   - the before block must start >= now + 5 min and >= 06:00 local; the after
//     block must end <= 22:00 local; both on the same local date as the hold.
//   - the 10-minute post-busy buffer applies to the BEFORE BLOCK's start (the
//     travel block is now the thing that follows a meeting), not the hold's.
//   - the lunch rule applies to the HOLD only: travel blocks are exempt
//     (driving through lunch is allowed).
func candidateAllowedWithTravel(start, end time.Time, blockMinutes int, busy []BusyInterval, loc *time.Location, now time.Time) bool {
	if overlapsLunch(start, end, loc) {
		return false
	}
	block := time.Duration(blockMinutes) * time.Minute
	beforeStart := start.Add(-block).In(loc)
	afterEnd := end.Add(block).In(loc)
	if beforeStart.Before(now.Add(5 * time.Minute)) {
		return false
	}
	holdStartLocal := start.In(loc)
	holdEndLocal := end.In(loc)
	dayFloor := time.Date(holdStartLocal.Year(), holdStartLocal.Month(), holdStartLocal.Day(), 6, 0, 0, 0, loc)
	dayCeiling := time.Date(holdEndLocal.Year(), holdEndLocal.Month(), holdEndLocal.Day(), 22, 0, 0, 0, loc)
	if beforeStart.Before(dayFloor) || afterEnd.After(dayCeiling) {
		return false
	}
	for _, b := range busy {
		if intervalsOverlap(beforeStart, afterEnd, b.Start, b.End) {
			return false
		}
		if !b.AllDay && beforeStart.Before(b.End.Add(postBusyBuffer)) && !beforeStart.Before(b.End) {
			return false
		}
	}
	return true
}

func NearestAlternative(windows []Interval, events []calendarread.Event, extraBusy []BusyInterval, now time.Time, duration time.Duration) (Slot, bool) {
	alt := alternativeWindows(windows)
	if len(alt) == 0 {
		return Slot{}, false
	}
	return SelectSlotWithBusy(alt, events, extraBusy, now, duration)
}

func alternativeWindows(windows []Interval) []Interval {
	if len(windows) == 0 {
		return nil
	}
	loc := loadLocation()
	latestEnd := windows[0].End.In(loc)
	for _, w := range windows[1:] {
		if w.End.After(latestEnd) {
			latestEnd = w.End.In(loc)
		}
	}
	horizon := latestEnd.AddDate(0, 0, alternativeHorizon)
	date := localDateStart(latestEnd, loc)
	out := make([]Interval, 0, alternativeHorizon+1)
	for !date.After(localDateStart(horizon, loc)) {
		w := dayWindow(date, loc)
		if latestEnd.After(w.Start) {
			w.Start = latestEnd
		}
		if w.Start.Before(w.End) {
			out = append(out, w)
		}
		date = date.AddDate(0, 0, 1)
	}
	return out
}

func candidateAllowed(start, end time.Time, busy []BusyInterval, loc *time.Location) bool {
	if overlapsLunch(start, end, loc) {
		return false
	}
	for _, b := range busy {
		if intervalsOverlap(start, end, b.Start, b.End) {
			return false
		}
		if !b.AllDay && start.Before(b.End.Add(postBusyBuffer)) && !start.Before(b.End) {
			return false
		}
	}
	return true
}

func overlapsLunch(start, end time.Time, loc *time.Location) bool {
	start = start.In(loc)
	end = end.In(loc)
	date := localDateStart(start, loc)
	for !date.After(localDateStart(end, loc)) {
		lunchStart := time.Date(date.Year(), date.Month(), date.Day(), lunchStartHour, 0, 0, 0, loc)
		lunchEnd := time.Date(date.Year(), date.Month(), date.Day(), lunchEndHour, 0, 0, 0, loc)
		if intervalsOverlap(start, end, lunchStart, lunchEnd) {
			return true
		}
		date = date.AddDate(0, 0, 1)
	}
	return false
}

func busyIntervals(events []calendarread.Event, loc *time.Location) []BusyInterval {
	out := make([]BusyInterval, 0, len(events))
	for _, event := range events {
		if strings.EqualFold(strings.TrimSpace(event.Transparency), "transparent") {
			continue
		}
		start, end, allDay, ok := eventBounds(event, loc)
		if !ok || !start.Before(end) {
			continue
		}
		if allDay && isGuardSummary(event.Summary) {
			continue
		}
		out = append(out, BusyInterval{Start: start, End: end, AllDay: allDay})
	}
	return out
}

func eventBounds(event calendarread.Event, loc *time.Location) (time.Time, time.Time, bool, bool) {
	if strings.TrimSpace(event.Start.Date) != "" || strings.TrimSpace(event.End.Date) != "" {
		start, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(event.Start.Date), loc)
		if err != nil {
			return time.Time{}, time.Time{}, false, false
		}
		end, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(event.End.Date), loc)
		if err != nil {
			return time.Time{}, time.Time{}, false, false
		}
		return start, end, true, true
	}
	start, err := parseEventTime(event.Start.DateTime)
	if err != nil {
		return time.Time{}, time.Time{}, false, false
	}
	end, err := parseEventTime(event.End.DateTime)
	if err != nil {
		return time.Time{}, time.Time{}, false, false
	}
	return start.In(loc), end.In(loc), false, true
}

func parseEventTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("expected RFC3339 timestamp")
}

func isGuardSummary(summary string) bool {
	summary = strings.TrimSpace(summary)
	return summary == "Meeting Quota Reached" || strings.HasPrefix(summary, "No more meetings")
}

func intervalsOverlap(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}

func roundUpToBoundary(t time.Time, boundary time.Duration) time.Time {
	if boundary <= 0 {
		return t
	}
	truncated := t.Truncate(boundary)
	if truncated.Equal(t) {
		return t
	}
	return truncated.Add(boundary)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func sortBusy(busy []BusyInterval) {
	sort.Slice(busy, func(i, j int) bool {
		if busy[i].Start.Equal(busy[j].Start) {
			return busy[i].End.Before(busy[j].End)
		}
		return busy[i].Start.Before(busy[j].Start)
	})
}
