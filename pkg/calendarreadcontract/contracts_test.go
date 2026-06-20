package calendarreadcontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventJSONPreservesMixedGoogleAndOutlookFields(t *testing.T) {
	event := Event{
		ID:           "evt-1",
		Summary:      "Meeting",
		Categories:   []string{"Yellow category"},
		HTMLLink:     "https://calendar.example/evt-1",
		ColorID:      "5",
		Transparency: "opaque",
		Start:        EventDateTime{DateTime: "2026-06-20T09:00:00-07:00", TimeZone: "America/Los_Angeles"},
		End:          EventDateTime{DateTime: "2026-06-20T10:00:00-07:00", TimeZone: "America/Los_Angeles"},
		Attendees:    []EventAttendee{{Email: "joel@example.com", Self: true}},
		ExtendedProperties: &ExtendedProperties{Private: map[string]string{
			"entry_id": "00000000ABCD",
		}},
	}

	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, field := range []string{
		`"htmlLink"`,
		`"colorId"`,
		`"dateTime"`,
		`"timeZone"`,
		`"extendedProperties"`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("event JSON %s missing %s", text, field)
		}
	}
}

func TestRequestJSONPreservesCalendarQueryShape(t *testing.T) {
	body, err := json.Marshal(Request{
		Action: "events-list",
		Query: EventsQuery{
			CalendarID:                "default",
			TimeMin:                   "2026-06-20T00:00:00-07:00",
			TimeMax:                   "2026-06-21T00:00:00-07:00",
			SingleEvents:              true,
			OrderBy:                   "startTime",
			PrivateExtendedProperties: []string{"entry_id=abc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, field := range []string{
		`"calendar_id"`,
		`"time_min"`,
		`"time_max"`,
		`"single_events"`,
		`"order_by"`,
		`"private_extended_properties"`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("request JSON %s missing %s", text, field)
		}
	}
}
