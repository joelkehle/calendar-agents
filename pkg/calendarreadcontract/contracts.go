// Package calendarreadcontract contains shared calendar read/write utility
// JSON shapes used by Joel's Google and Outlook calendar agents.
package calendarreadcontract

type Calendar struct {
	ID              string `json:"id"`
	Summary         string `json:"summary,omitempty"`
	Description     string `json:"description,omitempty"`
	TimeZone        string `json:"timeZone,omitempty"`
	AccessRole      string `json:"accessRole,omitempty"`
	Primary         bool   `json:"primary,omitempty"`
	BackgroundColor string `json:"backgroundColor,omitempty"`
	ForegroundColor string `json:"foregroundColor,omitempty"`
}

type EventDateTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type EventAttendee struct {
	Email          string `json:"email,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
	Optional       bool   `json:"optional,omitempty"`
	Organizer      bool   `json:"organizer,omitempty"`
	Self           bool   `json:"self,omitempty"`
}

type ExtendedProperties struct {
	Private map[string]string `json:"private,omitempty"`
	Shared  map[string]string `json:"shared,omitempty"`
}

type ReminderOverride struct {
	Method  string `json:"method,omitempty"`
	Minutes int    `json:"minutes,omitempty"`
}

type Reminders struct {
	UseDefault bool               `json:"useDefault,omitempty"`
	Overrides  []ReminderOverride `json:"overrides,omitempty"`
}

type Event struct {
	ID                 string              `json:"id,omitempty"`
	Status             string              `json:"status,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	Description        string              `json:"description,omitempty"`
	Location           string              `json:"location,omitempty"`
	Categories         []string            `json:"categories,omitempty"`
	HTMLLink           string              `json:"htmlLink,omitempty"`
	HangoutLink        string              `json:"hangoutLink,omitempty"`
	ETag               string              `json:"etag,omitempty"`
	Created            string              `json:"created,omitempty"`
	Updated            string              `json:"updated,omitempty"`
	ColorID            string              `json:"colorId,omitempty"`
	Transparency       string              `json:"transparency,omitempty"`
	Visibility         string              `json:"visibility,omitempty"`
	Sequence           int                 `json:"sequence,omitempty"`
	Start              EventDateTime       `json:"start,omitempty"`
	End                EventDateTime       `json:"end,omitempty"`
	Attendees          []EventAttendee     `json:"attendees,omitempty"`
	ExtendedProperties *ExtendedProperties `json:"extendedProperties,omitempty"`
	Reminders          *Reminders          `json:"reminders,omitempty"`
	ConferenceData     any                 `json:"conferenceData,omitempty"`
}

type EventsQuery struct {
	CalendarID                string   `json:"calendar_id,omitempty"`
	Query                     string   `json:"query,omitempty"`
	TimeMin                   string   `json:"time_min,omitempty"`
	TimeMax                   string   `json:"time_max,omitempty"`
	SingleEvents              bool     `json:"single_events,omitempty"`
	OrderBy                   string   `json:"order_by,omitempty"`
	MaxResults                int      `json:"max_results,omitempty"`
	PageToken                 string   `json:"page_token,omitempty"`
	PrivateExtendedProperties []string `json:"private_extended_properties,omitempty"`
}

type MutationOptions struct {
	SendUpdates           string `json:"send_updates,omitempty"`
	SupportsAttachments   bool   `json:"supports_attachments,omitempty"`
	ConferenceDataVersion int    `json:"conference_data_version,omitempty"`
	IfMatchETag           string `json:"if_match_etag,omitempty"`
}

type Request struct {
	Action  string          `json:"action"`
	Query   EventsQuery     `json:"query,omitempty"`
	Event   Event           `json:"event,omitempty"`
	EventID string          `json:"event_id,omitempty"`
	Options MutationOptions `json:"options,omitempty"`
}

type CalendarListResponse struct {
	Calendars []Calendar `json:"calendars,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type EventsListResponse struct {
	Events        []Event `json:"events,omitempty"`
	NextPageToken string  `json:"next_page_token,omitempty"`
	Error         string  `json:"error,omitempty"`
}

type EventResponse struct {
	Event Event  `json:"event,omitempty"`
	Error string `json:"error,omitempty"`
}
