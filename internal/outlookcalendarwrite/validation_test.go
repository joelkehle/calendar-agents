package outlookcalendarwrite

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildInsertAddsMarkerAndValidatesGuardBlock(t *testing.T) {
	t.Parallel()

	summary := "No more meetings - inbox recovery"
	description := "Inbox recovery block"
	showAs := "busy"
	event, err := BuildInsert(EventInput{
		Summary:     &summary,
		Description: &description,
		Start:       &EventDateTime{DateTime: "2026-05-07T13:00:00-07:00", TimeZone: DefaultTimeZone},
		End:         &EventDateTime{DateTime: "2026-05-07T15:00:00-07:00", TimeZone: DefaultTimeZone},
		ShowAs:      &showAs,
	})
	if err != nil {
		t.Fatalf("BuildInsert() error = %v", err)
	}
	if !HasOwnershipMarker(event.Description) {
		t.Fatalf("description missing marker: %q", event.Description)
	}
	if event.ShowAs != "busy" {
		t.Fatalf("ShowAs = %q", event.ShowAs)
	}
}

func TestBuildInsertAllowsOwnedAllDayQuotaGuard(t *testing.T) {
	t.Parallel()

	summary := AllowedDaySummary
	description := "Inbox recovery day"
	showAs := "busy"
	event, err := BuildInsert(EventInput{
		Summary:     &summary,
		Description: &description,
		Start:       &EventDateTime{Date: "2026-05-07", TimeZone: DefaultTimeZone},
		End:         &EventDateTime{Date: "2026-05-08", TimeZone: DefaultTimeZone},
		ShowAs:      &showAs,
	})
	if err != nil {
		t.Fatalf("BuildInsert() error = %v", err)
	}
	if event.Start.Date != "2026-05-07" || event.End.Date != "2026-05-08" {
		t.Fatalf("all-day dates = %#v %#v", event.Start, event.End)
	}
	if event.Start.DateTime != "" || event.End.DateTime != "" {
		t.Fatalf("all-day event includes date_time: %#v %#v", event.Start, event.End)
	}
	if !HasOwnershipMarker(event.Description) {
		t.Fatalf("description missing marker: %q", event.Description)
	}
}

func TestBuildInsertRejectsUnsafeShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "wrong summary prefix",
			body: `{"summary":"Focus block","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date_time":"2026-05-07T13:00:00-07:00","time_zone":"America/Los_Angeles"},"end":{"date_time":"2026-05-07T14:00:00-07:00","time_zone":"America/Los_Angeles"},"show_as":"busy"}`,
			want: "summary",
		},
		{
			name: "too long",
			body: `{"summary":"No more meetings - long","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date_time":"2026-05-07T13:00:00-07:00","time_zone":"America/Los_Angeles"},"end":{"date_time":"2026-05-07T18:00:00-07:00","time_zone":"America/Los_Angeles"},"show_as":"busy"}`,
			want: "4 hours",
		},
		{
			name: "all day too long",
			body: `{"summary":"Meeting Quota Reached","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date":"2026-05-07","time_zone":"America/Los_Angeles"},"end":{"date":"2026-05-09","time_zone":"America/Los_Angeles"},"show_as":"busy"}`,
			want: "exactly one local day",
		},
		{
			name: "mixed all day and timed",
			body: `{"summary":"Meeting Quota Reached","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date":"2026-05-07","date_time":"2026-05-07T00:00:00-07:00","time_zone":"America/Los_Angeles"},"end":{"date":"2026-05-08","time_zone":"America/Los_Angeles"},"show_as":"busy"}`,
			want: "date only",
		},
		{
			name: "attendees",
			body: `{"summary":"No more meetings - invite","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date_time":"2026-05-07T13:00:00-07:00","time_zone":"America/Los_Angeles"},"end":{"date_time":"2026-05-07T14:00:00-07:00","time_zone":"America/Los_Angeles"},"show_as":"busy","attendees":[{"email":"x@example.com"}]}`,
			want: "attendees",
		},
		{
			name: "recurrence",
			body: `{"summary":"No more meetings - recurring","description":"` + ManagedByMarker + `\n` + OwnerAgentMarker + `","start":{"date_time":"2026-05-07T13:00:00-07:00","time_zone":"America/Los_Angeles"},"end":{"date_time":"2026-05-07T14:00:00-07:00","time_zone":"America/Los_Angeles"},"show_as":"busy","recurrence":["RRULE:FREQ=DAILY"]}`,
			want: "recurrence",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var event EventInput
			if err := json.Unmarshal([]byte(tc.body), &event); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, err := BuildInsert(event)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildInsert() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestBuildPatchRefusesUnownedExistingEvent(t *testing.T) {
	t.Parallel()

	summary := "No more meetings - shorter"
	_, err := BuildPatch(StoredEvent{
		ID:          "evt-1",
		Summary:     "No more meetings - old",
		Description: "ordinary appointment",
		Start:       EventDateTime{DateTime: "2026-05-07T13:00:00-07:00", TimeZone: DefaultTimeZone},
		End:         EventDateTime{DateTime: "2026-05-07T14:00:00-07:00", TimeZone: DefaultTimeZone},
		ShowAs:      "busy",
	}, EventInput{Summary: &summary})
	if err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("BuildPatch() error = %v, want ownership marker refusal", err)
	}
}
