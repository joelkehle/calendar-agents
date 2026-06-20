// Package schedulercontract contains the shared bus schema for Joel's
// scheduling and travel-time orchestrator.
package schedulercontract

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/pkg/calendarcontract"
	"github.com/joelkehle/calendar-agents/pkg/calendaridentity"
)

const (
	DefaultAgentID            = calendaridentity.SchedulerAgentID
	DefaultBusURL             = "http://localhost:8080"
	DefaultHTTPAddr           = ":8245"
	DefaultCalendarReadAgent  = calendaridentity.ProfessionalOutlookReadAgentID
	DefaultCalendarWriteAgent = calendaridentity.OutlookWriteAgentID
	DefaultTimeZone           = calendarcontract.DefaultTimeZone

	CapabilityRequest  = "schedule-request"
	CapabilityMove     = "schedule-move"
	CapabilityCancel   = "schedule-cancel"
	CapabilityEstimate = "travel-estimate"

	StatusBooked     = "booked"
	StatusMoved      = "moved"
	StatusCancelled  = "cancelled"
	StatusInfeasible = "infeasible"
	StatusRefused    = "refused"
	StatusError      = "error"
	StatusEstimated  = "estimated"

	ErrorInvalidWindow       = "invalid_window"
	ErrorInvalidRequest      = "invalid_request"
	ErrorUpstreamUnavailable = "upstream_unavailable"
	ErrorNotOwned            = "not_owned"
	ErrorBookingRefused      = "booking_refused"
	ErrorOtherPeople         = "involves_other_people"
	ErrorEstimateUnavailable = "estimate_unavailable"
	ErrorTravelBooking       = "travel_booking_failed"

	// DefaultOffsiteCategory is Outlook's default display name for the yellow
	// category; Joel's 2026-06-11 ruling: yellow = offsite. Consumers should
	// keep a contains-"yellow" fallback for renamed category labels.
	DefaultOffsiteCategory = "Yellow category"
)

type Request struct {
	Action          string `json:"action"`
	RequestID       string `json:"request_id"`
	Purpose         string `json:"purpose,omitempty"`
	RequesterLabel  string `json:"requester_label,omitempty"`
	DurationMinutes int    `json:"duration_minutes,omitempty"`
	Window          string `json:"window,omitempty"`
	Agenda          string `json:"agenda,omitempty"`
	Earliest        string `json:"earliest,omitempty"`
	Latest          string `json:"latest,omitempty"`
	EventID         string `json:"event_id,omitempty"`
	// Location is optional free text on schedule-request and required on
	// travel-estimate. It is accepted-and-ignored on schedule-move and
	// schedule-cancel in the current runtime.
	Location string `json:"location,omitempty"`
	// EventStart is the travel-estimate meeting start (RFC3339 with offset).
	EventStart string `json:"event_start,omitempty"`

	prohibitedField string
}

type Reply struct {
	Status             string `json:"status"`
	RequestID          string `json:"request_id"`
	EventID            string `json:"event_id,omitempty"`
	Start              string `json:"start,omitempty"`
	End                string `json:"end,omitempty"`
	Summary            string `json:"summary,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
	Message            string `json:"message,omitempty"`
	NearestAlternative *Slot  `json:"nearest_alternative,omitempty"`
	// Estimate is set only on travel-estimate replies; Travel only on booked
	// offsite schedule-request replies. Nil pointers preserve pre-travel reply
	// serialization.
	Estimate *EstimateResult `json:"estimate,omitempty"`
	Travel   *TravelBooking  `json:"travel,omitempty"`
}

// EstimateOrigin / EstimateVenue / EstimateResult form the travel-estimate
// reply payload. Inner fields are plain types so false booleans serialize
// normally.
type EstimateOrigin struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type EstimateVenue struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Parking string `json:"parking,omitempty"`
}

type EstimateResult struct {
	Minutes      int             `json:"minutes"`
	DriveMinutes int             `json:"drive_minutes"`
	WalkMinutes  int             `json:"walk_minutes"`
	Origin       *EstimateOrigin `json:"origin,omitempty"`
	Venue        *EstimateVenue  `json:"venue,omitempty"`
	Source       string          `json:"source"`
	IsOffice     bool            `json:"is_office"`
	IsVirtual    bool            `json:"is_virtual"`
}

// TravelBooking is the booked-offsite reply extension.
type TravelBooking struct {
	Minutes        int        `json:"minutes"`
	OriginID       string     `json:"origin_id"`
	EstimateSource string     `json:"estimate_source"`
	Before         *TravelLeg `json:"before"`
	After          *TravelLeg `json:"after"`
	Notes          []string   `json:"notes"`
}

type TravelLeg struct {
	EventID string `json:"event_id"`
	Start   string `json:"start"`
	End     string `json:"end"`
}

type Slot struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Interval struct {
	Start time.Time
	End   time.Time
}

type RequestError struct {
	Code    string
	Message string
}

func (e RequestError) Error() string {
	return strings.TrimSpace(e.Message)
}

func DecodeRequest(body string) (Request, error) {
	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return Request{}, err
	}
	req.prohibitedField = FirstProhibitedField([]byte(body))
	return req, nil
}

func (r Request) ProhibitedField() string {
	return strings.TrimSpace(r.prohibitedField)
}

func (r Reply) Terminal() bool {
	switch r.Status {
	case StatusBooked, StatusMoved, StatusCancelled, StatusInfeasible, StatusRefused, StatusError:
		return true
	default:
		return false
	}
}

func ErrorReply(requestID, code, message string) Reply {
	return Reply{
		Status:    StatusError,
		RequestID: strings.TrimSpace(requestID),
		ErrorCode: strings.TrimSpace(code),
		Message:   strings.TrimSpace(message),
	}
}

func RefusedReply(requestID string) Reply {
	return Reply{
		Status:    StatusRefused,
		RequestID: strings.TrimSpace(requestID),
		ErrorCode: ErrorOtherPeople,
		Message:   "escalate to Joel",
	}
}

func FirstProhibitedField(body []byte) string {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return ""
	}
	return scanProhibited(value)
}

func scanProhibited(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if prohibitedSchedulerField(key) {
				return key
			}
			if found := scanProhibited(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := scanProhibited(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func prohibitedSchedulerField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "attendee", "attendees", "invitee", "invitees", "guests",
		"conferencedata", "conference_data", "recurrence", "recurrence_rule", "recurrencerule":
		return true
	default:
		return false
	}
}
