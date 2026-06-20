package outlookcalendar

import (
	"encoding/json"
	"reflect"
	"testing"

	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

func TestDecodeRowsAcceptsSingleObjectAndArray(t *testing.T) {
	t.Parallel()

	rows, err := decodeRows([]byte(`{"ID":"one","Start":"2026-05-05T09:00:00","End":"2026-05-05T09:30:00","Subject":"Planning"}`))
	if err != nil {
		t.Fatalf("decodeRows(single) error = %v", err)
	}
	if len(rows) != 1 || rows[0].Subject != "Planning" {
		t.Fatalf("single rows = %#v", rows)
	}

	rows, err = decodeRows([]byte(`[{"ID":"one"},{"ID":"two"}]`))
	if err != nil {
		t.Fatalf("decodeRows(array) error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("array rows = %#v", rows)
	}
}

func TestRowsToEventsNormalizesOutlookRows(t *testing.T) {
	t.Parallel()

	events := rowsToEvents([]outlookRow{{
		ID:                "global-id",
		EntryID:           "entry-id",
		Start:             "2026-05-05T09:00:00.0000000",
		End:               "2026-05-05T09:30:00.0000000",
		Subject:           "Design review",
		Location:          "Zoom",
		Categories:        "UCLA, Meeting ,  Guard",
		BusyStatus:        2,
		Sensitivity:       0,
		Organizer:         "Joel Kehle",
		RequiredAttendees: "Ada Lovelace; Grace Hopper",
	}}, "", DefaultTimeZone)

	if len(events) != 1 {
		t.Fatalf("events len = %d", len(events))
	}
	event := events[0]
	if event.ID == "" || event.ID == "global-id" {
		t.Fatalf("event ID = %q", event.ID)
	}
	if event.Summary != "Design review" || event.Location != "Zoom" {
		t.Fatalf("event = %#v", event)
	}
	if !reflect.DeepEqual(event.Categories, []string{"UCLA", "Meeting", "Guard"}) {
		t.Fatalf("event categories = %#v", event.Categories)
	}
	if event.Start.DateTime == "" || event.End.DateTime == "" {
		t.Fatalf("event times = %#v %#v", event.Start, event.End)
	}
	if event.Transparency != "opaque" || event.Visibility != "default" {
		t.Fatalf("event transparency/visibility = %q/%q", event.Transparency, event.Visibility)
	}
	if len(event.Attendees) != 3 {
		blob, _ := json.Marshal(event.Attendees)
		t.Fatalf("attendees = %s", blob)
	}
}

func TestExtractorEntryID(t *testing.T) {
	t.Parallel()

	events := rowsToEvents([]outlookRow{
		{
			ID:      "global-id",
			EntryID: "  00000000ABCDEF0123456789  ",
			Start:   "2026-06-12T09:00:00",
			End:     "2026-06-12T09:30:00",
			Subject: "With entry id",
		},
		{
			ID:      "global-id-2",
			Start:   "2026-06-12T10:00:00",
			End:     "2026-06-12T10:30:00",
			Subject: "Without entry id",
		},
	}, "", DefaultTimeZone)

	if len(events) != 2 {
		t.Fatalf("events len = %d", len(events))
	}
	withEntry := events[0]
	if withEntry.ExtendedProperties == nil || withEntry.ExtendedProperties.Private == nil {
		t.Fatalf("extended properties missing: %#v", withEntry.ExtendedProperties)
	}
	if got := withEntry.ExtendedProperties.Private["entry_id"]; got != "00000000ABCDEF0123456789" {
		t.Fatalf("entry_id = %q, want raw trimmed EntryID", got)
	}
	if withEntry.ExtendedProperties.Private["source_entry"] == "" ||
		withEntry.ExtendedProperties.Private["source_entry"] == "00000000ABCDEF0123456789" {
		t.Fatalf("source_entry must stay hashed: %q", withEntry.ExtendedProperties.Private["source_entry"])
	}
	withoutEntry := events[1]
	if _, ok := withoutEntry.ExtendedProperties.Private["entry_id"]; ok {
		t.Fatalf("entry_id key must be absent when EntryID is empty: %#v", withoutEntry.ExtendedProperties.Private)
	}
}

func TestNewPowerShellExtractorIncludesPrivateDetailsByDefault(t *testing.T) {
	t.Parallel()

	if !NewPowerShellExtractor().IncludePrivateDetails {
		t.Fatal("IncludePrivateDetails default = false, want true")
	}
}

func TestEventWindowDefaultsAndRejectsLargeWindows(t *testing.T) {
	t.Parallel()

	start, end, err := eventWindow(calendarread.EventsQuery{})
	if err != nil {
		t.Fatalf("eventWindow(default) error = %v", err)
	}
	if end.Sub(start).Hours() != 24 {
		t.Fatalf("default window = %s", end.Sub(start))
	}

	_, _, err = eventWindow(calendarread.EventsQuery{
		TimeMin: "2026-05-01T00:00:00-07:00",
		TimeMax: "2026-06-15T00:00:00-07:00",
	})
	if err == nil {
		t.Fatal("eventWindow accepted too-large window")
	}
}
