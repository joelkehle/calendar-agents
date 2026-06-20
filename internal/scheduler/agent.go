package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/joelkehle/calendar-agents/internal/busagent"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type Agent struct {
	cfg     Config
	loop    *busagent.Loop
	metrics *telemetry.Registry
	cache   *replyCache
	jobs    chan schedulerJob

	// knowledge is nil when travel knowledge failed to load (§1.6
	// degradation): scheduler.v1 runs exactly as before, travel-estimate and
	// offsite booking reply estimate_unavailable, the watcher does nothing.
	knowledge *travelknowledge.Knowledge
	watcher   *travelWatcher

	startWorkersOnce sync.Once

	mu       sync.Mutex
	routes   map[string]chan busclient.InboxEvent
	inflight map[string][]busclient.InboxEvent
}

type schedulerJob struct {
	key string
	evt busclient.InboxEvent
	req Request
}

type upstreamSession struct {
	conversationID string
	keyHash        string
	responses      chan busclient.InboxEvent
}

func NewAgent(cfg Config, metrics *telemetry.Registry) *Agent {
	cfg = withDefaults(cfg)
	a := &Agent{
		cfg:      cfg,
		metrics:  metrics,
		cache:    newReplyCache(),
		jobs:     make(chan schedulerJob, 64),
		routes:   make(map[string]chan busclient.InboxEvent),
		inflight: make(map[string][]busclient.InboxEvent),
	}
	knowledge, err := travelknowledge.Load(cfg.LocationsPath, cfg.VenuesPath)
	if err != nil {
		metrics.IncCounter("travel_knowledge_load_failed")
		log.Printf("%s travel knowledge unavailable (travel features degraded): %v", cfg.AgentID, err)
	} else {
		a.knowledge = knowledge
	}
	a.watcher = newTravelWatcher(a)
	a.loop = busagent.New(busagent.LoopConfig{
		BusURL:        cfg.BusURL,
		AgentID:       cfg.AgentID,
		Secret:        cfg.Secret,
		Capabilities:  []string{CapabilityRequest, CapabilityMove, CapabilityCancel, CapabilityEstimate},
		Description:   "Intent-level scheduler that books, moves, and cancels agent working holds on Joel's calendar; answers travel-estimate; reconciles travel blocks around offsite meetings.",
		AgentClass:    "orchestrator",
		MutationClass: "mutate",
		PollWaitSec:   cfg.PollWaitSec,
		Metrics:       metrics,
		HandleEvent:   a.handleEvent,
	})
	return a
}

func (a *Agent) Run(ctx context.Context) error {
	a.startWorkers(ctx)
	go a.watcher.run(ctx)
	return a.loop.Run(ctx)
}

func (a *Agent) startWorkers(ctx context.Context) {
	a.startWorkersOnce.Do(func() {
		for i := 0; i < a.cfg.Workers; i++ {
			go a.worker(ctx)
		}
	})
}

func (a *Agent) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.jobs:
			a.runJob(ctx, job)
		}
	}
}

func (a *Agent) handleEvent(ctx context.Context, evt busclient.InboxEvent) error {
	if evt.Type == "response" && strings.HasPrefix(evt.ConversationID, "sched-") {
		a.routeResponse(evt)
		return nil
	}
	if evt.Type != "request" {
		return nil
	}
	if strings.HasPrefix(evt.ConversationID, "sched-") {
		return a.loop.Client().Ack(ctx, a.cfg.AgentID, a.cfg.Secret, evt.MessageID, "rejected", "scheduler ignores derived conversations")
	}
	if err := a.loop.Client().Ack(ctx, a.cfg.AgentID, a.cfg.Secret, evt.MessageID, "accepted", "scheduler request accepted"); err != nil {
		return err
	}
	req, err := DecodeRequest(evt.Body)
	if err != nil {
		return a.sendReply(ctx, evt, errorReply("", ErrorInvalidRequest, fmt.Sprintf("invalid scheduler request: %v", err)))
	}
	// Prohibited-field refusal is action-independent (§2.4 step 2); for
	// scheduler.v1 actions validateRequest performs the identical check first,
	// so existing replies are unchanged.
	if req.ProhibitedField() != "" {
		return a.sendReply(ctx, evt, refusedReply(req.RequestID))
	}
	// travel-estimate is answered synchronously, BEFORE validateRequest (whose
	// action switch would reject it), before the reply cache (its key has no
	// action component — a reused request_id must never replay a booked
	// reply), and before the inflight/enqueue logic. No idempotency cache
	// entry is stored: the reply is deterministic.
	if strings.ToLower(strings.TrimSpace(req.Action)) == CapabilityEstimate {
		return a.sendReply(ctx, evt, a.estimateReply(req))
	}
	if reqErr := validateRequest(req); reqErr != nil {
		if reqErr.Code == ErrorOtherPeople {
			return a.sendReply(ctx, evt, refusedReply(req.RequestID))
		}
		return a.sendReply(ctx, evt, errorReply(req.RequestID, reqErr.Code, reqErr.Message))
	}
	key := canonicalKey(evt.From, req.RequestID)
	if cached, ok := a.cache.Get(key); ok {
		a.metrics.IncCounter("schedule_idempotent_replay")
		cached.RequestID = req.RequestID
		return a.sendReply(ctx, evt, cached)
	}

	a.mu.Lock()
	if _, ok := a.inflight[key]; ok {
		a.inflight[key] = append(a.inflight[key], evt)
		a.mu.Unlock()
		return nil
	}
	a.inflight[key] = nil
	a.mu.Unlock()

	select {
	case a.jobs <- schedulerJob{key: key, evt: evt, req: req}:
		a.metrics.IncCounter("schedule_jobs_enqueued")
		return nil
	case <-ctx.Done():
		a.clearInflight(key)
		return ctx.Err()
	}
}

