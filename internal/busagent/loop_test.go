package busagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type loopTestServer struct {
	t *testing.T

	mu            sync.Mutex
	registerCalls int
	pollCalls     int

	onRegister func(call int, body map[string]any)
	onPoll     func(w http.ResponseWriter, r *http.Request, call int)

	server *httptest.Server
}

func newLoopTestServer(t *testing.T) *loopTestServer {
	t.Helper()
	s := &loopTestServer{t: t}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.server.Close)
	return s
}

func (s *loopTestServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/register":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Fatalf("decode register body: %v", err)
		}
		s.mu.Lock()
		s.registerCalls++
		call := s.registerCalls
		s.mu.Unlock()
		if s.onRegister != nil {
			s.onRegister(call, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/inbox":
		s.mu.Lock()
		s.pollCalls++
		call := s.pollCalls
		s.mu.Unlock()
		if s.onPoll != nil {
			s.onPoll(w, r, call)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"cursor":"0"}`))
	default:
		s.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}
}

func TestRunHeartbeatReregisters(t *testing.T) {
	t.Parallel()

	origHeartbeat := heartbeatInterval
	origSleep := sleep
	heartbeatInterval = 20 * time.Millisecond
	sleep = func(time.Duration) {}
	t.Cleanup(func() {
		heartbeatInterval = origHeartbeat
		sleep = origSleep
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onRegister = func(call int, body map[string]any) {
		if call == 1 {
			if got := body["description"]; got != "loop test agent" {
				t.Fatalf("description = %#v, want loop test agent", got)
			}
		}
		if call >= 2 {
			cancel()
		}
	}

	loop := New(LoopConfig{
		BusURL:      server.server.URL,
		AgentID:     "loop-agent",
		Secret:      "secret",
		Description: "loop test agent",
		Metrics:     telemetry.New("loop-test"),
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.registerCalls < 2 {
		t.Fatalf("register calls = %d, want >= 2", server.registerCalls)
	}
}

func TestRunTickHandlerFiresAtInterval(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	var mu sync.Mutex
	ticks := make([]time.Time, 0, 3)

	loop := New(LoopConfig{
		BusURL:       server.server.URL,
		AgentID:      "tick-agent",
		Secret:       "secret",
		TickInterval: 15 * time.Millisecond,
		HandleTick: func(_ context.Context, now time.Time) error {
			mu.Lock()
			ticks = append(ticks, now)
			count := len(ticks)
			mu.Unlock()
			if count >= 3 {
				cancel()
			}
			return nil
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ticks) < 3 {
		t.Fatalf("tick count = %d, want >= 3", len(ticks))
	}
	if !ticks[1].After(ticks[0]) {
		t.Fatalf("tick order invalid: %v", ticks)
	}
}

func TestRunPollDeliversEventsToHandler(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"evt-1","type":"request","from":"jk-travel-agent","conversation_id":"conv-1","body":"hello","created_at":"2026-03-18T12:00:00Z"}],"cursor":"7"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"7"}`))
	}

	var got []busclient.InboxEvent
	loop := New(LoopConfig{
		BusURL:  server.server.URL,
		AgentID: "poll-agent",
		Secret:  "secret",
		HandleEvent: func(_ context.Context, evt busclient.InboxEvent) error {
			got = append(got, evt)
			cancel()
			return nil
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(got) != 1 || got[0].MessageID != "evt-1" {
		t.Fatalf("events = %#v", got)
	}
	if loop.cursor != 7 {
		t.Fatalf("cursor = %d, want 7", loop.cursor)
	}
}

func TestRunPersistsCursor(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"evt-1","type":"request","from":"jk-travel-agent","conversation_id":"conv-1","body":"hello","created_at":"2026-03-18T12:00:00Z"}],"cursor":"17"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"17"}`))
	}

	cursorFile := t.TempDir() + "/cursor.txt"
	loop := New(LoopConfig{
		BusURL:     server.server.URL,
		AgentID:    "cursor-agent",
		Secret:     "secret",
		CursorFile: cursorFile,
		HandleEvent: func(_ context.Context, _ busclient.InboxEvent) error {
			cancel()
			return nil
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	cursor, ok, err := loadCursor(cursorFile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cursor != 17 {
		t.Fatalf("cursor file = (%d, %v), want (17, true)", cursor, ok)
	}
}

func TestRunPersistsCursorAfterHandler(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"evt-1","type":"request","from":"jk-travel-agent","conversation_id":"conv-1","body":"hello","created_at":"2026-03-18T12:00:00Z"}],"cursor":"17"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"17"}`))
	}

	cursorFile := t.TempDir() + "/cursor.txt"
	handlerSawUnadvancedCursor := false
	loop := New(LoopConfig{
		BusURL:     server.server.URL,
		AgentID:    "cursor-after-handler-agent",
		Secret:     "secret",
		CursorFile: cursorFile,
		HandleEvent: func(_ context.Context, _ busclient.InboxEvent) error {
			_, ok, err := loadCursor(cursorFile)
			if err != nil {
				t.Fatal(err)
			}
			handlerSawUnadvancedCursor = !ok
			cancel()
			return nil
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if !handlerSawUnadvancedCursor {
		t.Fatal("cursor advanced before handler completed")
	}
	cursor, ok, err := loadCursor(cursorFile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cursor != 17 {
		t.Fatalf("cursor file = (%d, %v), want (17, true)", cursor, ok)
	}
}

func TestRunPrioritizesEventsBeforeHandling(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"fetch","type":"request","from":"a","conversation_id":"fetch-conv","body":"fetch","created_at":"2026-03-18T12:00:00Z"},{"message_id":"label","type":"request","from":"a","conversation_id":"label-conv","body":"label","created_at":"2026-03-18T12:00:01Z"}],"cursor":"9"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"9"}`))
	}

	var handled []string
	loop := New(LoopConfig{
		BusURL:  server.server.URL,
		AgentID: "priority-agent",
		Secret:  "secret",
		EventPriority: func(evt busclient.InboxEvent) int {
			if evt.MessageID == "label" {
				return 0
			}
			return 10
		},
		HandleEvent: func(_ context.Context, evt busclient.InboxEvent) error {
			handled = append(handled, evt.MessageID)
			if len(handled) == 2 {
				cancel()
			}
			return nil
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := strings.Join(handled, ","); got != "label,fetch" {
		t.Fatalf("handled order = %s, want label,fetch", got)
	}
}

func TestRunStartAtLatestSkipsBacklog(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			if got := r.URL.Query().Get("cursor"); got == "" {
				t.Fatalf("initial cursor is empty")
			}
			_, _ = w.Write([]byte(`{"events":[],"cursor":"2337"}`))
			return
		}
		if got := r.URL.Query().Get("cursor"); got != "2337" {
			t.Fatalf("poll cursor = %q, want 2337", got)
		}
		cancel()
		_, _ = w.Write([]byte(`{"events":[],"cursor":"2337"}`))
	}

	loop := New(LoopConfig{
		BusURL:        server.server.URL,
		AgentID:       "latest-agent",
		Secret:        "secret",
		StartAtLatest: true,
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if loop.cursor != 2337 {
		t.Fatalf("cursor = %d, want 2337", loop.cursor)
	}
}

func TestRunHeartbeatContinuesWhileHandlerBlocked(t *testing.T) {
	t.Parallel()

	origHeartbeat := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		heartbeatInterval = origHeartbeat
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	started := make(chan struct{})
	done := make(chan struct{})
	var closeDone sync.Once
	server.onRegister = func(call int, _ map[string]any) {
		if call >= 2 {
			closeDone.Do(func() { close(done) })
		}
	}
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"evt-1","type":"request","from":"jk-travel-agent","conversation_id":"conv-1","body":"hello","created_at":"2026-03-18T12:00:00Z"}],"cursor":"1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"cursor":"1"}`))
	}

	loop := New(LoopConfig{
		BusURL:  server.server.URL,
		AgentID: "heartbeat-during-handler-agent",
		Secret:  "secret",
		HandleEvent: func(ctx context.Context, _ busclient.InboxEvent) error {
			close(started)
			select {
			case <-done:
				cancel()
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				t.Fatal("heartbeat did not run while handler was blocked")
				return nil
			}
		},
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("handler did not start")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.registerCalls < 2 {
		t.Fatalf("register calls = %d, want >= 2", server.registerCalls)
	}
}

func TestRunReRegistersOnUnauthorizedPoll(t *testing.T) {
	t.Parallel()

	origSleep := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = origSleep })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onRegister = func(call int, _ map[string]any) {
		if call >= 2 {
			cancel()
		}
	}
	server.onPoll = func(w http.ResponseWriter, _ *http.Request, call int) {
		if call == 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"cursor":"0"}`))
	}

	loop := New(LoopConfig{
		BusURL:  server.server.URL,
		AgentID: "reauth-agent",
		Secret:  "secret",
	})

	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.registerCalls < 2 {
		t.Fatalf("register calls = %d, want >= 2", server.registerCalls)
	}
	if server.pollCalls < 1 {
		t.Fatalf("poll calls = %d, want >= 1", server.pollCalls)
	}
}

func TestRunShutdownDrainsInFlightHandlerUntilGrace(t *testing.T) {
	t.Parallel()

	grace := 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newLoopTestServer(t)
	server.onPoll = func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"events":[{"message_id":"evt-1","type":"request","from":"jk-travel-agent","conversation_id":"conv-1","body":"hello","created_at":"2026-03-18T12:00:00Z"}],"cursor":"1"}`))
			return
		}
		<-r.Context().Done()
		_, _ = w.Write([]byte(`{"events":[],"cursor":"1"}`))
	}

	started := make(chan struct{})
	finished := make(chan struct{})
	loop := New(LoopConfig{
		BusURL:        server.server.URL,
		AgentID:       "shutdown-agent",
		Secret:        "secret",
		ShutdownGrace: grace,
		HandleEvent: func(ctx context.Context, evt busclient.InboxEvent) error {
			if evt.MessageID != "evt-1" {
				t.Fatalf("unexpected event: %#v", evt)
			}
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		},
	})

	done := make(chan error, 1)
	go func() {
		done <- loop.Run(ctx)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler never started")
	}

	cancel()
	select {
	case <-finished:
		t.Fatal("handler canceled before grace elapsed")
	case <-time.After(grace / 2):
	}

	start := time.Now()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after grace period")
	}
	if elapsed := time.Since(start) + grace/2; elapsed+2*time.Millisecond < grace {
		t.Fatalf("handler canceled too early: %s < %s", elapsed, grace)
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return")
	}
}
