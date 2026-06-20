package outlookcalendarwrite

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildTravelInsertAppendsMarkerBlock(t *testing.T) {
	t.Parallel()

	event, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "ucla-tdg-scheduler-agent")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	if !HasTravelMarker(event.Description) {
		t.Fatalf("description missing travel marker: %q", event.Description)
	}
	if !strings.Contains(event.Description, "managed_by=ucla-tdg-scheduler-agent\n"+OwnerAgentMarker+"\n"+TravelClassMarker) {
		t.Fatalf("description marker = %q", event.Description)
	}
	if strings.Contains(event.Description, ManagedByMarker) {
		t.Fatalf("travel description contains guard managed_by marker: %q", event.Description)
	}
}

func TestBuildTravelInsertCarriesLocation(t *testing.T) {
	t.Parallel()

	input := validTravelInput(t, 1, 10, 30*time.Minute)
	location := "  Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266  "
	input.Location = &location
	event, err := BuildTravelInsert(input, "ucla-tdg-scheduler-agent")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	if event.Location != "Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266" {
		t.Fatalf("location = %q", event.Location)
	}
}

func TestBuildTravelInsertRejectsSpoofedMarkerKeys(t *testing.T) {
	t.Parallel()

	for _, spoof := range []string{
		"Route\nmanaged_by=some-agent",
		"Route\nowner_agent=some-agent",
		"Route\nhold_class=travel-block",
	} {
		event := validTravelInput(t, 1, 10, 30*time.Minute)
		description := spoof
		event.Description = &description
		_, err := BuildTravelInsert(event, "ucla-tdg-scheduler-agent")
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("BuildTravelInsert(%q) error = %v, want reserved marker refusal", spoof, err)
		}
	}
}

