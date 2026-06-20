package scheduler

import (
	"testing"
	"time"

	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

func TestSelectSlotHonorsBoundaryAndFutureLead(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 7, 2, 0, 0, loc)
	windows := []Interval{{Start: time.Date(2026, 6, 11, 7, 7, 0, 0, loc), End: time.Date(2026, 6, 11, 10, 0, 0, 0, loc)}}
	slot, ok := SelectSlot(windows, nil, now, 30*time.Minute)
	if !ok {
		t.Fatal("SelectSlot() ok = false")
	}
	if slot.Start != "2026-06-11T07:45:00-07:00" {
		t.Fatalf("slot.Start = %s, want 07:45", slot.Start)
	}
}

func TestSelectSlotAvoidsLunch(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 7, 0, 0, 0, loc)
	windows := []Interval{{Start: time.Date(2026, 6, 11, 11, 30, 0, 0, loc), End: time.Date(2026, 6, 11, 14, 0, 0, 0, loc)}}
	slot, ok := SelectSlot(windows, nil, now, time.Hour)
	if !ok {
		t.Fatal("SelectSlot() ok = false")
	}
	if slot.Start != "2026-06-11T13:00:00-07:00" {
		t.Fatalf("slot.Start = %s, want 13:00", slot.Start)
	}
}

func TestSelectSlotAppliesBufferAfterTimedBusyOnly(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 6, 0, 0, 0, loc)
	windows := []Interval{{Start: time.Date(2026, 6, 11, 8, 0, 0, 0, loc), End: time.Date(2026, 6, 11, 10, 30, 0, 0, loc)}}
	events := []calendarread.Event{timedEvent("meeting", "2026-06-11T08:00:00-07:00", "2026-06-11T09:00:00-07:00")}
	slot, ok := SelectSlot(windows, events, now, 30*time.Minute)
	if !ok {
		t.Fatal("SelectSlot() ok = false")
	}
	if slot.Start != "2026-06-11T09:15:00-07:00" {
		t.Fatalf("slot.Start = %s, want 09:15", slot.Start)
	}
}

func TestSelectSlotBusySetExemptions(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 6, 0, 0, 0, loc)
	windows := []Interval{{Start: time.Date(2026, 6, 11, 7, 0, 0, 0, loc), End: time.Date(2026, 6, 11, 9, 0, 0, 0, loc)}}
	events := []calendarread.Event{
		allDayEvent("Meeting Quota Reached", "2026-06-11", "2026-06-12"),
		timedTransparentEvent("free-looking", "2026-06-11T07:00:00-07:00", "2026-06-11T08:00:00-07:00"),
	}
	slot, ok := SelectSlot(windows, events, now, 30*time.Minute)
	if !ok {
		t.Fatal("SelectSlot() ok = false")
	}
	if slot.Start != "2026-06-11T07:00:00-07:00" {
		t.Fatalf("slot.Start = %s, want 07:00", slot.Start)
	}

	events = append(events, allDayEvent("OOO", "2026-06-11", "2026-06-12"))
	if _, ok := SelectSlot(windows, events, now, 30*time.Minute); ok {
		t.Fatal("SelectSlot() ok = true with all-day busy event")
	}
}

func TestNearestAlternativeAfterInfeasibleWindow(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 6, 0, 0, 0, loc)
	windows := []Interval{{Start: time.Date(2026, 6, 11, 8, 0, 0, 0, loc), End: time.Date(2026, 6, 11, 9, 0, 0, 0, loc)}}
	events := []calendarread.Event{timedEvent("meeting", "2026-06-11T08:00:00-07:00", "2026-06-11T09:00:00-07:00")}
	if _, ok := SelectSlot(windows, events, now, time.Hour); ok {
		t.Fatal("SelectSlot() ok = true, want infeasible")
	}
	alt, ok := NearestAlternative(windows, events, nil, now, 30*time.Minute)
	if !ok {
		t.Fatal("NearestAlternative() ok = false")
	}
	if alt.Start != "2026-06-11T09:15:00-07:00" {
		t.Fatalf("alt.Start = %s, want 09:15", alt.Start)
	}
}

func timedEvent(summary, start, end string) calendarread.Event {
	return calendarread.Event{
		Summary: summary,
		Start:   calendarread.EventDateTime{DateTime: start, TimeZone: DefaultTimeZone},
		End:     calendarread.EventDateTime{DateTime: end, TimeZone: DefaultTimeZone},
	}
}

func timedTransparentEvent(summary, start, end string) calendarread.Event {
	event := timedEvent(summary, start, end)
	event.Transparency = "transparent"
	return event
}

func allDayEvent(summary, start, end string) calendarread.Event {
	return calendarread.Event{
		Summary: summary,
		Start:   calendarread.EventDateTime{Date: start, TimeZone: DefaultTimeZone},
		End:     calendarread.EventDateTime{Date: end, TimeZone: DefaultTimeZone},
	}
}
