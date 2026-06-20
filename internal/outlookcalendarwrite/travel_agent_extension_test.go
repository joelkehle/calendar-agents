package outlookcalendarwrite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
)

// TestTravelIdempotency (SCHEDULER_TRAVEL_SPEC §3.7/§9): a duplicate
// meta.request_id returns the cached response WITH replayed: true and a
// single service call; fresh responses omit the field entirely.
func TestTravelIdempotencyReplayedFlag(t *testing.T) {
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
	fresh := decodeMutationResponse(t, recorder)
	if fresh.Replayed {
		t.Fatalf("fresh response = %#v, must not be marked replayed", fresh)
	}
	if strings.Contains(recorder.lastMessageBody(t), `"replayed"`) {
		t.Fatalf("fresh response JSON must omit replayed: %s", recorder.lastMessageBody(t))
	}

	evt.MessageID = "msg-2"
	if err := agent.handleEvent(context.Background(), evt); err != nil {
		t.Fatalf("second handleEvent() error = %v", err)
	}
	if insertCalls != 1 {
		t.Fatalf("insertCalls = %d, want 1", insertCalls)
	}
	replayed := decodeMutationResponse(t, recorder)
	if !replayed.Replayed || replayed.Event == nil || replayed.Event.ID != "evt-1" {
		t.Fatalf("replayed response = %#v, want replayed=true with cached event", replayed)
	}
}

// TestTravelAllowlistAndCaps budget-release row (SCHEDULER_TRAVEL_SPEC
// §3.6/§9): a successful LIVE travel cancel releases one unit of the date's
// insert budget, so failed-booking compensation does not permanently consume
// the date's travel protection. Double-cancel must not release again.
func TestAgentTravelCancelReleasesBudget(t *testing.T) {
	t.Parallel()

	recorder := newBusRecorder(t)
	insertCalls := 0
	stored := mustTravelStoredEvent(t, "agent-a")
	cancelled := false
	agent := NewAgent(AgentConfig{
		BusURL:         recorder.server.URL,
		AgentID:        DefaultAgentID,
		Secret:         "secret",
		DryRun:         false,
		HoldRequesters: []string{"agent-a"},
		HoldTimeZoneOK: true,
	}, fakeWriteService{
		getEvent: func(context.Context, string, string) (StoredEvent, error) {
			if cancelled {
				out := stored
				out.Summary = CancelledPrefix + out.Summary
				out.ShowAs = "free"
				return out, nil
			}
			return stored, nil
		},
		insertEvent: func(_ context.Context, _ string, event StoredEvent) (StoredEvent, error) {
			insertCalls++
			event.ID = fmt.Sprintf("evt-%d", insertCalls)
			return event, nil
		},
		patchEvent: func(_ context.Context, _ string, _ string, event StoredEvent) (StoredEvent, error) {
			event.ID = "evt-1"
			return event, nil
		},
	}, telemetry.New("outlook-calendar-write-test"))

	// Burn the full per-requester budget (8/event-date).
	for i := 0; i < 8; i++ {
		evt := travelInboxEvent(t, "agent-a", fmt.Sprintf("msg-%d", i), fmt.Sprintf("req-%d", i), travelInsertBody(t, 1, 10, 30*time.Minute))
		if err := agent.handleEvent(context.Background(), evt); err != nil {
			t.Fatalf("handleEvent(%d) error = %v", i, err)
		}
	}
	if insertCalls != 8 {
		t.Fatalf("insertCalls = %d, want 8", insertCalls)
	}

	// Budget exhausted: 9th refused.
	refusedEvt := travelInboxEvent(t, "agent-a", "msg-refused", "req-refused", travelInsertBody(t, 1, 10, 30*time.Minute))
	if err := agent.handleEvent(context.Background(), refusedEvt); err != nil {
		t.Fatalf("handleEvent(refused) error = %v", err)
	}
	if resp := decodeMutationResponse(t, recorder); resp.ErrorCode != "rate_limited" {
		t.Fatalf("response = %#v, want rate_limited", resp)
	}

	// Live cancel of one block (same event local date) releases one unit.
	cancelSummary := CancelledPrefix
	showAsFree := "free"
	cancelEvt := travelInboxEvent(t, "agent-a", "msg-cancel", "req-cancel", travelPatchBody(t, &cancelSummary, &showAsFree))
	if err := agent.handleEvent(context.Background(), cancelEvt); err != nil {
		t.Fatalf("handleEvent(cancel) error = %v", err)
	}
	if resp := decodeMutationResponse(t, recorder); resp.Error != "" || resp.Event == nil || !strings.HasPrefix(resp.Event.Summary, CancelledPrefix) {
		t.Fatalf("cancel response = %#v", resp)
	}
	cancelled = true

	// Released budget admits a new insert.
	retryEvt := travelInboxEvent(t, "agent-a", "msg-retry", "req-retry", travelInsertBody(t, 1, 11, 30*time.Minute))
	if err := agent.handleEvent(context.Background(), retryEvt); err != nil {
		t.Fatalf("handleEvent(retry) error = %v", err)
	}
	if resp := decodeMutationResponse(t, recorder); resp.Error != "" || resp.Event == nil {
		t.Fatalf("post-release insert response = %#v, want success", resp)
	}
	if insertCalls != 9 {
		t.Fatalf("insertCalls = %d, want 9", insertCalls)
	}

	// Double-cancel is an idempotent no-mutation success and must NOT release
	// another unit: the next insert is refused again.
	doubleCancelEvt := travelInboxEvent(t, "agent-a", "msg-cancel-2", "req-cancel-2", travelPatchBody(t, &cancelSummary, &showAsFree))
	if err := agent.handleEvent(context.Background(), doubleCancelEvt); err != nil {
		t.Fatalf("handleEvent(double cancel) error = %v", err)
	}
	overEvt := travelInboxEvent(t, "agent-a", "msg-over", "req-over", travelInsertBody(t, 1, 12, 30*time.Minute))
	if err := agent.handleEvent(context.Background(), overEvt); err != nil {
		t.Fatalf("handleEvent(over) error = %v", err)
	}
	if resp := decodeMutationResponse(t, recorder); resp.ErrorCode != "rate_limited" {
		t.Fatalf("response after double cancel = %#v, want rate_limited (no second release)", resp)
	}
}