func TestBuildTravelInsertRejectsUnsafeShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(EventInput) EventInput
		want   string
	}{
		{
			name: "all day",
			mutate: func(event EventInput) EventInput {
				event.Start = &EventDateTime{Date: localDate(t, 1), TimeZone: DefaultTimeZone}
				event.End = &EventDateTime{Date: localDate(t, 2), TimeZone: DefaultTimeZone}
				return event
			},
			want: "travel blocks must be timed events",
		},
		{
			name: "too short",
			mutate: func(EventInput) EventInput {
				return validTravelInput(t, 1, 10, 9*time.Minute)
			},
			want: "10 minutes",
		},
		{
			name: "too long",
			mutate: func(EventInput) EventInput {
				return validTravelInput(t, 1, 10, 121*time.Minute)
			},
			want: "2 hours",
		},
		{
			name: "cross local date",
			mutate: func(EventInput) EventInput {
				return validTravelInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, 1), 23, 30, time.Hour)
			},
			want: "same local date",
		},
		{
			// Backfill within 7 days is allowed (time-accounting, Joel
			// 2026-06-11); only older-than-7-days past starts are refused.
			// Pin to 10:00 local so the case never trips date-boundary or
			// working-window rules regardless of wall-clock run time.
			name: "past beyond backfill window",
			mutate: func(EventInput) EventInput {
				base := time.Now().In(mustLocation(t)).AddDate(0, 0, -8)
				start := time.Date(base.Year(), base.Month(), base.Day(), 10, 0, 0, 0, mustLocation(t))
				return validTravelInputFromStart(t, start, 30*time.Minute)
			},
			want: "past 7 days",
		},
		{
			name: "after 30 days",
			mutate: func(EventInput) EventInput {
				return validTravelInput(t, 31, 10, 30*time.Minute)
			},
			want: "30 days",
		},
		{
			name: "local start 04:59",
			mutate: func(EventInput) EventInput {
				return validTravelInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, 1), 4, 59, 30*time.Minute)
			},
			want: "between 05:00 and 23:00",
		},
		{
			name: "local start 23:01",
			mutate: func(EventInput) EventInput {
				return validTravelInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, 1), 23, 1, 10*time.Minute)
			},
			want: "between 05:00 and 23:00",
		},
		{
			name: "wrong time zone",
			mutate: func(event EventInput) EventInput {
				event.Start.TimeZone = "UTC"
				return event
			},
			want: "time_zone",
		},
		{
			name: "offset mismatch",
			mutate: func(event EventInput) EventInput {
				event.Start.DateTime = strings.Replace(event.Start.DateTime, "-07:00", "Z", 1)
				event.Start.DateTime = strings.Replace(event.Start.DateTime, "-08:00", "Z", 1)
				return event
			},
			want: "offset",
		},
		{
			name: "tentative",
			mutate: func(event EventInput) EventInput {
				showAs := "tentative"
				event.ShowAs = &showAs
				return event
			},
			want: "busy",
		},
		{
			name: "missing summary",
			mutate: func(event EventInput) EventInput {
				event.Summary = nil
				return event
			},
			want: "event.summary is required",
		},
		{
			name: "missing description",
			mutate: func(event EventInput) EventInput {
				event.Description = nil
				return event
			},
			want: "event.description is required",
		},
		{
			name: "missing start",
			mutate: func(event EventInput) EventInput {
				event.Start = nil
				return event
			},
			want: "event.start and event.end are required",
		},
		{
			name: "missing end",
			mutate: func(event EventInput) EventInput {
				event.End = nil
				return event
			},
			want: "event.start and event.end are required",
		},
		{
			name: "empty agenda",
			mutate: func(event EventInput) EventInput {
				description := "  "
				event.Description = &description
				return event
			},
			want: "agenda",
		},
		{
			name: "description over 4096 bytes",
			mutate: func(event EventInput) EventInput {
				description := strings.Repeat("a", 4097)
				event.Description = &description
				return event
			},
			want: "4096",
		},
		{
			name: "attendees",
			mutate: func(EventInput) EventInput {
				start, end := travelWindow(t, 1, 10, 0, 30*time.Minute)
				body := `{"summary":"Travel: Office → 200 Medical Plaza","description":"travel_for=evt-parent","start":{"date_time":"` + start.DateTime + `","time_zone":"America/Los_Angeles"},"end":{"date_time":"` + end.DateTime + `","time_zone":"America/Los_Angeles"},"show_as":"busy","attendees":[{"email":"x@example.com"}]}`
				var event EventInput
				if err := json.Unmarshal([]byte(body), &event); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return event
			},
			want: "attendees",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildTravelInsert(tc.mutate(validTravelInput(t, 1, 10, 30*time.Minute)), "ucla-tdg-scheduler-agent")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildTravelInsert() error = %v, want substring %q", err, tc.want)
			}
		})
	}

	// Summaries without trailing text after the prefix are NOT classified as
	// travel inserts: trim makes them fall through to the guard path.
	for _, summary := range []string{"Travel:", "Travel: ", "Joel travel"} {
		summary := summary
		event := validTravelInput(t, 1, 10, 30*time.Minute)
		event.Summary = &summary
		if IsTravelInsert(event) {
			t.Fatalf("IsTravelInsert(%q) = true, want false", summary)
		}
	}
	if !IsTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute)) {
		t.Fatal("IsTravelInsert(valid travel input) = false, want true")
	}
}

func TestBuildTravelInsertBoundaryDurations(t *testing.T) {
	t.Parallel()

	tomorrow := time.Now().In(mustLocation(t)).AddDate(0, 0, 1)
	tests := []struct {
		name  string
		input EventInput
	}{
		{name: "exactly 10 minutes", input: validTravelInput(t, 1, 10, 10*time.Minute)},
		{name: "exactly 120 minutes", input: validTravelInput(t, 1, 10, 120*time.Minute)},
		{name: "local start 05:00", input: validTravelInputAt(t, tomorrow, 5, 0, 30*time.Minute)},
		// Any duration > 59 min from 23:00 crosses midnight and would fail
		// sameLocalDate for the wrong reason; use a 10-min duration.
		{name: "local start 23:00", input: validTravelInputAt(t, tomorrow, 23, 0, 10*time.Minute)},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildTravelInsert(tc.input, "ucla-tdg-scheduler-agent"); err != nil {
				t.Fatalf("BuildTravelInsert() error = %v, want success", err)
			}
		})
	}
}