func validateRequest(req Request) *requestError {
	if req.ProhibitedField() != "" {
		return &requestError{Code: ErrorOtherPeople, Message: "escalate to Joel"}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		return &requestError{Code: ErrorInvalidRequest, Message: "request_id is required"}
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case CapabilityRequest:
		if strings.TrimSpace(req.Purpose) == "" {
			return &requestError{Code: ErrorInvalidRequest, Message: "purpose is required"}
		}
		if strings.TrimSpace(req.Agenda) == "" {
			return &requestError{Code: ErrorInvalidRequest, Message: "agenda is required"}
		}
		if _, err := requestDuration(req.DurationMinutes, true); err != nil {
			return &requestError{Code: ErrorInvalidRequest, Message: err.Error()}
		}
		if utf8.RuneCountInString(strings.TrimSpace(req.Location)) > maxLocationRunes {
			return &requestError{Code: ErrorInvalidRequest, Message: "location must be 200 characters or fewer"}
		}
	case CapabilityMove:
		if strings.TrimSpace(req.EventID) == "" {
			return &requestError{Code: ErrorInvalidRequest, Message: "event_id is required"}
		}
		if _, err := requestDuration(req.DurationMinutes, false); err != nil {
			return &requestError{Code: ErrorInvalidRequest, Message: err.Error()}
		}
	case CapabilityCancel:
		if strings.TrimSpace(req.EventID) == "" {
			return &requestError{Code: ErrorInvalidRequest, Message: "event_id is required"}
		}
	default:
		return &requestError{Code: ErrorInvalidRequest, Message: "unsupported scheduler action"}
	}
	return nil
}

func requestDuration(minutes int, required bool) (time.Duration, error) {
	if minutes == 0 && !required {
		minutes = 60
	}
	if minutes == 0 && required {
		return 0, errors.New("duration_minutes is required")
	}
	if minutes < 15 || minutes > 120 || minutes%15 != 0 {
		return 0, errors.New("duration_minutes must be 15-120 and a multiple of 15")
	}
	return time.Duration(minutes) * time.Minute, nil
}

func (a *Agent) runJob(ctx context.Context, job schedulerJob) {
	reply := a.executeJob(ctx, job)
	if !reply.Terminal() {
		reply = errorReply(job.req.RequestID, ErrorUpstreamUnavailable, "scheduler job did not produce a terminal reply")
	}
	a.cache.Put(job.key, reply)
	waiting := a.clearInflight(job.key)
	_ = a.sendReply(ctx, job.evt, reply)
	for _, evt := range waiting {
		_ = a.sendReply(ctx, evt, reply)
	}
}

func (a *Agent) routeResponse(evt busclient.InboxEvent) {
	a.mu.Lock()
	ch := a.routes[evt.ConversationID]
	a.mu.Unlock()
	if ch == nil {
		log.Printf("%s unmatched scheduler response conversation_id=%s from=%s", a.cfg.AgentID, evt.ConversationID, evt.From)
		return
	}
	select {
	case ch <- evt:
	default:
		log.Printf("%s dropped scheduler response conversation_id=%s from=%s reason=pending channel full", a.cfg.AgentID, evt.ConversationID, evt.From)
	}
}

func (a *Agent) clearInflight(key string) []busclient.InboxEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiting := append([]busclient.InboxEvent{}, a.inflight[key]...)
	delete(a.inflight, key)
	return waiting
}

func (a *Agent) sendReply(ctx context.Context, evt busclient.InboxEvent, reply Reply) error {
	body, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = a.loop.Client().SendMessage(
		ctx,
		a.cfg.AgentID,
		a.cfg.Secret,
		evt.From,
		evt.ConversationID,
		fmt.Sprintf("%s-response-%s", a.cfg.AgentID, evt.MessageID),
		"response",
		string(body),
		nil,
		nil,
	)
	return err
}

func (a *Agent) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}
