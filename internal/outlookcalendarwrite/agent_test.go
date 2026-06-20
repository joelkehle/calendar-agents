package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type fakeWriteService struct {
	getEvent    func(context.Context, string, string) (StoredEvent, error)
	insertEvent func(context.Context, string, StoredEvent) (StoredEvent, error)
	patchEvent  func(context.Context, string, string, StoredEvent) (StoredEvent, error)
}

func (f fakeWriteService) GetEvent(ctx context.Context, calendarID, eventID string) (StoredEvent, error) {
	return f.getEvent(ctx, calendarID, eventID)
}

func (f fakeWriteService) InsertEvent(ctx context.Context, calendarID string, event StoredEvent) (StoredEvent, error) {
	return f.insertEvent(ctx, calendarID, event)
}

func (f fakeWriteService) PatchEvent(ctx context.Context, calendarID, eventID string, event StoredEvent) (StoredEvent, error) {
	return f.patchEvent(ctx, calendarID, eventID, event)
}

type busRecorder struct {
	t *testing.T

	mu       sync.Mutex
	messages []map[string]any
	server   *httptest.Server
}

func newBusRecorder(t *testing.T) *busRecorder {
	t.Helper()
	r := &busRecorder{t: t}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/v1/acks":
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

func TestAgentDryRunInsertReturnsWouldWriteWithoutMutating(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	mutated := false
	agent := NewAgent(AgentConfig{
		BusURL:  recorder.server.URL,
		AgentID: DefaultAgentID,
		Secret:  "secret",
		DryRun:  true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			mutated = true
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	body := `{"action":"event-insert","calendar_id":"default","event":{"summary":"Meeting Quota Reached","description":"guard day","start":{"date":"2026-05-07","time_zone":"America/Los_Angeles"},"end":{"date":"2026-05-08","time_zone":"America/Los_Angeles"},"show_as":"busy"}}`
	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "calendar-guard",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           body,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if mutated {
		t.Fatal("dry-run insert called mutation service")
	}
	var resp MutationResponse
	if err := json.Unmarshal([]byte(recorder.lastMessageBody(t)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.DryRun || resp.WouldWrite == nil {
		t.Fatalf("response = %#v", resp)
	}
	if resp.WouldWrite.Start.Date != "2026-05-07" || resp.WouldWrite.End.Date != "2026-05-08" {
		t.Fatalf("would_write all-day dates = %#v %#v", resp.WouldWrite.Start, resp.WouldWrite.End)
	}
	if !HasOwnershipMarker(resp.WouldWrite.Description) {
		t.Fatalf("would_write missing marker: %#v", resp.WouldWrite)
	}
}

func TestAgentPatchRefusesEventWithoutOwnershipMarker(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	patched := false
	agent := NewAgent(AgentConfig{
		BusURL:  recorder.server.URL,
		AgentID: DefaultAgentID,
		Secret:  "secret",
		DryRun:  false,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return StoredEvent{
				ID:          "evt-1",
				Summary:     "No more meetings - old",
				Description: "not managed",
				Start:       EventDateTime{DateTime: "2026-05-07T13:00:00-07:00", TimeZone: DefaultTimeZone},
				End:         EventDateTime{DateTime: "2026-05-07T14:00:00-07:00", TimeZone: DefaultTimeZone},
				ShowAs:      "busy",
			}, nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patched = true
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "calendar-guard",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           `{"action":"event-patch","calendar_id":"default","event_id":"evt-1","event":{"summary":"No more meetings - patched"}}`,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patched {
		t.Fatal("patch called mutation service for unowned event")
	}
	var resp MutationResponse
	if err := json.Unmarshal([]byte(recorder.lastMessageBody(t)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.Error, "ownership marker") {
		t.Fatalf("response = %#v", resp)
	}
}