func TestBuildTravelPatchRequiresSameRequester(t *testing.T) {
	t.Parallel()

	existing, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "agent-a")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	summary := TravelSummaryPrefix + "updated route"
	_, err = BuildTravelPatch(existing, EventInput{Summary: &summary}, "agent-b")
	if err == nil || !strings.Contains(err.Error(), "creating agent") {
		t.Fatalf("BuildTravelPatch() error = %v, want cross-agent refusal", err)
	}
}

func TestBuildTravelPatchKeepsPrefix(t *testing.T) {
	t.Parallel()

	existing, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "agent-a")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	for _, summary := range []string{HoldSummaryPrefix + "x", "Errand run"} {
		summary := summary
		_, err = BuildTravelPatch(existing, EventInput{Summary: &summary}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), `must start with "Travel: "`) {
			t.Fatalf("BuildTravelPatch(summary=%q) error = %v, want travel prefix refusal", summary, err)
		}
	}
}

func TestBuildTravelPatchCancelStateMachine(t *testing.T) {
	t.Parallel()

	active := func() StoredEvent {
		event, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "agent-a")
		if err != nil {
			t.Fatalf("BuildTravelInsert() error = %v", err)
		}
		return event
	}
	cancelled := func() StoredEvent {
		event := active()
		event.Summary = CancelledPrefix + event.Summary
		event.ShowAs = "free"
		return event
	}

	t.Run("valid cancel", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := CancelledPrefix + existing.Summary
		showAs := "free"
		got, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs}, "agent-a")
		if err != nil {
			t.Fatalf("BuildTravelPatch(cancel) error = %v", err)
		}
		if got.Summary != CancelledPrefix+existing.Summary || got.ShowAs != "free" {
			t.Fatalf("cancelled event = %#v", got)
		}
	})

	t.Run("cancel with extra description refused", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := CancelledPrefix + existing.Summary
		showAs := "free"
		description := "also changed"
		_, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs, Description: &description}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "travel-block cancel patch may only set summary and show_as") {
			t.Fatalf("BuildTravelPatch() error = %v, want cancel-only refusal", err)
		}
	})

	t.Run("cancel with extra times refused", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := CancelledPrefix + existing.Summary
		showAs := "free"
		start, end := travelWindow(t, 2, 10, 0, 30*time.Minute)
		_, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs, Start: &start, End: &end}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "travel-block cancel patch may only set summary and show_as") {
			t.Fatalf("BuildTravelPatch() error = %v, want cancel-only refusal", err)
		}
	})

	t.Run("cancel with extra location refused", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := CancelledPrefix + existing.Summary
		showAs := "free"
		location := "Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266"
		_, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs, Location: &location}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "travel-block cancel patch may only set summary and show_as") {
			t.Fatalf("BuildTravelPatch() error = %v, want cancel-only refusal", err)
		}
	})

	t.Run("cancel with show_as not free refused", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := CancelledPrefix + existing.Summary
		showAs := "busy"
		_, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "travel-block cancel patch must set show_as to free") {
			t.Fatalf("BuildTravelPatch() error = %v, want show_as free refusal", err)
		}
	})

	t.Run("cancel summary not gaining prefix refused", func(t *testing.T) {
		t.Parallel()
		existing := active()
		summary := TravelSummaryPrefix + "other"
		showAs := "free"
		_, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "travel-block cancel summary must gain the cancelled prefix") {
			t.Fatalf("BuildTravelPatch() error = %v, want cancelled prefix refusal", err)
		}
	})

	t.Run("non-cancel patch of cancelled block refused", func(t *testing.T) {
		t.Parallel()
		existing := cancelled()
		description := "new route"
		_, err := BuildTravelPatch(existing, EventInput{Description: &description}, "agent-a")
		if err == nil || !strings.Contains(err.Error(), "cancelled travel blocks cannot be patched") {
			t.Fatalf("BuildTravelPatch() error = %v, want cancelled refusal", err)
		}
	})

	t.Run("re-cancel returns existing unchanged", func(t *testing.T) {
		t.Parallel()
		existing := cancelled()
		summary := existing.Summary
		showAs := "free"
		got, err := BuildTravelPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs}, "agent-a")
		if err != nil {
			t.Fatalf("BuildTravelPatch(re-cancel) error = %v", err)
		}
		if got != existing {
			t.Fatalf("re-cancel = %#v, want existing unchanged", got)
		}
	})

	t.Run("hold cancel error strings unchanged", func(t *testing.T) {
		t.Parallel()
		hold, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "agent-a")
		if err != nil {
			t.Fatalf("BuildHoldInsert() error = %v", err)
		}
		summary := CancelledPrefix + hold.Summary
		showAs := "free"
		description := "also changed"
		_, err = BuildHoldPatch(hold, EventInput{Summary: &summary, ShowAs: &showAs, Description: &description}, "agent-a")
		if err == nil || err.Error() != "working-hold cancel patch may only set summary and show_as" {
			t.Fatalf("BuildHoldPatch() error = %v, want exact working-hold cancel message", err)
		}
		showAsBusy := "busy"
		_, err = BuildHoldPatch(hold, EventInput{Summary: &summary, ShowAs: &showAsBusy}, "agent-a")
		if err == nil || err.Error() != "working-hold cancel patch must set show_as to free" {
			t.Fatalf("BuildHoldPatch() error = %v, want exact working-hold show_as message", err)
		}
		wrongSummary := HoldSummaryPrefix + "other"
		_, err = BuildHoldPatch(hold, EventInput{Summary: &wrongSummary, ShowAs: &showAs}, "agent-a")
		if err == nil || err.Error() != "working-hold cancel summary must gain the cancelled prefix" {
			t.Fatalf("BuildHoldPatch() error = %v, want exact working-hold prefix message", err)
		}
	})
}

