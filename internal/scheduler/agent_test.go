package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type schedulerHarness struct {
	t *testing.T

	mu       sync.Mutex
	upstream []upstreamMessage
	replies  []Reply
	agent    *Agent
	server   *httptest.Server
	onSend   func(upstreamMessage) (string, bool)
	metrics  *telemetry.Registry
}

type upstreamMessage struct {
	To             string
	ConversationID string
	RequestID      string
	Body           string
	Meta           map[string]any
}

func newSchedulerHarness(t *testing.T, now time.Time, onSend func(upstreamMessage) (string, bool)) *schedulerHarness {
	t.Helper()
	h := &schedulerHarness{t: t, onSend: onSend}
	h.server = httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(h.server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.metrics = telemetry.New("scheduler-test")
	h.agent = NewAgent(Config{
		BusURL:          h.server.URL,
		AgentID:         DefaultAgentID,
		Secret:          "secret",
		UpstreamTimeout: 20 * time.Millisecond,
		Now:             func() time.Time { return now },
	}, h.metrics)
	h.agent.startWorkers(ctx)
	return h
}

func (h *schedulerHarness) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/acks":
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.t.Fatalf("decode message body: %v", err)
		}
		msg := upstreamMessage{
			To:             stringValue(body["to"]),
			ConversationID: stringValue(body["conversation_id"]),
			RequestID:      stringValue(body["request_id"]),
			Body:           stringValue(body["body"]),
			Meta:           mapValue(body["meta"]),
		}
		if msg.To == "caller" || msg.To == "caller-2" {
			var reply Reply
			if err := json.Unmarshal([]byte(msg.Body), &reply); err != nil {
				h.t.Fatalf("decode scheduler reply: %v body=%s", err, msg.Body)
			}
			h.mu.Lock()
			h.replies = append(h.replies, reply)
			h.mu.Unlock()
			_, _ = w.Write([]byte(`{"message_id":"reply-1"}`))
			return
		}
		h.mu.Lock()
		h.upstream = append(h.upstream, msg)
		h.mu.Unlock()
		if h.onSend != nil {
			resp, ok := h.onSend(msg)
			if ok {
				go func() {
					_ = h.agent.handleEvent(context.Background(), busclient.InboxEvent{
						Type:           "response",
						From:           msg.To,
						ConversationID: msg.ConversationID,
						MessageID:      msg.RequestID + "-response",
						Body:           resp,
					})
				}()
			}
		}
		_, _ = w.Write([]byte(`{"message_id":"upstream-1"}`))
	default:
		h.t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}
}

func (h *schedulerHarness) sendRequest(t *testing.T, from, conversationID, messageID, body string) {
	t.Helper()
	if err := h.agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           from,
		MessageID:      messageID,
		ConversationID: conversationID,
		Body:           body,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
}

