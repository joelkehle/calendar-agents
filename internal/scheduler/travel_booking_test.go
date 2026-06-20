package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

func TestRequestLocationValidation(t *testing.T) {
	t.Parallel()

	t.Run("absent location follows the unchanged path", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true
			}
			return writeSuccessFromRequest(t, msg.Body, "evt-1"), true
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-plain", "tomorrow morning", 60))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusBooked || reply.Travel != nil {
			t.Fatalf("reply = %#v, want booked with no travel field", reply)
		}
		if writes := len(h.writeMessages()); writes != 1 {
			t.Fatalf("write calls = %d, want 1 (hold only)", writes)
		}
	})

	for _, tc := range []struct {
		name     string
		location string
	}{
		{name: "office alias yields no travel", location: "UCLA TDG office"},
		{name: "virtual location yields no travel", location: "Microsoft Teams Meeting"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
				if msg.To == DefaultCalendarReadAgent {
					return calendarResponse(nil), true
				}
				return writeSuccessFromRequest(t, msg.Body, "evt-1"), true
			})
			h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-onsite", "tomorrow morning", 60, tc.location))
			reply := h.waitReply(t, 1)
			if reply.Status != StatusBooked || reply.Travel != nil {
				t.Fatalf("reply = %#v, want booked with no travel field", reply)
			}
			if writes := len(h.writeMessages()); writes != 1 {
				t.Fatalf("write calls = %d, want 1 (hold only)", writes)
			}
		})
	}

	t.Run("location over 200 runes is invalid_request", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, nil)
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-long", "tomorrow morning", 60, strings.Repeat("x", 201)))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusError || reply.ErrorCode != ErrorInvalidRequest || !strings.Contains(reply.Message, "location") {
			t.Fatalf("reply = %#v", reply)
		}
		if writes := len(h.writeMessages()); writes != 0 {
			t.Fatalf("write calls = %d, want 0", writes)
		}
	})
}

func TestOffsiteBookingHappyPath(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		return echoWriteSuccess(t)(msg)
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-offsite", "tomorrow morning", 60, "200 Medical Plaza"))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusBooked || reply.EventID != "evt-hold" || reply.Start != "2026-06-12T07:00:00-07:00" {
		t.Fatalf("reply = %#v", reply)
	}

	writes := h.writeMessages()
	if len(writes) != 3 {
		t.Fatalf("write calls = %d, want hold + before + after", len(writes))
	}
	keys := map[string]bool{}
	for _, msg := range writes {
		keys[metaRequestID(msg)] = true
	}
	if len(keys) != 3 {
		t.Fatalf("meta request_ids must be distinct: %#v", keys)
	}
	holdReq := decodeWriteRequest(t, writes[0])
	beforeReq := decodeWriteRequest(t, writes[1])
	afterReq := decodeWriteRequest(t, writes[2])
	if !strings.HasSuffix(metaRequestID(writes[0]), "-insert") ||
		!strings.HasSuffix(metaRequestID(writes[1]), "-travel-before") ||
		!strings.HasSuffix(metaRequestID(writes[2]), "-travel-after") {
		t.Fatalf("meta keys = %q %q %q", metaRequestID(writes[0]), metaRequestID(writes[1]), metaRequestID(writes[2]))
	}
	if !strings.HasPrefix(*holdReq.Event.Summary, "Joel + Fable: ") {
		t.Fatalf("hold summary = %q", *holdReq.Event.Summary)
	}
	if *beforeReq.Event.Summary != "Travel: 200 Medical Plaza (for 07:00)" {
		t.Fatalf("before summary = %q", *beforeReq.Event.Summary)
	}
	if *afterReq.Event.Summary != "Travel: 200 Medical Plaza (return 08:00)" {
		t.Fatalf("after summary = %q", *afterReq.Event.Summary)
	}
	if beforeReq.Event.Location == nil || *beforeReq.Event.Location != "200 Medical Plaza, Los Angeles, CA 90024" {
		t.Fatalf("before travel location = %#v", beforeReq.Event.Location)
	}
	if afterReq.Event.Location == nil || *afterReq.Event.Location != "Monte & Jacqueline's (housesitting), 9121 Alto Cedro Drive, Beverly Hills, CA 90210" {
		t.Fatalf("after travel location = %#v", afterReq.Event.Location)
	}
	if !strings.HasPrefix(*beforeReq.Event.Description, "travel_for=evt-hold\nparent_start=2026-06-12T07:00:00-07:00\nDestination: 200 Medical Plaza, Los Angeles, CA 90024\nParking: ") {
		t.Fatalf("before travel description = %q", *beforeReq.Event.Description)
	}
	if !strings.HasPrefix(*afterReq.Event.Description, "travel_for=evt-hold\nparent_start=2026-06-12T07:00:00-07:00\nDestination: Monte & Jacqueline's (housesitting), 9121 Alto Cedro Drive, Beverly Hills, CA 90210\nParking: ") {
		t.Fatalf("after travel description = %q", *afterReq.Event.Description)
	}
	for _, req := range []outlookcalendarwrite.Request{beforeReq, afterReq} {
		if *req.Event.ShowAs != "busy" {
			t.Fatalf("travel show_as = %q", *req.Event.ShowAs)
		}
	}
	if beforeReq.Event.Start.DateTime != "2026-06-12T06:30:00-07:00" || beforeReq.Event.End.DateTime != "2026-06-12T07:00:00-07:00" {
		t.Fatalf("before block = %s/%s", beforeReq.Event.Start.DateTime, beforeReq.Event.End.DateTime)
	}
	if afterReq.Event.Start.DateTime != "2026-06-12T08:00:00-07:00" || afterReq.Event.End.DateTime != "2026-06-12T08:30:00-07:00" {
		t.Fatalf("after block = %s/%s", afterReq.Event.Start.DateTime, afterReq.Event.End.DateTime)
	}

	travel := reply.Travel
	if travel == nil || travel.Before == nil || travel.After == nil {
		t.Fatalf("travel = %#v, want BOTH before and after", travel)
	}
	if travel.Minutes != 30 || travel.OriginID != "alto-cedro" || travel.EstimateSource != "matrix" {
		t.Fatalf("travel = %#v", travel)
	}
	if travel.Before.EventID != "evt-before" || travel.After.EventID != "evt-after" {
		t.Fatalf("travel legs = %#v %#v", travel.Before, travel.After)
	}
	if travel.Notes == nil || len(travel.Notes) != 0 {
		t.Fatalf("travel notes must be [] in v1: %#v", travel.Notes)
	}
	blob, _ := json.Marshal(reply)
	if !strings.Contains(string(blob), `"notes":[]`) {
		t.Fatalf("reply JSON must serialize notes as []: %s", blob)
	}
}