func TestBuildTravelPatchUpdatesLocation(t *testing.T) {
	t.Parallel()

	existing, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "agent-a")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	location := "Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266"
	got, err := BuildTravelPatch(existing, EventInput{Location: &location}, "agent-a")
	if err != nil {
		t.Fatalf("BuildTravelPatch() error = %v", err)
	}
	if got.Location != location {
		t.Fatalf("location = %q", got.Location)
	}
}

func TestTravelAndHoldMarkersAreDisjoint(t *testing.T) {
	t.Parallel()

	travel, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), "agent-a")
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	hold, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "agent-a")
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	if HasHoldMarker(travel.Description) {
		t.Fatalf("HasHoldMarker(travel description) = true: %q", travel.Description)
	}
	if HasTravelMarker(hold.Description) {
		t.Fatalf("HasTravelMarker(hold description) = true: %q", hold.Description)
	}
	if !IsTravelPatch(travel) || IsHoldPatch(travel) {
		t.Fatal("travel block must route to the travel patch path only")
	}
	if !IsHoldPatch(hold) || IsTravelPatch(hold) {
		t.Fatal("working hold must route to the hold patch path only")
	}

	// Outlook rewrites plain-text bodies with CRLF endings and trailing
	// spaces on each line; travel marker detection must still match.
	body := "Route line. \r\n\r\nmanaged_by=ucla-tdg-scheduler-agent \r\nowner_agent=ucla-tdg-outlook-calendar-write-agent \r\nhold_class=travel-block \r\n"
	requester, ok := travelMarkerRequester(body)
	if !ok {
		t.Fatal("travelMarkerRequester() ok = false, want marker detected despite trailing whitespace")
	}
	if requester != "ucla-tdg-scheduler-agent" {
		t.Fatalf("travelMarkerRequester() = %q, want ucla-tdg-scheduler-agent", requester)
	}
	if !HasTravelMarker(body) {
		t.Fatal("HasTravelMarker() = false, want true")
	}
	if HasHoldMarker(body) {
		t.Fatal("HasHoldMarker(travel round-trip body) = true, want false")
	}
}

