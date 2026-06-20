package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

// travelHarness wraps schedulerHarness with travel knowledge loaded from the
// repo seed files and a handle on the telemetry registry for metric
// assertions.
type travelHarness struct {
	*schedulerHarness
	metrics *telemetry.Registry
}

func newTravelHarness(t *testing.T, now time.Time, edit func(*Config), onSend func(upstreamMessage) (string, bool)) *travelHarness {
	t.Helper()
	h := &schedulerHarness{t: t, onSend: onSend}
	h.server = httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(h.server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := Config{
		BusURL:          h.server.URL,
		AgentID:         DefaultAgentID,
		Secret:          "secret",
		UpstreamTimeout: 200 * time.Millisecond,
		Now:             func() time.Time { return now },
		LocationsPath:   "../../data/locations.json",
		VenuesPath:      "../../data/venues.json",
	}
	if edit != nil {
		edit(&cfg)
	}
	metrics := telemetry.New("scheduler-travel-test")
	h.agent = NewAgent(cfg, metrics)
	h.agent.startWorkers(ctx)
	return &travelHarness{schedulerHarness: h, metrics: metrics}
}

func (h *travelHarness) writeMessages() []upstreamMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []upstreamMessage
	for _, msg := range h.upstream {
		if msg.To == DefaultCalendarWriteAgent {
			out = append(out, msg)
		}
	}
	return out
}

func (h *travelHarness) clearUpstream() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upstream = nil
}

func metaRequestID(msg upstreamMessage) string {
	if msg.Meta == nil {
		return ""
	}
	value, _ := msg.Meta["request_id"].(string)
	return value
}

func decodeWriteRequest(t *testing.T, msg upstreamMessage) outlookcalendarwrite.Request {
	t.Helper()
	var req outlookcalendarwrite.Request
	if err := json.Unmarshal([]byte(msg.Body), &req); err != nil {
		t.Fatalf("decode write request: %v body=%s", err, msg.Body)
	}
	return req
}

func metricValue(t *testing.T, metrics *telemetry.Registry, name string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.HandleMetrics(rec, nil)
	scanner := bufio.NewScanner(rec.Body)
	prefix := "email_agents_" + name + "_total{"
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		return value
	}
	return 0
}

func metricGaugeValue(t *testing.T, metrics *telemetry.Registry, name string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.HandleMetrics(rec, nil)
	scanner := bufio.NewScanner(rec.Body)
	prefix := "email_agents_" + name + "{"
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		return value
	}
	return 0
}

func lat(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, loadLocation())
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// wevt builds a timed busy calendar event for watcher/booking tests with an
// entry_id extended property by default.
func wevt(id, summary string, start, end time.Time, mods ...func(*calendarread.Event)) calendarread.Event {
	event := calendarread.Event{
		ID:      id,
		Summary: summary,
		Start:   calendarread.EventDateTime{DateTime: formatLA(start), TimeZone: DefaultTimeZone},
		End:     calendarread.EventDateTime{DateTime: formatLA(end), TimeZone: DefaultTimeZone},
		ExtendedProperties: &calendarread.ExtendedProperties{Private: map[string]string{
			"entry_id": "entry-" + id,
		}},
	}
	for _, mod := range mods {
		mod(&event)
	}
	return event
}

func withLocation(location string) func(*calendarread.Event) {
	return func(e *calendarread.Event) { e.Location = location }
}

func withCategories(categories ...string) func(*calendarread.Event) {
	return func(e *calendarread.Event) { e.Categories = categories }
}

func withTransparency(value string) func(*calendarread.Event) {
	return func(e *calendarread.Event) { e.Transparency = value }
}

func withAttendees(attendees ...calendarread.EventAttendee) func(*calendarread.Event) {
	return func(e *calendarread.Event) { e.Attendees = attendees }
}

func withoutEntryID() func(*calendarread.Event) {
	return func(e *calendarread.Event) { delete(e.ExtendedProperties.Private, "entry_id") }
}

func asAllDay(date string) func(*calendarread.Event) {
	return func(e *calendarread.Event) {
		e.Start = calendarread.EventDateTime{Date: date}
		end, _ := time.ParseInLocation("2006-01-02", date, loadLocation())
		e.End = calendarread.EventDateTime{Date: end.AddDate(0, 0, 1).Format("2006-01-02")}
	}
}

// calendarByDate answers read-agent events-list requests from a date-keyed
// map ("2006-01-02" local). writeFn handles write-agent requests.
func calendarByDate(t *testing.T, events map[string][]calendarread.Event, writeFn func(upstreamMessage) (string, bool)) func(upstreamMessage) (string, bool) {
	return func(msg upstreamMessage) (string, bool) {
		switch msg.To {
		case DefaultCalendarReadAgent:
			var req calendarread.Request
			if err := json.Unmarshal([]byte(msg.Body), &req); err != nil {
				t.Errorf("decode read request: %v", err)
				return calendarResponse(nil), true
			}
			min, err := time.Parse(time.RFC3339, req.Query.TimeMin)
			if err != nil {
				t.Errorf("parse TimeMin %q: %v", req.Query.TimeMin, err)
				return calendarResponse(nil), true
			}
			date := min.In(loadLocation()).Format("2006-01-02")
			return calendarResponse(events[date]), true
		case DefaultCalendarWriteAgent:
			if writeFn != nil {
				return writeFn(msg)
			}
			return writeSuccessFromRequest(t, msg.Body, "evt-write"), true
		default:
			return "", false
		}
	}
}

// echoWriteSuccess answers every write with a success echo whose event id is
// derived from the payload class (hold/before/after/cancel) for easy
// assertions.
func echoWriteSuccess(t *testing.T) func(upstreamMessage) (string, bool) {
	return func(msg upstreamMessage) (string, bool) {
		req := decodeWriteRequest(t, msg)
		id := "evt-write"
		if req.Event.Summary != nil {
			summary := *req.Event.Summary
			switch {
			case strings.HasPrefix(summary, "Joel + "):
				id = "evt-hold"
			case strings.Contains(summary, "(for "):
				id = "evt-before"
			case strings.Contains(summary, "(return "):
				id = "evt-after"
			}
		}
		if req.Action == "event-patch" {
			id = strings.TrimSpace(req.EventID)
		}
		return writeSuccessFromRequest(t, msg.Body, id), true
	}
}

func offsiteRequestBody(requestID, window string, duration int, location string) string {
	blob, _ := json.Marshal(Request{
		Action:          CapabilityRequest,
		RequestID:       requestID,
		Purpose:         "Pick up Marc after procedure",
		RequesterLabel:  "Fable",
		DurationMinutes: duration,
		Window:          window,
		Agenda:          "1. Drive over\n2. Pick up",
		Location:        location,
	})
	return string(blob)
}
