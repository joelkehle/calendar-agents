package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type fakeSnapshotWriteService struct {
	fakeWriteService
	patchEventExpecting func(context.Context, string, string, EventDateTime, EventDateTime, StoredEvent) (StoredEvent, error)
}

func (f fakeSnapshotWriteService) PatchEventExpecting(ctx context.Context, calendarID, eventID string, expectedStart, expectedEnd EventDateTime, event StoredEvent) (StoredEvent, error) {
	return f.patchEventExpecting(ctx, calendarID, eventID, expectedStart, expectedEnd, event)
}

func TestAgentTravelInsertRequiresAllowlistedRequester(t *testing.T) {
	t.Parallel()

	for name, requesters := range map[string][]string{
		"allowlist unset": nil,
		"non-member":      {"agent-b"},
	} {
		requesters := requesters
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recorder := newBusRecorder(t)
			serviceCalled := false
			agent := NewAgent(AgentConfig{
				BusURL:         recorder.server.URL,
				AgentID:        DefaultAgentID,
				Secret:         "secret",
				DryRun:         false,
				HoldRequesters: requesters,
				HoldTimeZoneOK: true,
			}, fakeWriteService{
				getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
				insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
					serviceCalled = true
					return StoredEvent{}, nil
				},
				patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
			}, telemetry.New("outlook-calendar-write-test"))

			if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelInsertBody(t, 1, 10, 30*time.Minute))); err != nil {
				t.Fatalf("handleEvent() error = %v", err)
			}
			if serviceCalled {
				t.Fatal("non-allowlisted travel insert called mutation service")
			}
			resp := decodeMutationResponse(t, recorder)
			if resp.ErrorCode != "not_allowlisted" {
				t.Fatalf("response = %#v, want not_allowlisted", resp)
			}
		})
	}
}

func TestAgentTravelInsertRefusedOnTimeZoneMismatch(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: false,
	}, noopWriteService(), telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelInsertBody(t, 1, 10, 30*time.Minute))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "tz_mismatch" {
		t.Fatalf("response = %#v, want tz_mismatch", resp)
	}
}

func TestAgentTravelInsertDuplicateRequestReturnsCachedResponse(t *testing.T) {
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

	evt := travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelInsertBody(t, 1, 10, 30*time.Minute))
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("first handleEvent() error = %v", err)
	}
	evt.MessageID = "msg-2"
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

