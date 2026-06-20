package outlookcalendarwrite

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildHoldInsertAppendsExactRequesterMarker(t *testing.T) {
	t.Parallel()

	event, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "ucla-tdg-project-manager")
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	if !HasHoldMarker(event.Description) {
		t.Fatalf("description missing hold marker: %q", event.Description)
	}
	if !strings.Contains(event.Description, "managed_by=ucla-tdg-project-manager\n"+OwnerAgentMarker+"\n"+HoldClassMarker) {
		t.Fatalf("description marker = %q", event.Description)
	}
	if strings.Contains(event.Description, ManagedByMarker) {
		t.Fatalf("hold description contains guard managed_by marker: %q", event.Description)
	}
}

func TestBuildHoldInsertRejectsSpoofedMarkerKeys(t *testing.T) {
	t.Parallel()

	event := validHoldInput(t, 1, 10, time.Hour)
	description := "Agenda\nmanaged_by=some-agent"
	event.Description = &description
	_, err := BuildHoldInsert(event, "ucla-tdg-project-manager")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("BuildHoldInsert() error = %v, want reserved marker refusal", err)
	}
}

func TestBuildHoldInsertRejectsUnsafeShapes(t *testing.T) {
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
			want: "timed",
		},
		{
			name: "too short",
			mutate: func(EventInput) EventInput {
				return validHoldInput(t, 1, 10, 10*time.Minute)
			},
			want: "15 minutes",
		},
		{
			name: "too long",
			mutate: func(EventInput) EventInput {
				return validHoldInput(t, 1, 10, 3*time.Hour)
			},
			want: "2 hours",
		},
		{
			name: "cross local date",
			mutate: func(EventInput) EventInput {
				return validHoldInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, 1), 23, 30, time.Hour)
			},
			want: "same local date",
		},
		{
			name: "past",
			mutate: func(EventInput) EventInput {
				return validHoldInputFromStart(t, time.Now().In(mustLocation(t)).Add(-time.Hour), 30*time.Minute)
			},
			want: "future",
		},
		{
			name: "after 30 days",
			mutate: func(EventInput) EventInput {
				return validHoldInput(t, 31, 10, time.Hour)
			},
			want: "30 days",
		},
		{
			name: "too early",
			mutate: func(EventInput) EventInput {
				return validHoldInput(t, 1, 6, time.Hour)
			},
			want: "07:00",
		},
		{
			name: "too late",
			mutate: func(EventInput) EventInput {
				return validHoldInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, 1), 21, 30, 30*time.Minute)
			},
			want: "07:00",
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
			name: "free",
			mutate: func(event EventInput) EventInput {
				showAs := "free"
				event.ShowAs = &showAs
				return event
			},
			want: "busy",
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
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildHoldInsert(tc.mutate(validHoldInput(t, 1, 10, time.Hour)), "ucla-tdg-project-manager")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildHoldInsert() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestBuildHoldInsertRejectsProhibitedFields(t *testing.T) {
	t.Parallel()

	start, end := holdWindow(t, 1, 10, 0, time.Hour)
	body := `{"summary":"Joel + focus","description":"Agenda","start":{"date_time":"` + start.DateTime + `","time_zone":"America/Los_Angeles"},"end":{"date_time":"` + end.DateTime + `","time_zone":"America/Los_Angeles"},"show_as":"busy","attendees":[{"email":"x@example.com"}]}`
	var event EventInput
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, err := BuildHoldInsert(event, "ucla-tdg-project-manager")
	if err == nil || !strings.Contains(err.Error(), "attendees") {
		t.Fatalf("BuildHoldInsert() error = %v, want attendees refusal", err)
	}
}

func TestBuildHoldPatchRejectsCrossAgentPatch(t *testing.T) {
	t.Parallel()

	existing, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "agent-a")
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	summary := HoldSummaryPrefix + "updated focus"
	_, err = BuildHoldPatch(existing, EventInput{Summary: &summary}, "agent-b")
	if err == nil || !strings.Contains(err.Error(), "creating agent") {
		t.Fatalf("BuildHoldPatch() error = %v, want cross-agent refusal", err)
	}
}

func TestBuildHoldPatchCancelThenPatchRefused(t *testing.T) {
	t.Parallel()

	existing, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "agent-a")
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	summary := CancelledPrefix + existing.Summary
	showAs := "free"
	cancelled, err := BuildHoldPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs}, "agent-a")
	if err != nil {
		t.Fatalf("BuildHoldPatch(cancel) error = %v", err)
	}
	description := "New agenda"
	_, err = BuildHoldPatch(cancelled, EventInput{Description: &description}, "agent-a")
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("BuildHoldPatch(after cancel) error = %v, want cancelled refusal", err)
	}
}

func TestBuildHoldPatchRejectsCancelWithOtherChanges(t *testing.T) {
	t.Parallel()

	existing, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), "agent-a")
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	summary := CancelledPrefix + existing.Summary
	showAs := "free"
	description := "Also changed"
	_, err = BuildHoldPatch(existing, EventInput{Summary: &summary, ShowAs: &showAs, Description: &description}, "agent-a")
	if err == nil || !strings.Contains(err.Error(), "only set summary and show_as") {
		t.Fatalf("BuildHoldPatch() error = %v, want cancel-only refusal", err)
	}
}

func validHoldInput(t *testing.T, days int, hour int, duration time.Duration) EventInput {
	t.Helper()
	base := time.Now().In(mustLocation(t)).AddDate(0, 0, days)
	return validHoldInputAt(t, base, hour, 0, duration)
}

func validHoldInputAt(t *testing.T, base time.Time, hour, minute int, duration time.Duration) EventInput {
	t.Helper()
	start := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, mustLocation(t))
	if !start.After(time.Now()) {
		start = start.AddDate(0, 0, 1)
	}
	return validHoldInputFromStart(t, start, duration)
}

func validHoldInputFromStart(t *testing.T, start time.Time, duration time.Duration) EventInput {
	t.Helper()
	summary := HoldSummaryPrefix + "focus"
	description := "Agenda"
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

func holdWindow(t *testing.T, days int, hour, minute int, duration time.Duration) (EventDateTime, EventDateTime) {
	t.Helper()
	event := validHoldInputAt(t, time.Now().In(mustLocation(t)).AddDate(0, 0, days), hour, minute, duration)
	return *event.Start, *event.End
}

func localDate(t *testing.T, days int) string {
	t.Helper()
	return time.Now().In(mustLocation(t)).AddDate(0, 0, days).Format("2006-01-02")
}

func mustLocation(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return location
}

func TestHoldMarkerSurvivesOutlookBodyRoundTrip(t *testing.T) {
	t.Parallel()

	// Outlook rewrites plain-text bodies with CRLF endings and trailing
	// spaces on each line (observed live 2026-06-11); marker detection must
	// still match.
	body := "Agenda line. \r\n\r\nmanaged_by=jk-fable-operator \r\nowner_agent=ucla-tdg-outlook-calendar-write-agent \r\nhold_class=working-hold \r\n"
	requester, ok := holdMarkerRequester(body)
	if !ok {
		t.Fatalf("holdMarkerRequester() ok = false, want marker detected despite trailing whitespace")
	}
	if requester != "jk-fable-operator" {
		t.Fatalf("holdMarkerRequester() = %q, want jk-fable-operator", requester)
	}
	if !HasHoldMarker(body) {
		t.Fatalf("HasHoldMarker() = false, want true")
	}
}
