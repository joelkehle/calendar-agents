package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

func TestAgentHoldRequesterPatchingGuardBlockReturnsNotOwned(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	patched := false
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"ucla-tdg-scheduler-agent"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return guardStoredEvent(), nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patched = true
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "ucla-tdg-scheduler-agent",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           `{"action":"event-patch","calendar_id":"default","event_id":"guard-1","event":{"summary":"Meeting Quota Reached"}}`,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patched {
		t.Fatal("hold requester patched a guard block")
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "not_owned" || !strings.Contains(resp.Error, "own working holds") {
		t.Fatalf("response = %#v, want not_owned", resp)
	}
}

func TestAgentGuardPatchPathUnchangedForGuardAgent(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	patched := false
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"ucla-tdg-scheduler-agent"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return guardStoredEvent(), nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
			patched = true
			return event, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "jk-calendar-guard-agent",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           `{"action":"event-patch","calendar_id":"default","event_id":"guard-1","event":{"summary":"No more meetings - updated"}}`,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if !patched {
		t.Fatal("guard patch path did not call mutation service")
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || resp.Event.Summary != "No more meetings - updated" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentHoldPatchDuplicateRequestIDReturnsCachedResponse(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustHoldStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return existing, nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
			patchCalls++
			event.ID = "evt-1"
			return event, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	body := holdPatchBody(t, "Joel + updated focus", nil)
	evt := busclient.InboxEvent{
		Type:           "request",
		From:           "agent-a",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           body,
		Meta:           map[string]any{"request_id": "patch-1"},
	}
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("first handleEvent() error = %v", err)
	}
	evt.MessageID = "msg-2"
	evt.ConversationID = "conv-2"
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("second handleEvent() error = %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || resp.Event.Summary != "Joel + updated focus" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentHoldCancelDuplicateRequestIDReturnsCachedResponse(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustHoldStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return existing, nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
			patchCalls++
			event.ID = "evt-1"
			return event, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	evt := busclient.InboxEvent{
		Type:           "request",
		From:           "agent-a",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           holdPatchBody(t, CancelledPrefix, ptrString("free")),
		Meta:           map[string]any{"request_id": "cancel-1"},
	}
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("first handleEvent() error = %v", err)
	}
	evt.MessageID = "msg-2"
	evt.ConversationID = "conv-2"
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("second handleEvent() error = %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || !strings.HasPrefix(resp.Event.Summary, CancelledPrefix) || resp.Event.ShowAs != "free" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentHoldDoubleCancelSucceedsWithoutMutation(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	cancelled := mustHoldStoredEvent(t, "agent-a")
	cancelled.Summary = CancelledPrefix + cancelled.Summary
	cancelled.ShowAs = "free"
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			return cancelled, nil
		},
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), busclient.InboxEvent{
		Type:           "request",
		From:           "agent-a",
		MessageID:      "msg-1",
		ConversationID: "conv-1",
		Body:           holdPatchBody(t, CancelledPrefix, ptrString("free")),
		Meta:           map[string]any{"request_id": "cancel-2"},
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || resp.Event.Summary != cancelled.Summary || resp.Event.ShowAs != "free" {
		t.Fatalf("response = %#v", resp)
	}
}

func holdPatchBody(t *testing.T, summary string, showAs *string) string {
	t.Helper()
	req := Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    "evt-1",
		Event:      EventInput{Summary: &summary, ShowAs: showAs},
	}
	blob, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(blob)
}

func guardStoredEvent() StoredEvent {
	return StoredEvent{
		ID:          "guard-1",
		Summary:     "Meeting Quota Reached",
		Description: "guard day\n\n" + ManagedByMarker + "\n" + OwnerAgentMarker,
		Start:       EventDateTime{Date: "2026-06-15", TimeZone: DefaultTimeZone},
		End:         EventDateTime{Date: "2026-06-16", TimeZone: DefaultTimeZone},
		ShowAs:      "busy",
	}
}

func mustHoldStoredEvent(t *testing.T, requester string) StoredEvent {
	t.Helper()
	event, err := BuildHoldInsert(validHoldInput(t, 1, 10, time.Hour), requester)
	if err != nil {
		t.Fatalf("BuildHoldInsert() error = %v", err)
	}
	event.ID = "evt-1"
	return event
}

func ptrString(value string) *string {
	return &value
}
