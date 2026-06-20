package schedulercontract

import (
	"encoding/json"
	"testing"
)

func TestSchedulerConstantsPreserveLiveWireValues(t *testing.T) {
	tests := map[string]string{
		"DefaultAgentID":            DefaultAgentID,
		"DefaultBusURL":             DefaultBusURL,
		"DefaultHTTPAddr":           DefaultHTTPAddr,
		"DefaultCalendarReadAgent":  DefaultCalendarReadAgent,
		"DefaultCalendarWriteAgent": DefaultCalendarWriteAgent,
		"DefaultTimeZone":           DefaultTimeZone,
		"CapabilityRequest":         CapabilityRequest,
		"CapabilityMove":            CapabilityMove,
		"CapabilityCancel":          CapabilityCancel,
		"CapabilityEstimate":        CapabilityEstimate,
		"StatusBooked":              StatusBooked,
		"StatusEstimated":           StatusEstimated,
		"ErrorTravelBooking":        ErrorTravelBooking,
		"DefaultOffsiteCategory":    DefaultOffsiteCategory,
	}
	want := map[string]string{
		"DefaultAgentID":            "ucla-tdg-scheduler-agent",
		"DefaultBusURL":             "http://localhost:8080",
		"DefaultHTTPAddr":           ":8245",
		"DefaultCalendarReadAgent":  "ucla-tdg-outlook-calendar-agent",
		"DefaultCalendarWriteAgent": "ucla-tdg-outlook-calendar-write-agent",
		"DefaultTimeZone":           "America/Los_Angeles",
		"CapabilityRequest":         "schedule-request",
		"CapabilityMove":            "schedule-move",
		"CapabilityCancel":          "schedule-cancel",
		"CapabilityEstimate":        "travel-estimate",
		"StatusBooked":              "booked",
		"StatusEstimated":           "estimated",
		"ErrorTravelBooking":        "travel_booking_failed",
		"DefaultOffsiteCategory":    "Yellow category",
	}
	for name, got := range tests {
		if got != want[name] {
			t.Fatalf("%s = %q, want %q", name, got, want[name])
		}
	}
}

func TestDecodeRequestCapturesNestedProhibitedField(t *testing.T) {
	req, err := DecodeRequest(`{"action":"schedule-request","request_id":"r1","event":{"conferenceData":{}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.ProhibitedField(); got != "conferenceData" {
		t.Fatalf("ProhibitedField() = %q, want conferenceData", got)
	}
}

func TestReplyTerminalExcludesTravelEstimate(t *testing.T) {
	if !(Reply{Status: StatusBooked}).Terminal() {
		t.Fatal("booked reply should be terminal")
	}
	if (Reply{Status: StatusEstimated}).Terminal() {
		t.Fatal("estimated reply must not be terminal")
	}
}

func TestErrorAndRefusedRepliesTrimInputs(t *testing.T) {
	errReply := ErrorReply(" r1 ", " invalid_request ", " bad ")
	if errReply.RequestID != "r1" || errReply.ErrorCode != ErrorInvalidRequest || errReply.Message != "bad" {
		t.Fatalf("ErrorReply() = %#v", errReply)
	}

	refused := RefusedReply(" r2 ")
	if refused.Status != StatusRefused || refused.RequestID != "r2" || refused.ErrorCode != ErrorOtherPeople {
		t.Fatalf("RefusedReply() = %#v", refused)
	}
}

func TestTravelEstimateFalseBooleansSerialize(t *testing.T) {
	body, err := json.Marshal(Reply{
		Status:    StatusEstimated,
		RequestID: "r1",
		Estimate:  &EstimateResult{Source: "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"status":"estimated","request_id":"r1","estimate":{"minutes":0,"drive_minutes":0,"walk_minutes":0,"source":"fixture","is_office":false,"is_virtual":false}}` {
		t.Fatalf("estimate JSON = %s", body)
	}
}