func TestAgentTravelIdempotencyKeyClassQualified(t *testing.T) {
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

	holdEvt := holdInboxEvent(t, "agent-a", "msg-1", holdInsertBody(t, 1, 10, time.Hour))
	holdEvt.Meta = map[string]any{"request_id": "shared-id"}
	if err := agent.handleEvent(context.Background(), holdEvt); err != nil {
		t.Fatalf("hold handleEvent() error = %v", err)
	}
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-2", "shared-id", travelInsertBody(t, 1, 12, 30*time.Minute))); err != nil {
		t.Fatalf("travel handleEvent() error = %v", err)
	}
	if insertCalls != 2 {
		t.Fatalf("insertCalls = %d, want 2 (travel insert must not replay the hold cache entry)", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || resp.Event == nil || !strings.HasPrefix(resp.Event.Summary, TravelSummaryPrefix) {
		t.Fatalf("response = %#v, want travel event", resp)
	}
}

func TestAgentTravelInsertRefusesNinthSameDatePerRequester(t *testing.T) {
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

	for i := 0; i < 9; i++ {
		evt := travelInboxEvent(t, "agent-a", fmt.Sprintf("msg-%d", i), fmt.Sprintf("req-%d", i), travelInsertBody(t, 1, 10, 30*time.Minute))
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
	}
	if insertCalls != 8 {
		t.Fatalf("insertCalls = %d, want 8", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "rate_limited" {
		t.Fatalf("response = %#v, want rate_limited", resp)
	}
}

func TestAgentTravelInsertRefusesTwentyFirstSameDateGlobally(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	insertCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a", "agent-b", "agent-c"},
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

	// Per-requester cap is 8, so reaching the global 20 needs >= 3 requesters
	// (8 + 8 + 4 = 20); the 21st insert comes from agent-c, which is under
	// its own per-requester cap.
	sends := append(append(
		repeatRequester("agent-a", 8),
		repeatRequester("agent-b", 8)...),
		repeatRequester("agent-c", 5)...)
	for i, requester := range sends {
		evt := travelInboxEvent(t, requester, fmt.Sprintf("msg-%d", i), fmt.Sprintf("req-%d", i), travelInsertBody(t, 1, 10, 30*time.Minute))
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
	}
	if insertCalls != 20 {
		t.Fatalf("insertCalls = %d, want 20", insertCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "rate_limited" {
		t.Fatalf("response = %#v, want rate_limited", resp)
	}
}

func TestAgentTravelQuotaIndependentOfHoldQuota(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	insertCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-r1", "agent-r2"},
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

	seq := 0
	sendHold := func(from string) MutationResponse {
		t.Helper()
		seq++
		evt := holdInboxEvent(t, from, fmt.Sprintf("msg-%d", seq), holdInsertBody(t, 1, 10, time.Hour))
		evt.Meta = map[string]any{"request_id": fmt.Sprintf("req-%d", seq)}
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("hold handleEvent(%d) error = %v", seq, err)
		}
		return decodeMutationResponse(t, recorder)
	}
	sendTravel := func(from string) MutationResponse {
		t.Helper()
		seq++
		evt := travelInboxEvent(t, from, fmt.Sprintf("msg-%d", seq), fmt.Sprintf("req-%d", seq), travelInsertBody(t, 1, 10, 30*time.Minute))
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("travel handleEvent(%d) error = %v", seq, err)
		}
		return decodeMutationResponse(t, recorder)
	}

	// R1: 2 holds succeed, 3rd hold rate_limited.
	for i := 0; i < 2; i++ {
		if resp := sendHold("agent-r1"); resp.Error != "" {
			t.Fatalf("hold %d response = %#v, want success", i+1, resp)
		}
	}
	if resp := sendHold("agent-r1"); resp.ErrorCode != "rate_limited" {
		t.Fatalf("3rd hold response = %#v, want rate_limited", resp)
	}
	// Hold exhaustion must not bleed into travel: R1 travel 1-8 succeed.
	for i := 0; i < 8; i++ {
		if resp := sendTravel("agent-r1"); resp.Error != "" {
			t.Fatalf("R1 travel %d response = %#v, want success", i+1, resp)
		}
	}
	if resp := sendTravel("agent-r1"); resp.ErrorCode != "rate_limited" {
		t.Fatalf("R1 9th travel response = %#v, want rate_limited", resp)
	}
	// R2: 8 travels succeed, 9th rate_limited.
	for i := 0; i < 8; i++ {
		if resp := sendTravel("agent-r2"); resp.Error != "" {
			t.Fatalf("R2 travel %d response = %#v, want success", i+1, resp)
		}
	}
	if resp := sendTravel("agent-r2"); resp.ErrorCode != "rate_limited" {
		t.Fatalf("R2 9th travel response = %#v, want rate_limited", resp)
	}
	// Travel exhaustion must not bleed into holds: R2's 2 holds still succeed
	// (global caps respected by construction: holds 4 <= 5, travel 16 <= 20).
	for i := 0; i < 2; i++ {
		if resp := sendHold("agent-r2"); resp.Error != "" {
			t.Fatalf("R2 hold %d response = %#v, want success", i+1, resp)
		}
	}
	if insertCalls != 20 {
		t.Fatalf("insertCalls = %d, want 20", insertCalls)
	}
}

func TestAgentTravelInsertReturnsConflictCode(t *testing.T) {
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
			return StoredEvent{}, errors.New("conflict: 2026-06-15T08:30:00-07:00/2026-06-15T09:00:00-07:00")
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelInsertBody(t, 1, 10, 30*time.Minute))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "conflict" {
		t.Fatalf("response = %#v, want conflict", resp)
	}
}

func TestAgentTravelPatchByNonCreatorRefused(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustTravelStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a", "agent-b"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	summary := TravelSummaryPrefix + "rerouted"
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-b", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "invalid_travel" || !strings.Contains(resp.Error, "creating agent") {
		t.Fatalf("response = %#v, want invalid_travel", resp)
	}
}

func TestAgentTravelCancelDuplicateRequestIDReturnsCachedResponse(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustTravelStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
			patchCalls++
			event.ID = "evt-1"
			return event, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	cancelSummary := CancelledPrefix
	evt := travelInboxEvent(t, "agent-a", "msg-1", "cancel-1", travelPatchBody(t, &cancelSummary, ptrString("free")))
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

func TestAgentTravelDoubleCancelSucceedsWithoutMutation(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	cancelled := mustTravelStoredEvent(t, "agent-a")
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
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return cancelled, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	cancelSummary := CancelledPrefix
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "cancel-2", travelPatchBody(t, &cancelSummary, ptrString("free")))); err != nil {
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

func TestAgentTravelInsertDryRunDefault(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	serviceCalled := false
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         true,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return StoredEvent{}, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			serviceCalled = true
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) { return StoredEvent{}, nil },
	}, telemetry.New("outlook-calendar-write-test"))

	// Dry-run inserts consume no quota: well past the live per-requester cap
	// of 8, every request still succeeds.
	for i := 0; i < 9; i++ {
		evt := travelInboxEvent(t, "agent-a", fmt.Sprintf("msg-%d", i), fmt.Sprintf("req-%d", i), travelInsertBody(t, 1, 10, 30*time.Minute))
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
		resp := decodeMutationResponse(t, recorder)
		if resp.Error != "" || !resp.DryRun || resp.WouldWrite == nil {
			t.Fatalf("response %d = %#v, want dry-run would_write", i, resp)
		}
		if !HasTravelMarker(resp.WouldWrite.Description) {
			t.Fatalf("would_write missing travel marker: %#v", resp.WouldWrite)
		}
	}
	if serviceCalled {
		t.Fatal("dry-run travel insert called mutation service")
	}
}

