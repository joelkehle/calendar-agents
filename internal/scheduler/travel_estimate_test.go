package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
)

func estimateBody(requestID, eventStart, location string) string {
	blob, _ := json.Marshal(map[string]any{
		"action":      CapabilityEstimate,
		"request_id":  requestID,
		"event_start": eventStart,
		"location":    location,
	})
	return string(blob)
}

func TestTravelEstimateValidation(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), nil, nil)
	cases := []struct {
		name        string
		body        string
		wantStatus  string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "missing request_id",
			body:        estimateBody("", "2026-06-12T09:00:00-07:00", "200 Medical Plaza"),
			wantStatus:  StatusError,
			wantCode:    ErrorInvalidRequest,
			wantMessage: "request_id",
		},
		{
			name:        "missing location",
			body:        estimateBody("req-e2", "2026-06-12T09:00:00-07:00", ""),
			wantStatus:  StatusError,
			wantCode:    ErrorInvalidRequest,
			wantMessage: "location",
		},
		{
			name:        "blank location",
			body:        estimateBody("req-e3", "2026-06-12T09:00:00-07:00", "   "),
			wantStatus:  StatusError,
			wantCode:    ErrorInvalidRequest,
			wantMessage: "location",
		},
		{
			name:        "location over 200 runes",
			body:        estimateBody("req-e4", "2026-06-12T09:00:00-07:00", strings.Repeat("ü", 201)),
			wantStatus:  StatusError,
			wantCode:    ErrorInvalidRequest,
			wantMessage: "location",
		},
		{
			name:        "non-RFC3339 event_start",
			body:        estimateBody("req-e5", "tomorrow 9am", "200 Medical Plaza"),
			wantStatus:  StatusError,
			wantCode:    ErrorInvalidRequest,
			wantMessage: "event_start",
		},
		{
			name:       "prohibited field refused",
			body:       `{"action":"travel-estimate","request_id":"req-e6","event_start":"2026-06-12T09:00:00-07:00","location":"200 Medical Plaza","attendees":[{"email":"x@example.com"}]}`,
			wantStatus: StatusRefused,
			wantCode:   ErrorOtherPeople,
		},
	}
	for i, tc := range cases {
		h.sendRequest(t, "caller", "conv-est", "msg-est-"+tc.name, tc.body)
		reply := h.waitReply(t, i+1)
		if reply.Status != tc.wantStatus || reply.ErrorCode != tc.wantCode {
			t.Fatalf("%s: reply = %#v", tc.name, reply)
		}
		if tc.wantMessage != "" && !strings.Contains(reply.Message, tc.wantMessage) {
			t.Fatalf("%s: message %q must name the field %q", tc.name, reply.Message, tc.wantMessage)
		}
	}
}

