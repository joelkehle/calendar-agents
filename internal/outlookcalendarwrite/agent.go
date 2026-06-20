package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/joelkehle/calendar-agents/internal/busagent"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type AgentConfig struct {
	BusURL            string
	AgentID           string
	Secret            string
	PollWaitSec       int
	DryRun            bool
	HoldRequesters    []string
	HoldTimeZoneOK    bool
	HoldTimeZoneError string
}

type Agent struct {
	cfg            AgentConfig
	service        Service
	loop           *busagent.Loop
	metrics        *telemetry.Registry
	holdRequesters map[string]struct{}
	holdCache      *holdResponseCache
	holdRates      *holdRateLimiter
	travelRates    *holdRateLimiter
}

func NewAgent(cfg AgentConfig, service Service, metrics *telemetry.Registry) *Agent {
	a := &Agent{
		cfg:            cfg,
		service:        service,
		metrics:        metrics,
		holdRequesters: holdRequesterSet(cfg.HoldRequesters),
		holdCache:      newHoldResponseCache(),
		holdRates:      newHoldRateLimiter(),
		travelRates:    newTravelRateLimiter(),
	}
	a.loop = busagent.New(busagent.LoopConfig{
		BusURL:        cfg.BusURL,
		AgentID:       cfg.AgentID,
		Secret:        cfg.Secret,
		Capabilities:  []string{"event-insert", "event-patch"},
		Description:   "Writable Outlook calendar guard-block, working-hold, and travel-block agent for Joel Kehle. Only creates or patches owned sanctioned events.",
		AgentClass:    "worker",
		MutationClass: "mutate",
		PollWaitSec:   cfg.PollWaitSec,
		Metrics:       metrics,
		HandleEvent:   a.handleEvent,
	})
	return a
}

func (a *Agent) Run(ctx context.Context) error {
	return a.loop.Run(ctx)
}

func (a *Agent) handleEvent(ctx context.Context, evt busclient.InboxEvent) error {
	if evt.Type != "request" {
		return nil
	}
	if err := a.loop.Client().Ack(ctx, a.cfg.AgentID, a.cfg.Secret, evt.MessageID, "accepted", "outlook calendar write request received"); err != nil {
		return err
	}

	var req Request
	if err := json.Unmarshal([]byte(evt.Body), &req); err != nil {
		return a.sendError(ctx, evt, fmt.Sprintf("invalid outlook calendar write request: %v", err))
	}

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "event-insert":
		return a.handleInsert(ctx, evt, req)
	case "event-patch":
		return a.handlePatch(ctx, evt, req)
	default:
		return a.sendError(ctx, evt, "unsupported outlook calendar write action")
	}
}

func (a *Agent) handleInsert(ctx context.Context, evt busclient.InboxEvent, req Request) error {
	calendarID, err := normalizeCalendarID(req.CalendarID)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	if IsHoldInsert(req.Event) {
		return a.handleHoldInsert(ctx, evt, calendarID, req)
	}
	if IsTravelInsert(req.Event) {
		return a.handleTravelInsert(ctx, evt, calendarID, req)
	}
	event, err := BuildInsert(req.Event)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_insert_dry_run")
		return a.sendJSON(ctx, evt, MutationResponse{DryRun: true, WouldWrite: &event})
	}
	created, err := a.service.InsertEvent(ctx, calendarID, event)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	a.metrics.IncCounter("event_insert")
	return a.sendJSON(ctx, evt, MutationResponse{DryRun: false, Event: &created})
}

func (a *Agent) handlePatch(ctx context.Context, evt busclient.InboxEvent, req Request) error {
	calendarID, err := normalizeCalendarID(req.CalendarID)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	eventID := strings.TrimSpace(req.EventID)
	if eventID == "" {
		return a.sendError(ctx, evt, "event_id is required")
	}
	existing, err := a.service.GetEvent(ctx, calendarID, eventID)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	// Classification precedence (forgery resistance): an event carrying the
	// guard ownership marker is ALWAYS guard class, even if its body also
	// embeds a hold/travel marker block — a forged marker smuggled through
	// the unauthenticated guard insert path can never be managed via the
	// hold or travel paths.
	if HasOwnershipMarker(existing.Description) {
		return a.handleGuardPatch(ctx, evt, calendarID, eventID, existing, req)
	}
	if IsHoldPatch(existing) {
		return a.handleHoldPatch(ctx, evt, calendarID, eventID, existing, req)
	}
	if IsTravelPatch(existing) {
		return a.handleTravelPatch(ctx, evt, calendarID, eventID, existing, req)
	}
	return a.handleGuardPatch(ctx, evt, calendarID, eventID, existing, req)
}