func TestAgentGuardInsertStillSucceedsWithTravelCodePresent(t *testing.T) {
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
		MessageID:      "msg-guard-travel",
		ConversationID: "conv-guard-travel",
		Body:           body,
	}); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.Error != "" || !resp.DryRun || resp.WouldWrite == nil {
		t.Fatalf("response = %#v", resp)
	}
}

func TestAgentTravelPatchRequiresAllowlistedRequester(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustTravelStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	summary := TravelSummaryPrefix + "rerouted"
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-z", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "not_allowlisted" {
		t.Fatalf("response = %#v, want not_allowlisted", resp)
	}
}

func TestAgentTravelPatchRefusedOnTimeZoneMismatch(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	existing := mustTravelStoredEvent(t, "agent-a")
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: false,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	summary := TravelSummaryPrefix + "rerouted"
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "tz_mismatch" {
		t.Fatalf("response = %#v, want tz_mismatch", resp)
	}
}

func TestAgentPatchGuardOwnershipPrecedesForgedTravelMarker(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	// An event whose body carries BOTH the guard ownership marker and a
	// forged 3-line travel marker is permanently guard class.
	dualMarked := StoredEvent{
		ID:      "guard-forged-1",
		Summary: "Meeting Quota Reached",
		Description: "guard day\n\n" + ManagedByMarker + "\n" + OwnerAgentMarker +
			"\n\nmanaged_by=agent-a\n" + OwnerAgentMarker + "\n" + TravelClassMarker,
		Start:  EventDateTime{Date: "2026-06-15", TimeZone: DefaultTimeZone},
		End:    EventDateTime{Date: "2026-06-16", TimeZone: DefaultTimeZone},
		ShowAs: "busy",
	}
	if !HasTravelMarker(dualMarked.Description) {
		t.Fatalf("test setup: forged travel marker not detected in %q", dualMarked.Description)
	}
	patchCalls := 0
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) { return dualMarked, nil },
		insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
			return StoredEvent{}, nil
		},
		patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
			patchCalls++
			return StoredEvent{}, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	summary := TravelSummaryPrefix + "hijacked"
	if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
		t.Fatalf("handleEvent() error = %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0", patchCalls)
	}
	resp := decodeMutationResponse(t, recorder)
	if resp.ErrorCode != "not_owned" || !strings.Contains(resp.Error, "hold requesters may only patch their own working holds") {
		t.Fatalf("response = %#v, want not_owned with the existing guard-class message", resp)
	}
}