func TestTravelEstimateReply(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), nil, nil)
	agent := h.agent

	t.Run("matrix venue", func(t *testing.T) {
		reply := agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m1",
			EventStart: "2026-06-12T10:00:00-07:00", Location: "200 Medical Plaza",
		})
		if reply.Status != StatusEstimated || reply.Estimate == nil {
			t.Fatalf("reply = %#v", reply)
		}
		est := reply.Estimate
		if est.Minutes != 15 || est.DriveMinutes != 5 || est.WalkMinutes != 10 || est.Source != "matrix" {
			t.Fatalf("estimate = %#v", est)
		}
		if est.Origin == nil || est.Origin.ID != "ucla-tdg-office" {
			t.Fatalf("origin = %#v", est.Origin)
		}
		if est.Venue == nil || est.Venue.ID != "200-medical-plaza" || est.Venue.Parking == "" {
			t.Fatalf("venue = %#v", est.Venue)
		}
		blob, err := json.Marshal(reply)
		if err != nil {
			t.Fatalf("marshal reply: %v", err)
		}
		if !strings.Contains(string(blob), `"is_office":false`) || !strings.Contains(string(blob), `"is_virtual":false`) {
			t.Fatalf("reply JSON must carry explicit false flags: %s", blob)
		}
	})

	t.Run("unknown venue uses default and omits venue", func(t *testing.T) {
		reply := agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m2",
			EventStart: "2026-06-12T10:00:00-07:00", Location: "Cedars-Sinai Medical Center",
		})
		if reply.Status != StatusEstimated || reply.Estimate == nil {
			t.Fatalf("reply = %#v", reply)
		}
		if reply.Estimate.Source != "default" || reply.Estimate.Minutes != 30 || reply.Estimate.Venue != nil {
			t.Fatalf("estimate = %#v", reply.Estimate)
		}
	})

	t.Run("office location", func(t *testing.T) {
		reply := agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m3",
			EventStart: "2026-06-12T10:00:00-07:00", Location: "UCLA TDG office",
		})
		if reply.Status != StatusEstimated || reply.Estimate == nil || !reply.Estimate.IsOffice {
			t.Fatalf("reply = %#v", reply)
		}
		if reply.Estimate.Origin == nil || reply.Estimate.Origin.ID != "alto-cedro" {
			t.Fatalf("office destination must use residence origin: %#v", reply.Estimate.Origin)
		}
	})

	t.Run("virtual location", func(t *testing.T) {
		reply := agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m4",
			EventStart: "2026-06-12T10:00:00-07:00", Location: "Microsoft Teams Meeting",
		})
		if reply.Status != StatusEstimated || reply.Estimate == nil {
			t.Fatalf("reply = %#v", reply)
		}
		est := reply.Estimate
		if !est.IsVirtual || est.Minutes != 0 || est.Origin != nil || est.Venue != nil || est.Source != "virtual" {
			t.Fatalf("estimate = %#v", est)
		}
	})

	t.Run("event start outside all residence windows", func(t *testing.T) {
		// 2026-04-04 is a Saturday before all residence windows: no_origin.
		reply := agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m5",
			EventStart: "2026-04-04T10:00:00-07:00", Location: "200 Medical Plaza",
		})
		if reply.Status != StatusError || reply.ErrorCode != ErrorEstimateUnavailable {
			t.Fatalf("reply = %#v", reply)
		}
	})

	t.Run("knowledge unloaded", func(t *testing.T) {
		degraded := newTravelHarness(t, testNow(), func(cfg *Config) {
			cfg.LocationsPath = "testdata/does-not-exist.json"
		}, nil)
		reply := degraded.agent.estimateReply(Request{
			Action: CapabilityEstimate, RequestID: "req-m6",
			EventStart: "2026-06-12T10:00:00-07:00", Location: "200 Medical Plaza",
		})
		if reply.Status != StatusError || reply.ErrorCode != ErrorEstimateUnavailable {
			t.Fatalf("reply = %#v", reply)
		}
	})

	t.Run("no idempotency cache entry stored", func(t *testing.T) {
		h.sendRequest(t, "caller", "conv-cache", "msg-cache-1", estimateBody("req-m7", "2026-06-12T10:00:00-07:00", "200 Medical Plaza"))
		_ = h.waitReply(t, 1)
		if _, ok := agent.cache.Get(canonicalKey("caller", "req-m7")); ok {
			t.Fatal("travel-estimate must not store a reply-cache entry")
		}
	})
}

// TestEstimateBypassesReplyCache (§2.4 placement): a travel-estimate reusing
// a request_id from an earlier schedule-request gets a fresh estimated reply,
// never the cached booked one.
func TestEstimateBypassesReplyCache(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeSuccessFromRequest(t, msg.Body, "evt-1"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-shared", "tomorrow morning", 60))
	booked := h.waitReply(t, 1)
	if booked.Status != StatusBooked {
		t.Fatalf("first reply = %#v", booked)
	}
	h.sendRequest(t, "caller", "conv-2", "msg-2", estimateBody("req-shared", "2026-06-12T10:00:00-07:00", "200 Medical Plaza"))
	estimated := h.waitReply(t, 2)
	if estimated.Status != StatusEstimated || estimated.Estimate == nil || estimated.EventID != "" {
		t.Fatalf("second reply = %#v, want fresh estimated reply, not the cached booking", estimated)
	}
}
