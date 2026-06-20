package outlookcalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type fakeExtractor struct {
	events []calendarread.Event
	err    error
	query  calendarread.EventsQuery
}

func (f *fakeExtractor) ListEvents(query calendarread.EventsQuery) ([]calendarread.Event, error) {
	f.query = query
	return f.events, f.err
}

type busRecorder struct {
	t *testing.T

	mu       sync.Mutex
	acks     []map[string]any
	messages []map[string]any
	server   *httptest.Server
}

func newBusRecorder(t *testing.T) *busRecorder {
	t.Helper()
	r := &busRecorder{t: t}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/v1/acks":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode ack body: %v", err)
			}
			r.mu.Lock()
			r.acks = append(r.acks, body)
			r.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case req.Method == http.MethodPost && req.URL.Path == "/v1/messages":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode message body: %v", err)
			}
			r.mu.Lock()
			r.messages = append(r.messages, body)
			r.mu.Unlock()
			_, _ = w.Write([]byte(`{"message_id":"m-1"}`))
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *busRecorder) lastMessageBody(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.messages) == 0 {
		t.Fatal("no message captured")
	}
	var raw string
	blob, _ := json.Marshal(r.messages[len(r.messages)-1]["body"])
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("decode message body string: %v", err)
	}
	return raw
}

func TestAgentHandlesEventsList(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	extractor := &fakeExtractor{events: []calendarread.Event{{
		ID:         "evt-1",
		Summary:    "Design review",
		Location:   "Zoom",
		Categories: []string{"UCLA", "Meeting"},
	}}}
	agent := NewAgent(AgentConfig{
		BusURL:  recorder.server.URL,
		AgentID: DefaultAgentID,
		Secret:  "secret",
	}, extractor, telemetry.New("outlook-calendar-test"))

	err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "jk-email-operator",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           `{"action":"events-list","query":{"time_min":"2026-05-05T00:00:00-07:00","time_max":"2026-05-06T00:00:00-07:00"}}`,
	})
	if err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}

	if extractor.query.TimeMin != "2026-05-05T00:00:00-07:00" {
		t.Fatalf("query = %#v", extractor.query)
	}
	var resp calendarread.EventsListResponse
	if err := json.Unmarshal([]byte(recorder.lastMessageBody(t)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].Summary != "Design review" {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Events[0].Location != "Zoom" {
		t.Fatalf("response location = %#v", resp.Events[0])
	}
	if len(resp.Events[0].Categories) != 2 || resp.Events[0].Categories[0] != "UCLA" || resp.Events[0].Categories[1] != "Meeting" {
		t.Fatalf("response categories = %#v", resp.Events[0].Categories)
	}
}

func TestAgentHandlesCalendarList(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	agent := NewAgent(AgentConfig{
		BusURL:  recorder.server.URL,
		AgentID: DefaultAgentID,
		Secret:  "secret",
	}, &fakeExtractor{}, telemetry.New("outlook-calendar-test"))

	err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "jk-email-operator",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           `{"action":"calendar-list"}`,
	})
	if err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	var resp calendarread.CalendarListResponse
	if err := json.Unmarshal([]byte(recorder.lastMessageBody(t)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Calendars) != 1 || !resp.Calendars[0].Primary {
		t.Fatalf("response = %#v", resp)
	}
}