func (a *Agent) handleGuardPatch(ctx context.Context, evt busclient.InboxEvent, calendarID, eventID string, existing StoredEvent, req Request) error {
	if a.isHoldRequester(evt.From) {
		return a.sendErrorCode(ctx, evt, "not_owned", "hold requesters may only patch their own working holds")
	}
	merged, err := BuildPatch(existing, req.Event)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_patch_dry_run")
		return a.sendJSON(ctx, evt, MutationResponse{DryRun: true, WouldWrite: &merged})
	}
	updated, err := a.service.PatchEvent(ctx, calendarID, eventID, merged)
	if err != nil {
		return a.sendError(ctx, evt, err.Error())
	}
	a.metrics.IncCounter("event_patch")
	return a.sendJSON(ctx, evt, MutationResponse{DryRun: false, Event: &updated})
}

func (a *Agent) handleHoldInsert(ctx context.Context, evt busclient.InboxEvent, calendarID string, req Request) error {
	if !a.isHoldRequester(evt.From) {
		return a.sendErrorCode(ctx, evt, "not_allowlisted", "requesting agent is not allowlisted for working holds")
	}
	if !a.cfg.HoldTimeZoneOK {
		message := strings.TrimSpace(a.cfg.HoldTimeZoneError)
		if message == "" {
			message = "host timezone has not been verified for working holds"
		}
		return a.sendErrorCode(ctx, evt, "tz_mismatch", message)
	}
	key := holdIdempotencyKey(evt)
	if cached, ok := a.holdCache.Get(key); ok {
		a.metrics.IncCounter("event_hold_insert_idempotent")
		cached.Replayed = true
		return a.sendJSON(ctx, evt, cached)
	}
	eventDate, dateOK := HoldEventLocalDate(req.Event)
	if !a.cfg.DryRun && dateOK && !a.holdRates.Allow(evt.From, eventDate) {
		return a.sendErrorCode(ctx, evt, "rate_limited", "working-hold insert rate limit exceeded for event date")
	}
	event, err := BuildHoldInsert(req.Event, evt.From)
	if err != nil {
		return a.sendErrorCode(ctx, evt, "invalid_hold", err.Error())
	}
	if !dateOK {
		eventDate = storedEventLocalDate(event)
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_hold_insert_dry_run")
		resp := MutationResponse{DryRun: true, WouldWrite: &event}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	created, err := a.service.InsertEvent(ctx, calendarID, event)
	if err != nil {
		return a.sendErrorCode(ctx, evt, holdServiceErrorCode(err), err.Error())
	}
	a.holdRates.Record(evt.From, eventDate)
	a.metrics.IncCounter("event_hold_insert")
	resp := MutationResponse{DryRun: false, Event: &created}
	a.holdCache.Put(key, resp)
	return a.sendJSON(ctx, evt, resp)
}

func (a *Agent) handleHoldPatch(ctx context.Context, evt busclient.InboxEvent, calendarID, eventID string, existing StoredEvent, req Request) error {
	if !a.isHoldRequester(evt.From) {
		return a.sendErrorCode(ctx, evt, "not_allowlisted", "requesting agent is not allowlisted for working holds")
	}
	key := holdIdempotencyKey(evt)
	if cached, ok := a.holdCache.Get(key); ok {
		a.metrics.IncCounter("event_hold_patch_idempotent")
		cached.Replayed = true
		return a.sendJSON(ctx, evt, cached)
	}
	if !a.cfg.HoldTimeZoneOK {
		message := strings.TrimSpace(a.cfg.HoldTimeZoneError)
		if message == "" {
			message = "host timezone has not been verified for working holds"
		}
		return a.sendErrorCode(ctx, evt, "tz_mismatch", message)
	}
	merged, err := BuildHoldPatch(existing, req.Event, evt.From)
	if err != nil {
		return a.sendErrorCode(ctx, evt, "invalid_hold", err.Error())
	}
	if isCancelledHold(existing) && merged == existing {
		a.metrics.IncCounter("event_hold_cancel_idempotent")
		resp := MutationResponse{DryRun: a.cfg.DryRun, Event: &existing}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_hold_patch_dry_run")
		resp := MutationResponse{DryRun: true, WouldWrite: &merged}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	updated, err := a.service.PatchEvent(ctx, calendarID, eventID, merged)
	if err != nil {
		return a.sendErrorCode(ctx, evt, holdServiceErrorCode(err), err.Error())
	}
	a.metrics.IncCounter("event_hold_patch")
	resp := MutationResponse{DryRun: false, Event: &updated}
	a.holdCache.Put(key, resp)
	return a.sendJSON(ctx, evt, resp)
}

func (a *Agent) handleTravelInsert(ctx context.Context, evt busclient.InboxEvent, calendarID string, req Request) error {
	if !a.isHoldRequester(evt.From) {
		return a.sendErrorCode(ctx, evt, "not_allowlisted", "requesting agent is not allowlisted for travel blocks")
	}
	if !a.cfg.HoldTimeZoneOK {
		message := strings.TrimSpace(a.cfg.HoldTimeZoneError)
		if message == "" {
			message = "host timezone has not been verified for travel blocks"
		}
		return a.sendErrorCode(ctx, evt, "tz_mismatch", message)
	}
	key := travelIdempotencyKey(evt, "event-insert")
	if cached, ok := a.holdCache.Get(key); ok {
		a.metrics.IncCounter("event_travel_insert_idempotent")
		cached.Replayed = true
		return a.sendJSON(ctx, evt, cached)
	}
	eventDate, dateOK := HoldEventLocalDate(req.Event)
	if !a.cfg.DryRun && dateOK && !a.travelRates.Allow(evt.From, eventDate) {
		return a.sendErrorCode(ctx, evt, "rate_limited", "travel-block insert rate limit exceeded for event date")
	}
	event, err := BuildTravelInsert(req.Event, evt.From)
	if err != nil {
		return a.sendErrorCode(ctx, evt, "invalid_travel", err.Error())
	}
	if !dateOK {
		eventDate = storedEventLocalDate(event)
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_travel_insert_dry_run")
		resp := MutationResponse{DryRun: true, WouldWrite: &event}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	created, err := a.service.InsertEvent(ctx, calendarID, event)
	if err != nil {
		return a.sendErrorCode(ctx, evt, holdServiceErrorCode(err), err.Error())
	}
	a.travelRates.Record(evt.From, eventDate)
	a.metrics.IncCounter("event_travel_insert")
	resp := MutationResponse{DryRun: false, Event: &created}
	a.holdCache.Put(key, resp)
	return a.sendJSON(ctx, evt, resp)
}

func (a *Agent) handleTravelPatch(ctx context.Context, evt busclient.InboxEvent, calendarID, eventID string, existing StoredEvent, req Request) error {
	if !a.isHoldRequester(evt.From) {
		return a.sendErrorCode(ctx, evt, "not_allowlisted", "requesting agent is not allowlisted for travel blocks")
	}
	key := travelIdempotencyKey(evt, "event-patch")
	if cached, ok := a.holdCache.Get(key); ok {
		a.metrics.IncCounter("event_travel_patch_idempotent")
		cached.Replayed = true
		return a.sendJSON(ctx, evt, cached)
	}
	if !a.cfg.HoldTimeZoneOK {
		message := strings.TrimSpace(a.cfg.HoldTimeZoneError)
		if message == "" {
			message = "host timezone has not been verified for travel blocks"
		}
		return a.sendErrorCode(ctx, evt, "tz_mismatch", message)
	}
	merged, err := BuildTravelPatch(existing, req.Event, evt.From)
	if err != nil {
		return a.sendErrorCode(ctx, evt, "invalid_travel", err.Error())
	}
	if isCancelledHold(existing) && merged == existing {
		a.metrics.IncCounter("event_travel_cancel_idempotent")
		resp := MutationResponse{DryRun: a.cfg.DryRun, Event: &existing}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	if a.cfg.DryRun {
		a.metrics.IncCounter("event_travel_patch_dry_run")
		resp := MutationResponse{DryRun: true, WouldWrite: &merged}
		a.holdCache.Put(key, resp)
		return a.sendJSON(ctx, evt, resp)
	}
	updated, err := a.patchTravelEvent(ctx, calendarID, eventID, existing, merged)
	if err != nil {
		return a.sendErrorCode(ctx, evt, holdServiceErrorCode(err), err.Error())
	}
	// Budget release on cancel (SCHEDULER_TRAVEL_SPEC §3.6): a LIVE patch that
	// transitions the block into the cancelled state returns one unit of the
	// cancelling sender's travel insert budget for the block's event date, so
	// failed-booking compensation and watcher orphan cancels do not
	// permanently starve a date's travel protection. Double-cancels are
	// handled above (no mutation, no release).
	if !isCancelledHold(existing) && isCancelledHold(updated) {
		a.travelRates.Release(evt.From, storedEventLocalDate(existing))
		a.metrics.IncCounter("event_travel_budget_released")
	}
	a.metrics.IncCounter("event_travel_patch")
	resp := MutationResponse{DryRun: false, Event: &updated}
	a.holdCache.Put(key, resp)
	return a.sendJSON(ctx, evt, resp)
}

// patchTravelEvent applies a live travel patch with a stale-snapshot check
// when the mutation service supports it (lost-update protection: Joel may
// have moved the event in Outlook between the GetEvent read and this write).
// Services without the capability fall back to plain PatchEvent.
func (a *Agent) patchTravelEvent(ctx context.Context, calendarID, eventID string, existing, merged StoredEvent) (StoredEvent, error) {
	if snapshotChecked, ok := a.service.(SnapshotCheckedService); ok {
		return snapshotChecked.PatchEventExpecting(ctx, calendarID, eventID, existing.Start, existing.End, merged)
	}
	return a.service.PatchEvent(ctx, calendarID, eventID, merged)
}

func (a *Agent) sendJSON(ctx context.Context, evt busclient.InboxEvent, payload any) error {
	blob, _ := json.Marshal(payload)
	_, err := a.loop.Client().SendMessage(
		ctx,
		a.cfg.AgentID,
		a.cfg.Secret,
		evt.From,
		evt.ConversationID,
		fmt.Sprintf("%s-response-%s", a.cfg.AgentID, evt.MessageID),
		"response",
		string(blob),
		nil,
		nil,
	)
	return err
}

func (a *Agent) sendError(ctx context.Context, evt busclient.InboxEvent, message string) error {
	return a.sendErrorCode(ctx, evt, "", message)
}

func (a *Agent) sendErrorCode(ctx context.Context, evt busclient.InboxEvent, code, message string) error {
	a.metrics.IncCounter("errors")
	message = strings.TrimSpace(message)
	log.Printf("outlook calendar write request failed action=%s from=%s conversation_id=%s err=%s", extractAction(evt.Body), evt.From, evt.ConversationID, message)
	return a.sendJSON(ctx, evt, MutationResponse{DryRun: a.cfg.DryRun, Error: message, ErrorCode: strings.TrimSpace(code)})
}

func extractAction(body string) string {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return ""
	}
	return strings.TrimSpace(req.Action)
}

func normalizeCalendarID(calendarID string) (string, error) {
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		calendarID = "default"
	}
	switch strings.ToLower(calendarID) {
	case "default", "outlook-primary":
		return calendarID, nil
	default:
		return "", fmt.Errorf("calendar_id must be default")
	}
}

func ParseHoldRequesters(value string) []string {
	parts := strings.Split(value, ",")
	requesters := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		requesters = append(requesters, part)
	}
	return requesters
}

func holdRequesterSet(requesters []string) map[string]struct{} {
	out := make(map[string]struct{}, len(requesters))
	for _, requester := range requesters {
		requester = strings.TrimSpace(requester)
		if requester != "" {
			out[requester] = struct{}{}
		}
	}
	return out
}

func (a *Agent) isHoldRequester(agentID string) bool {
	_, ok := a.holdRequesters[strings.TrimSpace(agentID)]
	return ok
}

func holdServiceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(err.Error()), "conflict") {
		return "conflict"
	}
	return ""
}

func isCancelledHold(event StoredEvent) bool {
	return strings.HasPrefix(strings.TrimSpace(event.Summary), CancelledPrefix)
}