func TestOffsiteBookingCompensation(t *testing.T) {
	t.Parallel()

	t.Run("before insert conflict cancels the hold", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.Contains(*req.Event.Summary, "(for ") {
				return writeError("conflict", "conflict: busy"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-comp1", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusError || reply.ErrorCode != ErrorTravelBooking ||
			!strings.Contains(reply.Message, "travel-before") || !strings.Contains(reply.Message, "conflict") {
			t.Fatalf("reply = %#v", reply)
		}
		writes := h.writeMessages()
		if len(writes) != 3 { // hold insert, failed before insert, hold cancel
			t.Fatalf("write calls = %d: %#v", len(writes), writes)
		}
		cancel := decodeWriteRequest(t, writes[2])
		if cancel.Action != "event-patch" || cancel.EventID != "evt-hold" ||
			*cancel.Event.Summary != outlookcalendarwrite.CancelledPrefix || *cancel.Event.ShowAs != "free" {
			t.Fatalf("compensation patch = %#v", cancel)
		}
		if !strings.HasSuffix(metaRequestID(writes[2]), "-insert-cancel") {
			t.Fatalf("compensation key = %q", metaRequestID(writes[2]))
		}
	})

	t.Run("after insert failure cancels before block and hold", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.Contains(*req.Event.Summary, "(return ") {
				return writeError("rate_limited", "travel-block insert rate limit exceeded"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-comp2", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusError || reply.ErrorCode != ErrorTravelBooking || !strings.Contains(reply.Message, "travel-after") {
			t.Fatalf("reply = %#v", reply)
		}
		writes := h.writeMessages()
		// hold, before, failed after, before-cancel, hold-cancel
		if len(writes) != 5 {
			t.Fatalf("write calls = %d: %#v", len(writes), writes)
		}
		if !strings.HasSuffix(metaRequestID(writes[3]), "-travel-before-cancel") ||
			!strings.HasSuffix(metaRequestID(writes[4]), "-insert-cancel") {
			t.Fatalf("compensation keys = %q %q", metaRequestID(writes[3]), metaRequestID(writes[4]))
		}
		if decodeWriteRequest(t, writes[3]).EventID != "evt-before" || decodeWriteRequest(t, writes[4]).EventID != "evt-hold" {
			t.Fatalf("compensation targets = %#v", writes[3:])
		}
	})

	t.Run("compensation failure still yields a terminal reply", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.Contains(*req.Event.Summary, "(for ") {
				return writeError("conflict", "conflict: busy"), true
			}
			if req.Action == "event-patch" {
				return writeError("", "patch failed"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-comp3", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusError || reply.ErrorCode != ErrorTravelBooking || !strings.Contains(reply.Message, "orphaned") {
			t.Fatalf("reply = %#v", reply)
		}
		if got := metricValue(t, h.metrics, "schedule_travel_compensation_failed"); got != 1 {
			t.Fatalf("schedule_travel_compensation_failed = %d, want 1", got)
		}
	})
}

func replayedWriteSuccess(t *testing.T, body, eventID string) string {
	t.Helper()
	var resp outlookcalendarwrite.MutationResponse
	if err := json.Unmarshal([]byte(writeSuccessFromRequest(t, body, eventID)), &resp); err != nil {
		t.Fatalf("decode write success: %v", err)
	}
	resp.Replayed = true
	blob, _ := json.Marshal(resp)
	return string(blob)
}

func TestOffsiteBookingReplayGuard(t *testing.T) {
	t.Parallel()

	holdEvent := wevt("hold", "Joel + Fable: Pick up Marc after procedure",
		latStatic("2026-06-12T07:00:00"), latStatic("2026-06-12T08:00:00"))

	t.Run("replayed hold verified live proceeds", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				if strings.Contains(msg.RequestID, "verify-hold") {
					return calendarResponse([]calendarread.Event{holdEvent}), true
				}
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.HasPrefix(*req.Event.Summary, "Joel + ") {
				return replayedWriteSuccess(t, msg.Body, "evt-hold"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-rg1", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusBooked || reply.Travel == nil {
			t.Fatalf("reply = %#v", reply)
		}
		for _, msg := range h.writeMessages() {
			if strings.HasSuffix(metaRequestID(msg), "-insert-r2") {
				t.Fatalf("verified-live replay must not re-insert: %#v", msg)
			}
		}
	})

	t.Run("replayed hold missing re-inserts under the alternate key", func(t *testing.T) {
		t.Parallel()
		firstHoldInsert := true
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true // verify finds nothing
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.HasPrefix(*req.Event.Summary, "Joel + ") {
				if firstHoldInsert {
					firstHoldInsert = false
					return replayedWriteSuccess(t, msg.Body, "evt-hold-stale"), true
				}
				return writeSuccessFromRequest(t, msg.Body, "evt-hold"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-rg2", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusBooked || reply.EventID != "evt-hold" {
			t.Fatalf("reply = %#v", reply)
		}
		var r2 bool
		for _, msg := range h.writeMessages() {
			if strings.HasSuffix(metaRequestID(msg), "-insert-r2") {
				r2 = true
			}
		}
		if !r2 {
			t.Fatal("expected a re-insert under the -insert-r2 key")
		}
		if got := metricValue(t, h.metrics, "schedule_travel_replay_reinserts"); got != 1 {
			t.Fatalf("schedule_travel_replay_reinserts = %d, want 1", got)
		}
	})

	t.Run("second replay-and-missing fails the booking", func(t *testing.T) {
		t.Parallel()
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.HasPrefix(*req.Event.Summary, "Joel + ") {
				return replayedWriteSuccess(t, msg.Body, "evt-hold-stale"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-rg3", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusError || reply.ErrorCode != ErrorTravelBooking {
			t.Fatalf("reply = %#v, want travel_booking_failed (no further key generations)", reply)
		}
	})
}

func TestOffsiteBookingIdempotentReplay(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		return echoWriteSuccess(t)(msg)
	})
	body := offsiteRequestBody("req-dup-offsite", "tomorrow morning", 60, "200 Medical Plaza")
	h.sendRequest(t, "caller", "conv-1", "msg-1", body)
	first := h.waitReply(t, 1)
	if first.Status != StatusBooked || first.Travel == nil {
		t.Fatalf("first = %#v", first)
	}
	upstreamBefore := len(h.writeMessages())
	h.sendRequest(t, "caller", "conv-2", "msg-2", body)
	second := h.waitReply(t, 2)
	if second.Status != StatusBooked || second.Travel == nil || second.EventID != first.EventID {
		t.Fatalf("second = %#v", second)
	}
	if len(h.writeMessages()) != upstreamBefore {
		t.Fatalf("duplicate request made %d new write calls", len(h.writeMessages())-upstreamBefore)
	}
}

func TestKnowledgeUnloadedBooking(t *testing.T) {
	t.Parallel()

	h := newTravelHarness(t, testNow(), func(cfg *Config) {
		cfg.LocationsPath = "testdata/does-not-exist.json"
	}, nil)
	h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-degraded", "tomorrow morning", 60, "200 Medical Plaza"))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusError || reply.ErrorCode != ErrorEstimateUnavailable {
		t.Fatalf("reply = %#v", reply)
	}
	if writes := len(h.writeMessages()); writes != 0 {
		t.Fatalf("write calls = %d, want 0", writes)
	}
}