func (h *schedulerHarness) waitReply(t *testing.T, count int) Reply {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if len(h.replies) >= count {
			reply := h.replies[count-1]
			h.mu.Unlock()
			return reply
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("timed out waiting for reply %d; replies=%#v upstream=%#v", count, h.replies, h.upstream)
	return Reply{}
}

func (h *schedulerHarness) upstreamCount(to string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, msg := range h.upstream {
		if msg.To == to {
			count++
		}
	}
	return count
}

func TestAgentLoopHappyPath(t *testing.T) {
	t.Parallel()

	now := testNow()
	h := newSchedulerHarness(t, now, func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeSuccessFromRequest(t, msg.Body, "evt-1"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-1", "tomorrow morning", 60))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusBooked || reply.EventID != "evt-1" || reply.Start != "2026-06-12T07:00:00-07:00" {
		t.Fatalf("reply = %#v", reply)
	}
	if h.upstreamCount(DefaultCalendarReadAgent) != 1 || h.upstreamCount(DefaultCalendarWriteAgent) != 1 {
		t.Fatalf("upstream counts read=%d write=%d", h.upstreamCount(DefaultCalendarReadAgent), h.upstreamCount(DefaultCalendarWriteAgent))
	}
	if got := metricValue(t, h.metrics, "schedule_calendar_read_dependency_requests"); got != 1 {
		t.Fatalf("read dependency requests = %d, want 1", got)
	}
	if got := metricValue(t, h.metrics, "schedule_calendar_write_dependency_requests"); got != 1 {
		t.Fatalf("write dependency requests = %d, want 1", got)
	}
	if got := metricGaugeValue(t, h.metrics, "schedule_calendar_read_dependency_available"); got != 1 {
		t.Fatalf("read dependency available = %d, want 1", got)
	}
	if got := metricGaugeValue(t, h.metrics, "schedule_calendar_write_dependency_available"); got != 1 {
		t.Fatalf("write dependency available = %d, want 1", got)
	}
}

func TestAgentLoopUpstreamTimeout(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), func(upstreamMessage) (string, bool) {
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-timeout", "tomorrow morning", 60))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusError || reply.ErrorCode != ErrorUpstreamUnavailable {
		t.Fatalf("reply = %#v, want upstream_unavailable", reply)
	}
	if h.upstreamCount(DefaultCalendarReadAgent) != 2 {
		t.Fatalf("read attempts = %d, want 2", h.upstreamCount(DefaultCalendarReadAgent))
	}
}

func TestAgentLoopWriterConflictRetries(t *testing.T) {
	t.Parallel()

	var writeCalls int
	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			writeCalls++
			if writeCalls == 1 {
				return writeError("conflict", "conflict: 2026-06-12T07:00:00-07:00/2026-06-12T08:00:00-07:00"), true
			}
			return writeSuccessFromRequest(t, msg.Body, "evt-2"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-conflict", "tomorrow morning", 60))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusBooked || reply.Start != "2026-06-12T08:15:00-07:00" {
		t.Fatalf("reply = %#v, want retry booked at 08:15", reply)
	}
	if writeCalls != 2 {
		t.Fatalf("writeCalls = %d, want 2", writeCalls)
	}
}

func TestAgentLoopWriterRefusalPassthrough(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeError("not_allowlisted", "requesting agent is not allowlisted"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", scheduleRequestBody("req-refused", "tomorrow morning", 60))
	reply := h.waitReply(t, 1)
	if reply.Status != StatusError || reply.ErrorCode != ErrorBookingRefused || !strings.Contains(reply.Message, "writer: not_allowlisted") {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestAgentLoopMove(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeSuccessFromRequest(t, msg.Body, "evt-move"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", `{"action":"schedule-move","request_id":"req-move","event_id":"evt-move","window":"tomorrow evening","duration_minutes":60}`)
	reply := h.waitReply(t, 1)
	if reply.Status != StatusMoved || reply.EventID != "evt-move" || reply.Start != "2026-06-12T17:00:00-07:00" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestAgentLoopCancelAndCancelTwice(t *testing.T) {
	t.Parallel()

	var writeCalls int
	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarWriteAgent {
			writeCalls++
			return writeSuccessFromRequest(t, msg.Body, "evt-cancel"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", `{"action":"schedule-cancel","request_id":"req-cancel-1","event_id":"evt-cancel"}`)
	first := h.waitReply(t, 1)
	h.sendRequest(t, "caller", "conv-2", "msg-2", `{"action":"schedule-cancel","request_id":"req-cancel-2","event_id":"evt-cancel"}`)
	second := h.waitReply(t, 2)
	if first.Status != StatusCancelled || second.Status != StatusCancelled || writeCalls != 2 {
		t.Fatalf("first=%#v second=%#v writeCalls=%d", first, second, writeCalls)
	}
	if h.upstreamCount(DefaultCalendarReadAgent) != 0 {
		t.Fatalf("cancel made read calls = %d", h.upstreamCount(DefaultCalendarReadAgent))
	}
}

func TestAgentLoopNotOwnedMapsThrough(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeError(ErrorNotOwned, "hold requesters may only patch their own working holds"), true
		}
		return "", false
	})
	h.sendRequest(t, "caller", "conv-1", "msg-1", `{"action":"schedule-move","request_id":"req-not-owned","event_id":"evt-guard","window":"tomorrow evening"}`)
	reply := h.waitReply(t, 1)
	if reply.Status != StatusError || reply.ErrorCode != ErrorNotOwned {
		t.Fatalf("reply = %#v, want not_owned", reply)
	}
}

func TestAgentLoopDuplicateRequestReplaysCachedReply(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), func(msg upstreamMessage) (string, bool) {
		if msg.To == DefaultCalendarReadAgent {
			return calendarResponse(nil), true
		}
		if msg.To == DefaultCalendarWriteAgent {
			return writeSuccessFromRequest(t, msg.Body, "evt-dup"), true
		}
		return "", false
	})
	body := scheduleRequestBody("req-dup", "tomorrow morning", 60)
	h.sendRequest(t, "caller", "conv-1", "msg-1", body)
	first := h.waitReply(t, 1)
	h.sendRequest(t, "caller", "conv-2", "msg-2", body)
	second := h.waitReply(t, 2)
	if first.Status != StatusBooked || second.Status != StatusBooked || second.EventID != first.EventID {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if h.upstreamCount(DefaultCalendarWriteAgent) != 1 {
		t.Fatalf("write calls = %d, want cached replay with 1", h.upstreamCount(DefaultCalendarWriteAgent))
	}
}

func TestAgentLoopAuthorityRefusal(t *testing.T) {
	t.Parallel()

	h := newSchedulerHarness(t, testNow(), nil)
	body := `{"action":"schedule-request","request_id":"req-auth","purpose":"focus","duration_minutes":60,"window":"tomorrow","agenda":"Agenda","event":{"attendees":[{"email":"x@example.com"}]}}`
	h.sendRequest(t, "caller", "conv-1", "msg-1", body)
	reply := h.waitReply(t, 1)
	if reply.Status != StatusRefused || reply.ErrorCode != ErrorOtherPeople || reply.Message != "escalate to Joel" {
		t.Fatalf("reply = %#v", reply)
	}
}

func scheduleRequestBody(requestID, window string, duration int) string {
	blob, _ := json.Marshal(Request{
		Action:          CapabilityRequest,
		RequestID:       requestID,
		Purpose:         "Apple Health pipeline working session",
		RequesterLabel:  "Fable",
		DurationMinutes: duration,
		Window:          window,
		Agenda:          "1. Parse export baseline\n2. Decide next step",
	})
	return string(blob)
}

func calendarResponse(events []calendarread.Event) string {
	blob, _ := json.Marshal(calendarread.EventsListResponse{Events: events})
	return string(blob)
}

func writeSuccessFromRequest(t *testing.T, body, eventID string) string {
	t.Helper()
	var req outlookcalendarwrite.Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode write request: %v", err)
	}
	summary := ""
	if req.Event.Summary != nil {
		summary = *req.Event.Summary
	}
	if summary == "" {
		summary = "Joel + Fable: moved"
	}
	event := outlookcalendarwrite.StoredEvent{
		ID:      eventID,
		Summary: summary,
		Location: func() string {
			if req.Event.Location != nil {
				return strings.TrimSpace(*req.Event.Location)
			}
			return ""
		}(),
		Start:  derefDateTime(req.Event.Start),
		End:    derefDateTime(req.Event.End),
		ShowAs: "busy",
	}
	if req.Event.ShowAs != nil {
		event.ShowAs = *req.Event.ShowAs
	}
	blob, _ := json.Marshal(outlookcalendarwrite.MutationResponse{Event: &event})
	return string(blob)
}

func writeError(code, message string) string {
	blob, _ := json.Marshal(outlookcalendarwrite.MutationResponse{ErrorCode: code, Error: message})
	return string(blob)
}

func derefDateTime(value *outlookcalendarwrite.EventDateTime) outlookcalendarwrite.EventDateTime {
	if value == nil {
		return outlookcalendarwrite.EventDateTime{}
	}
	return *value
}

func testNow() time.Time {
	return time.Date(2026, 6, 11, 16, 0, 0, 0, loadLocation())
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(value.(string))
}

func mapValue(value any) map[string]any {
	if value == nil {
		return nil
	}
	out, _ := value.(map[string]any)
	return out
}