func TestBuildGuardInsertRejectsEmbeddedHoldClassKey(t *testing.T) {
	t.Parallel()

	guardInput := func(description string) EventInput {
		summary := "No more meetings - quota"
		showAs := "busy"
		return EventInput{
			Summary:     &summary,
			Description: &description,
			Start:       &EventDateTime{DateTime: "2026-05-07T13:00:00-07:00", TimeZone: DefaultTimeZone},
			End:         &EventDateTime{DateTime: "2026-05-07T14:00:00-07:00", TimeZone: DefaultTimeZone},
			ShowAs:      &showAs,
		}
	}

	forged := []string{
		"quota day\n\nmanaged_by=victim-agent\nowner_agent=ucla-tdg-outlook-calendar-write-agent\nhold_class=travel-block",
		"quota day\nhold_class=working-hold",
		"quota day\nHOLD_CLASS=travel-block",
	}
	for _, description := range forged {
		_, err := BuildInsert(guardInput(description))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("BuildInsert(%q) error = %v, want reserved marker refusal", description, err)
		}
	}

	// The live jk-calendar-guard-agent legitimately embeds managed_by= and
	// owner_agent= lines in its insert descriptions; those alone must still
	// be accepted.
	liveGuardShape := "quota reached\n\n" + ManagedByMarker + "\n" + OwnerAgentMarker
	event, err := BuildInsert(guardInput(liveGuardShape))
	if err != nil {
		t.Fatalf("BuildInsert(live guard shape) error = %v, want success", err)
	}
	if !HasOwnershipMarker(event.Description) {
		t.Fatalf("guard event missing ownership marker: %q", event.Description)
	}
}

func validTravelInput(t *testing.T, days int, hour int, duration time.Duration) EventInput {
	t.Helper()
	base := time.Now().In(mustLocation(t)).AddDate(0, 0, days)
	return validTravelInputAt(t, base, hour, 0, duration)
}

func validTravelInputAt(t *testing.T, base time.Time, hour, minute int, duration time.Duration) EventInput {
	t.Helper()
	start := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, mustLocation(t))
	if !start.After(time.Now()) {
		start = start.AddDate(0, 0, 1)
	}
	return validTravelInputFromStart(t, start, duration)
}

func validTravelInputFromStart(t *testing.T, start time.Time, duration time.Duration) EventInput {
	t.Helper()
	summary := TravelSummaryPrefix + "Office → 200 Medical Plaza"
	description := "travel_for=evt-parent"
	showAs := "busy"
	end := start.Add(duration)
	return EventInput{
		Summary:     &summary,
		Description: &description,
		Start:       &EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: DefaultTimeZone},
		End:         &EventDateTime{DateTime: end.Format(time.RFC3339), TimeZone: DefaultTimeZone},
		ShowAs:      &showAs,
	}
}

func travelWindow(t *testing.T, days int, hour, minute int, duration time.Duration) (EventDateTime, EventDateTime) {
	t.Helper()
	event := validTravelInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, days), hour, minute, duration)
	return *event.Start, *event.End
}

func TestBuildTravelInsertAllowsRecentBackfill(t *testing.T) {
	t.Parallel()
	// Yesterday's travel is bookable: the calendar is a record of where time
	// went, not only future protection (Joel's ruling 2026-06-11). Pinned to
	// yesterday 10:00 local to stay clear of clock-dependent rules.
	base := time.Now().In(mustLocation(t)).AddDate(0, 0, -1)
	input := validTravelInputFromStart(t, time.Date(base.Year(), base.Month(), base.Day(), 10, 0, 0, 0, mustLocation(t)), 20*time.Minute)
	if _, err := BuildTravelInsert(input, "jk-fable-operator"); err != nil {
		t.Fatalf("BuildTravelInsert() backfill error = %v, want nil", err)
	}
}