func TestNoOriginBooking(t *testing.T) {
	t.Parallel()

	// 2026-07-04 is a Saturday after the fixture's only residence window
	// closed: every candidate departure has no origin.
	h := newTravelHarness(t, testNow(), func(cfg *Config) {
		cfg.LocationsPath = "testdata/locations_closed.json"
	}, func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		t.Errorf("unexpected write during no-origin booking: %#v", msg)
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-no-origin", "2026-07-04", 60, "200 Medical Plaza"))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusError || reply.ErrorCode != ErrorEstimateUnavailable {
		t.Fatalf("reply = %#v", reply)
	}
	if writes := len(h.writeMessages()); writes != 0 {
		t.Fatalf("write calls = %d, want 0", writes)
	}
}

// latStatic parses without a *testing.T for use in package-level fixtures.
func latStatic(value string) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, loadLocation())
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestOffsiteBookingTravelStepReplayGuard(t *testing.T) {
	t.Parallel()

	t.Run("replayed travel-before missing re-inserts under the alternate key", func(t *testing.T) {
		t.Parallel()
		firstTravelInsert := true
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				return calendarResponse(nil), true // verify finds nothing live
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.HasPrefix(*req.Event.Summary, outlookcalendarwrite.TravelSummaryPrefix) {
				if firstTravelInsert {
					firstTravelInsert = false
					return replayedWriteSuccess(t, msg.Body, "evt-travel-stale"), true
				}
				return echoWriteSuccess(t)(msg)
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-trg1", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusBooked || reply.Travel == nil || reply.Travel.Before == nil {
			t.Fatalf("reply = %#v", reply)
		}
		var r2 bool
		for _, msg := range h.writeMessages() {
			if strings.HasSuffix(metaRequestID(msg), "-travel-before-r2") {
				r2 = true
			}
		}
		if !r2 {
			t.Fatal("expected a re-insert under the -travel-before-r2 key")
		}
		if got := metricValue(t, h.metrics, "schedule_travel_replay_reinserts"); got != 1 {
			t.Fatalf("schedule_travel_replay_reinserts = %d, want 1", got)
		}
	})

	t.Run("replayed travel-after verified live proceeds without re-insert", func(t *testing.T) {
		t.Parallel()
		var afterSummary string
		var afterStart, afterEnd string
		h := newTravelHarness(t, testNow(), nil, func(msg upstreamMessage) (string, bool) {
			if msg.To == DefaultCalendarReadAgent {
				if strings.Contains(msg.RequestID, "verify-hold") && afterSummary != "" {
					return calendarResponse([]calendarread.Event{wevt("evt-travel-after", afterSummary, latStatic(afterStart), latStatic(afterEnd))}), true
				}
				return calendarResponse(nil), true
			}
			req := decodeWriteRequest(t, msg)
			if req.Action == "event-insert" && req.Event.Summary != nil && strings.HasPrefix(*req.Event.Summary, outlookcalendarwrite.TravelSummaryPrefix) && strings.Contains(*req.Event.Summary, "(return ") {
				afterSummary = *req.Event.Summary
				afterStart = strings.TrimSuffix((*req.Event.Start).DateTime, "-07:00")
				afterEnd = strings.TrimSuffix((*req.Event.End).DateTime, "-07:00")
				return replayedWriteSuccess(t, msg.Body, "evt-travel-after"), true
			}
			return echoWriteSuccess(t)(msg)
		})
		h.sendRequest(t, "caller", "conv-1", "msg-1", offsiteRequestBody("req-trg2", "tomorrow morning", 60, "200 Medical Plaza"))
		reply := h.waitReply(t, 1)
		if reply.Status != StatusBooked || reply.Travel == nil || reply.Travel.After == nil {
			t.Fatalf("reply = %#v", reply)
		}
		for _, msg := range h.writeMessages() {
			if strings.HasSuffix(metaRequestID(msg), "-travel-after-r2") {
				t.Fatalf("verified-live travel replay must not re-insert: %#v", msg)
			}
		}
	})
}
