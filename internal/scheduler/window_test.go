package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestResolveWindowGrammarProductions(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 10, 30, 0, 0, loc)
	tests := []struct {
		name      string
		window    string
		wantStart string
		wantEnd   string
		wantCount int
	}{
		{name: "today", window: "today", wantStart: "2026-06-11T10:30:00-07:00", wantEnd: "2026-06-11T21:00:00-07:00", wantCount: 1},
		{name: "tomorrow", window: "ToMoRrOw", wantStart: "2026-06-12T07:00:00-07:00", wantEnd: "2026-06-12T21:00:00-07:00", wantCount: 1},
		{name: "this morning", window: "this morning", wantStart: "2026-06-11T10:30:00-07:00", wantEnd: "2026-06-11T12:00:00-07:00", wantCount: 1},
		{name: "this afternoon", window: "this afternoon", wantStart: "2026-06-11T12:00:00-07:00", wantEnd: "2026-06-11T17:00:00-07:00", wantCount: 1},
		{name: "this evening", window: "this evening", wantStart: "2026-06-11T17:00:00-07:00", wantEnd: "2026-06-11T21:00:00-07:00", wantCount: 1},
		{name: "tomorrow morning", window: "tomorrow morning", wantStart: "2026-06-12T07:00:00-07:00", wantEnd: "2026-06-12T12:00:00-07:00", wantCount: 1},
		{name: "tomorrow afternoon", window: "tomorrow afternoon", wantStart: "2026-06-12T12:00:00-07:00", wantEnd: "2026-06-12T17:00:00-07:00", wantCount: 1},
		{name: "tomorrow evening", window: "tomorrow evening", wantStart: "2026-06-12T17:00:00-07:00", wantEnd: "2026-06-12T21:00:00-07:00", wantCount: 1},
		{name: "next days", window: "next 3 days", wantStart: "2026-06-11T10:30:00-07:00", wantEnd: "2026-06-11T21:00:00-07:00", wantCount: 3},
		{name: "date", window: "2026-06-15", wantStart: "2026-06-15T07:00:00-07:00", wantEnd: "2026-06-15T21:00:00-07:00", wantCount: 1},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveWindow(tc.window, "", "", now)
			if err != nil {
				t.Fatalf("ResolveWindow() error = %v", err)
			}
			if len(got) != tc.wantCount {
				t.Fatalf("len = %d, want %d (%#v)", len(got), tc.wantCount, got)
			}
			if formatLA(got[0].Start) != tc.wantStart || formatLA(got[0].End) != tc.wantEnd {
				t.Fatalf("window = %s/%s, want %s/%s", formatLA(got[0].Start), formatLA(got[0].End), tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestResolveWindowPastSegmentAndInvalidGrammar(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	_, err := ResolveWindow("this morning", "", "", time.Date(2026, 6, 11, 13, 0, 0, 0, loc))
	if err == nil || !strings.Contains(err.Error(), "already passed") {
		t.Fatalf("past segment error = %v, want already passed", err)
	}
	_, err = ResolveWindow("later-ish", "", "", time.Date(2026, 6, 11, 10, 0, 0, 0, loc))
	if err == nil || !strings.Contains(err.Error(), "today | tomorrow") {
		t.Fatalf("invalid grammar error = %v, want grammar", err)
	}
}

func TestResolveWindowDSTOffsets(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	spring, err := ResolveWindow("today", "", "", time.Date(2026, 3, 8, 0, 30, 0, 0, loc))
	if err != nil {
		t.Fatalf("spring ResolveWindow() error = %v", err)
	}
	if got := formatLA(spring[0].Start); got != "2026-03-08T07:00:00-07:00" {
		t.Fatalf("spring start = %s", got)
	}
	fall, err := ResolveWindow("2026-11-01", "", "", time.Date(2026, 10, 31, 9, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("fall ResolveWindow() error = %v", err)
	}
	if got := formatLA(fall[0].Start); got != "2026-11-01T07:00:00-08:00" {
		t.Fatalf("fall start = %s", got)
	}
}

func TestResolveWindowEarliestLatestIntersection(t *testing.T) {
	t.Parallel()

	loc := loadLocation()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, loc)
	got, err := ResolveWindow("tomorrow", "2026-06-12T09:00:00-07:00", "2026-06-12T10:00:00-07:00", now)
	if err != nil {
		t.Fatalf("ResolveWindow() error = %v", err)
	}
	if formatLA(got[0].Start) != "2026-06-12T09:00:00-07:00" || formatLA(got[0].End) != "2026-06-12T10:00:00-07:00" {
		t.Fatalf("intersection = %#v", got[0])
	}
	got, err = ResolveWindow("", "2026-06-12T09:00:00-07:00", "2026-06-12T10:00:00-07:00", now)
	if err != nil || len(got) != 1 {
		t.Fatalf("earliest/latest-only = %#v err=%v", got, err)
	}
	_, err = ResolveWindow("tomorrow morning", "2026-06-12T18:00:00-07:00", "2026-06-12T19:00:00-07:00", now)
	if err == nil || !strings.Contains(err.Error(), "empty window") {
		t.Fatalf("empty intersection error = %v", err)
	}
}
