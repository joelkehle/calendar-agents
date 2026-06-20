// Package outlookwritecontract contains the shared JSON schema for Joel's
// narrow Outlook calendar write agent.
package outlookwritecontract

import (
	"encoding/json"
	"strings"
)

type Request struct {
	Action     string     `json:"action"`
	CalendarID string     `json:"calendar_id,omitempty"`
	EventID    string     `json:"event_id,omitempty"`
	Event      EventInput `json:"event,omitempty"`
}

type EventDateTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"date_time,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
}

func (d *EventDateTime) UnmarshalJSON(data []byte) error {
	var raw struct {
		Date          string `json:"date"`
		DateTime      string `json:"date_time"`
		DateTimeCamel string `json:"dateTime"`
		TimeZone      string `json:"time_zone"`
		TimeZoneCamel string `json:"timeZone"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.Date = strings.TrimSpace(raw.Date)
	d.DateTime = firstNonEmpty(raw.DateTime, raw.DateTimeCamel)
	d.TimeZone = firstNonEmpty(raw.TimeZone, raw.TimeZoneCamel)
	return nil
}

type EventInput struct {
	Summary     *string        `json:"summary,omitempty"`
	Description *string        `json:"description,omitempty"`
	Location    *string        `json:"location,omitempty"`
	Start       *EventDateTime `json:"start,omitempty"`
	End         *EventDateTime `json:"end,omitempty"`
	ShowAs      *string        `json:"show_as,omitempty"`

	prohibitedFields []string
}

func (e *EventInput) UnmarshalJSON(data []byte) error {
	type alias EventInput
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, field := range []string{
		"attendees",
		"recurrence",
		"recurrence_rule",
		"recurrenceRule",
		"conferenceData",
		"conference_data",
	} {
		if _, ok := raw[field]; ok {
			decoded.prohibitedFields = append(decoded.prohibitedFields, field)
		}
	}
	*e = EventInput(decoded)
	return nil
}

func (e EventInput) ProhibitedFields() []string {
	out := make([]string, len(e.prohibitedFields))
	copy(out, e.prohibitedFields)
	return out
}

type StoredEvent struct {
	ID          string        `json:"id,omitempty"`
	Summary     string        `json:"summary,omitempty"`
	Description string        `json:"description,omitempty"`
	Location    string        `json:"location,omitempty"`
	Start       EventDateTime `json:"start,omitempty"`
	End         EventDateTime `json:"end,omitempty"`
	ShowAs      string        `json:"show_as,omitempty"`
}

type MutationResponse struct {
	DryRun     bool         `json:"dry_run"`
	Event      *StoredEvent `json:"event,omitempty"`
	WouldWrite *StoredEvent `json:"would_write,omitempty"`
	Error      string       `json:"error,omitempty"`
	ErrorCode  string       `json:"error_code,omitempty"`
	// Replayed is true when this response was served from the 24 h
	// idempotency cache rather than freshly written. The cached StoredEvent
	// reflects the event as inserted, not its current state.
	Replayed bool `json:"replayed,omitempty"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
