package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

func TestAgentGuardInsertSucceedsWithHoldRequestersUnset(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	agent := NewAgent(AgentConfig{
		BusURL:  recorder.server.URL,
		AgentID: DefaultAgentID,
		Secret:  "secret",
		DryRun:  true,
	}, noopWriteService(), telemetry.New("outlook-calendar-write-test"))

	body := `{"action":"event-insert","calendar_id":"default","event":{"summary":"Meeting Quota Reached","description":"guard day","start":{"date":"2026-05-07","time_zone":"America/Los_Angeles"},"end":{"date":"2026-05-08","time_zone":"America/Los_Angeles"},"show_as":"busy"}}`
	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "calendar-guard",
		MessageID:      "msg-guard",
		ConversationID: "conv-guard",
		Body:           body,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || !resp.DryRun || resp.WouldWrite == nil {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentHoldInsertRequiresAllowlistedRequester(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         true,
		HoldTimeZoneOK: true,
	}, noopWriteService(), telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), holdInboxEvent(t, "agent-a", "msg-1", holdInsertBody(t, 1, 10, time.Hour))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "not_allowlisted" {
		t.Fatalf("response = %#v, want not_allowlisted", resp)
	}
}

func TestParseHoldRequestersTrimsAndDeduplicates(t *testing.T) {
	t.Parallel()

	got := ParseHoldRequesters(" agent-a,agent-b, agent-a ,, ")
	want := []string{"agent-a", "agent-b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ParseHoldRequesters() = %#v, want %#v", got, want)
	}
}

func TestAgentHoldInsertDuplicateMessageReturnsCachedResponse(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	insertCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(_ context.Context, _ string, event StoredEvent) (StoredEvent, error) {
			insertCalls++
			event.ID = "evt-1"
			return event, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	evt := holdInboxEvent(t, "agent-a", "msg-1", holdInsertBody(t, 1, 10, time.Hour))
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("first handleEvent() error = %v", err)
	}
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("second handleEvent() error = %v", err)
	}
	if insertCalls != 1 {
		t.Fatalf("insertCalls = %d, want 1", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || resp.Event.ID != "evt-1" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentHoldInsertRefusesThirdSameDatePerRequester(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	insertCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(_ context.Context, _ string, event StoredEvent) (StoredEvent, error) {
			insertCalls++
			event.ID = fmt.Sprintf("evt-%d", insertCalls)
			return event, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	for i, hour := range []int{9, 11, 13} {
		if err := agent.handleEvent(context.Background(), holdInboxEvent(t, "agent-a", fmt.Sprintf("msg-%d", i), holdInsertBody(t, 1, hour, time.Hour))); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
	}
	if insertCalls != 2 {
		t.Fatalf("insertCalls = %d, want 2", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "rate_limited" {
		t.Fatalf("response = %#v, want rate_limited", resp)
	}
}

func TestAgentHoldInsertRefusesSixthSameDateGlobally(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	requesters := []string{"agent-a", "agent-b", "agent-c", "agent-d", "agent-e", "agent-f"}
	insertCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: requesters,
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(_ context.Context, _ string, event StoredEvent) (StoredEvent, error) {
			insertCalls++
			event.ID = fmt.Sprintf("evt-%d", insertCalls)
			return event, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	for i, requester := range requesters {
		if err := agent.handleEvent(context.Background(), holdInboxEvent(t, requester, fmt.Sprintf("msg-%d", i), holdInsertBody(t, 1, 8+i, 30*time.Minute))); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
	}
	if insertCalls != 5 {
		t.Fatalf("insertCalls = %d, want 5", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "rate_limited" {
		t.Fatalf("response = %#v, want rate_limited", resp)
	}
}

func TestAgentHoldInsertReturnsConflictCode(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, errors.New("conflict: 2026-06-15T10:00:00-07:00/2026-06-15T11:00:00-07:00")
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), holdInboxEvent(t, "agent-a", "msg-1", holdInsertBody(t, 1, 10, time.Hour))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "conflict" {
		t.Fatalf("response = %#v, want conflict", resp)
	}
}

func holdInsertBody(t *testing.T, days int, hour int, duration time.Duration) string {
	t.Helper()
	blob, err := json.Marshal(Request{
		Action:     "event-insert",
		CalendarID: "default",
		Event:      validHoldInput(t, days, hour, duration),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(blob)
}

func holdInboxEvent(t *testing.T, from, messageID, body string) busclient.InboxEvent {
	t.Helper()
	return busclient.InboxEvent{
		Type:           "request",
		From:           from,
		MessageID:      messageID,
		ConversationID: "conv-hold",
		Body:           body,
	}
}

func decodeMutationResponse(t *testing.T, recorder *busRecorder) MutationResponse {
	t.Helper()
	var resp MutationResponse
	if err := json.Unmarshal([]byte(recorder.lastMessageBody(t)), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func noopWriteService() fakeWriteService {
	return fakeWriteService{
		getEvent:    func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent:  func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}
}
