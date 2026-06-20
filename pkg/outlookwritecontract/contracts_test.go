package outlookwritecontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventDateTimeAcceptsSnakeAndCamelCaseInputs(t *testing.T) {
	var snake EventDateTime
	if err := json.Unmarshal([]byte(`{"date_time":"2026-06-20T09:00:00-07:00","time_zone":"America/Los_Angeles"}`), &snake); err != nil {
		t.Fatal(err)
	}
	if snake.DateTime != "2026-06-20T09:00:00-07:00" || snake.TimeZone != "America/Los_Angeles" {
		t.Fatalf("snake decode = %#v", snake)
	}

	var camel EventDateTime
	if err := json.Unmarshal([]byte(`{"dateTime":"2026-06-20T09:00:00-07:00","timeZone":"America/Los_Angeles"}`), &camel); err != nil {
		t.Fatal(err)
	}
	if camel.DateTime != snake.DateTime || camel.TimeZone != snake.TimeZone {
		t.Fatalf("camel decode = %#v, want %#v", camel, snake)
	}
}

func TestEventInputCapturesProhibitedFields(t *testing.T) {
	var event EventInput
	if err := json.Unmarshal([]byte(`{"summary":"Joel + focus","attendees":[],"conferenceData":{},"recurrence_rule":"RRULE:FREQ=DAILY"}`), &event); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(event.ProhibitedFields(), ",")
	for _, field := range []string{"attendees", "conferenceData", "recurrence_rule"} {
		if !strings.Contains(got, field) {
			t.Fatalf("ProhibitedFields() = %q, missing %s", got, field)
		}
	}

	fields := event.ProhibitedFields()
	fields[0] = "mutated"
	if event.ProhibitedFields()[0] == "mutated" {
		t.Fatal("ProhibitedFields returned mutable internal slice")
	}
}

func TestMutationResponsePreservesReplayShape(t *testing.T) {
	body, err := json.Marshal(MutationResponse{
		DryRun:   false,
		Replayed: true,
		Event: &StoredEvent{
			ID:      "evt-1",
			Summary: "Travel: office",
			Start:   EventDateTime{DateTime: "2026-06-20T09:00:00-07:00", TimeZone: "America/Los_Angeles"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, field := range []string{`"dry_run"`, `"replayed"`, `"date_time"`, `"time_zone"`} {
		if !strings.Contains(text, field) {
			t.Fatalf("mutation response JSON %s missing %s", text, field)
		}
	}
}
