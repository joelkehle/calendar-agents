package scheduler

import (
	"testing"
	"time"
)

func TestCandidateAllowedWithTravel(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := lat(t, "2026-06-11T16:00:00")
	busyAt := func(start, end string) []BusyInterval {
		return []BusyInterval{{Start: lat(t, start), End: lat(t, end)}}
	}

	cases := []struct {
		name         string
		start, end   string
		blockMinutes int
		busy         []BusyInterval
		now          time.Time
		want         bool
	}{
		{
			name:  "clear slot with both blocks free",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			now: now, want: true,
		},
		{
			name:  "travel-before conflicts",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			busy: busyAt("2026-06-12T09:15:00", "2026-06-12T09:45:00"),
			now:  now, want: false,
		},
		{
			name:  "travel-after conflicts",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			busy: busyAt("2026-06-12T11:15:00", "2026-06-12T11:45:00"),
			now:  now, want: false,
		},
		{
			name:  "travel over lunch allowed",
			start: "2026-06-12T13:00:00", end: "2026-06-12T14:00:00", blockMinutes: 30,
			now: now, want: true, // before block 12:30-13:00 covers lunch: fine
		},
		{
			name:  "hold over lunch still rejected",
			start: "2026-06-12T12:30:00", end: "2026-06-12T13:30:00", blockMinutes: 30,
			now: now, want: false,
		},
		{
			name:  "before block under the 06:00 floor",
			start: "2026-06-12T06:15:00", end: "2026-06-12T07:15:00", blockMinutes: 30,
			now: now, want: false, // before block would start 05:45
		},
		{
			name:  "before block exactly at the 06:00 floor",
			start: "2026-06-12T06:30:00", end: "2026-06-12T07:30:00", blockMinutes: 30,
			now: now, want: true,
		},
		{
			name:  "after block past the 22:00 ceiling",
			start: "2026-06-12T20:45:00", end: "2026-06-12T21:45:00", blockMinutes: 30,
			now: now, want: false, // after block would end 22:15
		},
		{
			name:  "after block exactly at the 22:00 ceiling",
			start: "2026-06-12T20:30:00", end: "2026-06-12T21:30:00", blockMinutes: 30,
			now: now, want: true,
		},
		{
			name:  "post-busy buffer applies to the before-block start",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			busy: busyAt("2026-06-12T09:00:00", "2026-06-12T09:25:00"),
			now:  now, want: false, // before block starts 09:30, only 5 min after busy end
		},
		{
			name:  "post-busy buffer satisfied",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			busy: busyAt("2026-06-12T09:00:00", "2026-06-12T09:20:00"),
			now:  now, want: true, // before block starts 09:30, 10 min after busy end
		},
		{
			name:  "now plus five minute lead on the before block",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			now: lat(t, "2026-06-12T09:27:00"), want: false, // before block 09:30 < now+5m
		},
		{
			name:  "now plus five minute lead satisfied",
			start: "2026-06-12T10:00:00", end: "2026-06-12T11:00:00", blockMinutes: 30,
			now: lat(t, "2026-06-12T09:25:00"), want: true,
		},
	}
	for _, tc := range cases {
		got := candidateAllowedWithTravel(lat(t, tc.start), lat(t, tc.end), tc.blockMinutes, tc.busy, loc, tc.now)
		if got != tc.want {
			t.Errorf("%s: candidateAllowedWithTravel = %t, want %t", tc.name, got, tc.want)
		}
	}
}
