package outlookcalendar

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/joelkehle/calendar-agents/internal/busagent"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

type AgentConfig struct {
	BusURL      string
	AgentID     string
	Secret      string
	PollWaitSec int
}

type Agent struct {
	cfg       AgentConfig
	extractor Extractor
	loop      *busagent.Loop
	metrics   *telemetry.Registry
}

func NewAgent(cfg AgentConfig, extractor Extractor, metrics *telemetry.Registry) *Agent {
	a := &Agent{
		cfg:       cfg,
		extractor: extractor,
		metrics:   metrics,
	}
	a.loop = busagent.New(busagent.LoopConfig{
		BusURL:       cfg.BusURL,
		AgentID:      cfg.AgentID,
		Secret:       cfg.Secret,
		Capabilities: []string{"calendar-list", "events-list", "calendar-agenda", "outlook-calendar"},
		Description:  "Read-only Outlook calendar source agent for Joel Kehle. Runs on the Windows laptop and answers agenda/event requests from a Pinakes bus.",
		PollWaitSec:  cfg.PollWaitSec,
		Metrics:      metrics,
		HandleEvent:  a.handleEvent,
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
	if err := a.loop.Client().Ack(ctx, a.cfg.AgentID, a.cfg.Secret, evt.MessageID, "accepted", "outlook calendar request received"); err != nil {
		return err
	}

	var req Request
	if err := json.Unmarshal([]byte(evt.Body), &req); err != nil {
		return a.sendError(ctx, evt, fmt.Sprintf("invalid outlook calendar request: %v", err))
	}

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "calendar-list":
		a.metrics.IncCounter("calendar_list")
		return a.sendJSON(ctx, evt, calendarread.CalendarListResponse{Calendars: []calendarread.Calendar{{
			ID:          "outlook-primary",
			Summary:     "Outlook Calendar",
			Description: "Primary Outlook calendar on Joel's Windows laptop",
			TimeZone:    DefaultTimeZone,
			AccessRole:  "reader",
			Primary:     true,
		}}})
	case "events-list", "calendar-agenda", "agenda":
		events, err := a.extractor.ListEvents(req.Query)
		if err != nil {
			return a.sendError(ctx, evt, err.Error())
		}
		a.metrics.IncCounter("events_list")
		return a.sendJSON(ctx, evt, calendarread.EventsListResponse{Events: events})
	default:
		return a.sendError(ctx, evt, "unsupported outlook calendar action")
	}
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
	a.metrics.IncCounter("errors")
	message = strings.TrimSpace(message)
	log.Printf("outlook calendar request failed action=%s from=%s conversation_id=%s err=%s", extractAction(evt.Body), evt.From, evt.ConversationID, message)
	return a.sendJSON(ctx, evt, map[string]any{"error": message})
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