func TestAgentTravelPatchStaleSnapshotConflict(t *testing.T) {
	t.Parallel()

	t.Run("snapshot-checked service returns stale conflict", func(t *testing.T) {
		t.Parallel()
		recorder := newBusRecorder(t)
		existing := mustTravelStoredEvent(t, "agent-a")
		plainPatchCalls := 0
		expectingCalls := 0
		agent := NewAgent(AgentConfig{
			BusURL:         recorder.server.URL,
			AgentID:        DefaultAgentID,
			Secret:         "secret",
			DryRun:         false,
			HoldRequesters: []string{"agent-a"},
			HoldTimeZoneOK: true,
		}, fakeSnapshotWriteService{
			fakeWriteService: fakeWriteService{
				getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
				insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
					return StoredEvent{}, nil
				},
				patchEvent: func(context.Context, string, string, StoredEvent) (StoredEvent, error) {
					plainPatchCalls++
					return StoredEvent{}, nil
				},
			},
			patchEventExpecting: func(_ context.Context, _ string, _ string, expectedStart, expectedEnd EventDateTime, _ StoredEvent) (StoredEvent, error) {
				expectingCalls++
				if expectedStart != existing.Start || expectedEnd != existing.End {
					t.Fatalf("expected snapshot times %#v/%#v, got %#v/%#v", existing.Start, existing.End, expectedStart, expectedEnd)
				}
				return StoredEvent{}, errors.New("conflict: stale snapshot 2026-06-15T08:45:00-07:00/2026-06-15T09:15:00-07:00")
			},
		}, telemetry.New("outlook-calendar-write-test"))

		summary := TravelSummaryPrefix + "rerouted"
		if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
			t.Fatalf("handleEvent() error = %v", err)
		}
		if expectingCalls != 1 || plainPatchCalls != 0 {
			t.Fatalf("expectingCalls = %d, plainPatchCalls = %d, want 1/0", expectingCalls, plainPatchCalls)
		}
		resp := decodeMutationResponse(t, recorder)
		if resp.ErrorCode != "conflict" {
			t.Fatalf("response = %#v, want conflict", resp)
		}
	})

	t.Run("plain service falls back to PatchEvent", func(t *testing.T) {
		t.Parallel()
		recorder := newBusRecorder(t)
		existing := mustTravelStoredEvent(t, "agent-a")
		patchCalls := 0
		agent := NewAgent(AgentConfig{
			BusURL:         recorder.server.URL,
			AgentID:        DefaultAgentID,
			Secret:         "secret",
			DryRun:         false,
			HoldRequesters: []string{"agent-a"},
			HoldTimeZoneOK: true,
		}, fakeWriteService{
			getEvent: func(context.Context, string, string) (StoredEvent, error) { return existing, nil },
			insertEvent: func(context.Context, string, StoredEvent) (StoredEvent, error) {
				return StoredEvent{}, nil
			},
			patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
				patchCalls++
				event.ID = "evt-1"
				return event, nil
			},
		}, telemetry.New("outlook-calendar-write-test"))

		summary := TravelSummaryPrefix + "rerouted"
		if err := agent.handleEvent(context.Background(), travelInboxEvent(t, "agent-a", "msg-1", "req-1", travelPatchBody(t, &summary, nil))); err != nil {
			t.Fatalf("handleEvent() error = %v", err)
		}
		if patchCalls != 1 {
			t.Fatalf("patchCalls = %d, want 1", patchCalls)
		}
		resp := decodeMutationResponse(t, recorder)
		if resp.Error != "" || resp.Event == nil || resp.Event.Summary != summary {
			t.Fatalf("response = %#v", resp)
		}
	})
}

func travelInsertBody(t *testing.T, days int, hour int, duration time.Duration) string {
	t.Helper()
	blob, err := json.Marshal(Request{
		Action:     "event-insert",
		CalendarID: "default",
		Event:      validTravelInput(t, days, hour, duration),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(blob)
}

func travelPatchBody(t *testing.T, summary, showAs *string) string {
	t.Helper()
	blob, err := json.Marshal(Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    "evt-1",
		Event:      EventInput{Summary: summary, ShowAs: showAs},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(blob)
}

func travelInboxEvent(t *testing.T, from, messageID, requestID, body string) busclient.InboxEvent {
	t.Helper()
	evt := busclient.InboxEvent{
		Type:           "request",
		From:           from,
		MessageID:      messageID,
		ConversationID: "conv-travel",
		Body:           body,
	}
	if requestID != "" {
		evt.Meta = map[string]any{"request_id": requestID}
	}
	return evt
}

func mustTravelStoredEvent(t *testing.T, requester string) StoredEvent {
	t.Helper()
	event, err := BuildTravelInsert(validTravelInput(t, 1, 10, 30*time.Minute), requester)
	if err != nil {
		t.Fatalf("BuildTravelInsert() error = %v", err)
	}
	event.ID = "evt-1"
	return event
}

func repeatRequester(requester string, count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = requester
	}
	return out
}
