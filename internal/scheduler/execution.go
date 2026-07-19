package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

func (a *Agent) executeJob(ctx context.Context, job schedulerJob) Reply {
	session := a.newUpstreamSession(job.key)
	defer a.closeUpstreamSession(session)

	switch strings.ToLower(strings.TrimSpace(job.req.Action)) {
	case CapabilityRequest:
		return a.executeRequest(ctx, session, job)
	case CapabilityMove:
		return a.executeMove(ctx, session, job)
	case CapabilityCancel:
		return a.executeCancel(ctx, session, job)
	default:
		return errorReply(job.req.RequestID, ErrorInvalidRequest, "unsupported scheduler action")
	}
}

func (a *Agent) executeRequest(ctx context.Context, session upstreamSession, job schedulerJob) Reply {
	req := job.req
	now := a.now()
	windows, err := ResolveWindow(req.Window, req.Earliest, req.Latest, now)
	if err != nil {
		return replyFromRequestError(req.RequestID, err)
	}
	duration, err := requestDuration(req.DurationMinutes, true)
	if err != nil {
		return errorReply(req.RequestID, ErrorInvalidRequest, err.Error())
	}
	// Offsite branch (SCHEDULER_TRAVEL_SPEC §4-§6). Absent/empty location, an
	// office location, or a virtual location follow the existing path
	// byte-for-byte (no travel field). A location with no travel knowledge
	// must not silently book without travel protection.
	location := strings.TrimSpace(req.Location)
	if location != "" {
		if a.knowledge == nil {
			return errorReply(req.RequestID, ErrorEstimateUnavailable, "travel knowledge is not loaded")
		}
		if !a.knowledge.IsOffice(location) && !a.knowledge.IsVirtual(location) {
			return a.executeOffsiteRequest(ctx, session, job, windows, duration, location)
		}
	}
	events, err := a.fetchCalendarEvents(ctx, session, windows, "read")
	if err != nil {
		a.recordCalendarWorkBlocked("read")
		return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
	}
	summary := holdSummary(req, job.evt.From)
	var extraBusy []BusyInterval
	for attempt := 0; attempt < 3; attempt++ {
		slot, ok := SelectSlotWithBusy(windows, events, extraBusy, now, duration)
		if !ok {
			return a.infeasibleReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration)
		}
		resp, err := a.writeHoldInsert(ctx, session, req, summary, slot)
		if err != nil {
			return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
		}
		if strings.TrimSpace(resp.Error) == "" {
			return bookedReply(req.RequestID, StatusBooked, summary, slot, resp)
		}
		if resp.ErrorCode == "conflict" {
			extraBusy = append(extraBusy, conflictBusy(resp.Error, slot)...)
			continue
		}
		return writerRefusedReply(req.RequestID, resp, false)
	}
	return a.infeasibleReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration)
}

func (a *Agent) executeMove(ctx context.Context, session upstreamSession, job schedulerJob) Reply {
	req := job.req
	now := a.now()
	windows, err := ResolveWindow(req.Window, req.Earliest, req.Latest, now)
	if err != nil {
		return replyFromRequestError(req.RequestID, err)
	}
	duration, err := requestDuration(req.DurationMinutes, false)
	if err != nil {
		return errorReply(req.RequestID, ErrorInvalidRequest, err.Error())
	}
	events, err := a.fetchCalendarEvents(ctx, session, windows, "read-move")
	if err != nil {
		a.recordCalendarWorkBlocked("read")
		return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
	}
	var extraBusy []BusyInterval
	for attempt := 0; attempt < 3; attempt++ {
		slot, ok := SelectSlotWithBusy(windows, events, extraBusy, now, duration)
		if !ok {
			return a.infeasibleReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration)
		}
		resp, err := a.writeHoldMove(ctx, session, req, slot)
		if err != nil {
			return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
		}
		if strings.TrimSpace(resp.Error) == "" {
			return bookedReply(req.RequestID, StatusMoved, "", slot, resp)
		}
		if resp.ErrorCode == ErrorNotOwned {
			return writerRefusedReply(req.RequestID, resp, true)
		}
		if resp.ErrorCode == "conflict" {
			extraBusy = append(extraBusy, conflictBusy(resp.Error, slot)...)
			continue
		}
		return writerRefusedReply(req.RequestID, resp, false)
	}
	return a.infeasibleReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration)
}

func (a *Agent) executeCancel(ctx context.Context, session upstreamSession, job schedulerJob) Reply {
	resp, err := a.writeHoldCancel(ctx, session, job.req)
	if err != nil {
		return errorReply(job.req.RequestID, ErrorUpstreamUnavailable, err.Error())
	}
	if strings.TrimSpace(resp.Error) != "" {
		return writerRefusedReply(job.req.RequestID, resp, resp.ErrorCode == ErrorNotOwned)
	}
	slot := slotFromWriter(resp)
	reply := Reply{
		Status:    StatusCancelled,
		RequestID: job.req.RequestID,
		EventID:   writerEventID(resp),
		Summary:   writerSummary(resp),
	}
	if slot.Start != "" {
		reply.Start = slot.Start
		reply.End = slot.End
	}
	return reply
}

func replyFromRequestError(requestID string, err error) Reply {
	var reqErr requestError
	if errors.As(err, &reqErr) {
		return errorReply(requestID, reqErr.Code, reqErr.Message)
	}
	return errorReply(requestID, ErrorInvalidRequest, err.Error())
}

func (a *Agent) infeasibleReply(ctx context.Context, session upstreamSession, requestID string, windows []Interval, events []calendarread.Event, extraBusy []BusyInterval, now time.Time, duration time.Duration) Reply {
	reply := Reply{Status: StatusInfeasible, RequestID: requestID}
	altWindows := alternativeWindows(windows)
	if len(altWindows) == 0 {
		return reply
	}
	altEvents := append([]calendarread.Event{}, events...)
	if fetched, err := a.fetchCalendarEvents(ctx, session, altWindows, "read-alternative"); err == nil {
		altEvents = append(altEvents, fetched...)
	}
	if alt, ok := SelectSlotWithBusy(altWindows, altEvents, extraBusy, now, duration); ok {
		reply.NearestAlternative = &alt
	}
	return reply
}

func (a *Agent) fetchCalendarEvents(ctx context.Context, session upstreamSession, windows []Interval, step string) ([]calendarread.Event, error) {
	dates := datesForWindows(windows)
	events := make([]calendarread.Event, 0)
	for _, date := range dates {
		req := calendarread.Request{
			Action: "events-list",
			Query: calendarread.EventsQuery{
				TimeMin:      date.Start.Format(time.RFC3339),
				TimeMax:      date.End.Format(time.RFC3339),
				SingleEvents: true,
				OrderBy:      "startTime",
				MaxResults:   250,
			},
		}
		raw, err := a.requestUpstream(ctx, session, a.cfg.CalendarReadAgent, step+"-"+date.Start.Format("20060102"), req, "")
		if err != nil {
			return nil, err
		}
		var resp calendarread.EventsListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			a.recordCalendarDependencyError(a.cfg.CalendarReadAgent)
			return nil, fmt.Errorf("decode calendar response: %w", err)
		}
		if strings.TrimSpace(resp.Error) != "" {
			a.recordCalendarDependencyError(a.cfg.CalendarReadAgent)
			return nil, errors.New(resp.Error)
		}
		events = append(events, resp.Events...)
	}
	return events, nil
}

func datesForWindows(windows []Interval) []Interval {
	loc := loadLocation()
	seen := make(map[string]bool)
	var out []Interval
	for _, w := range windows {
		startDate := localDateStart(w.Start, loc)
		endCursor := w.End.Add(-time.Nanosecond)
		endDate := localDateStart(endCursor, loc)
		for date := startDate; !date.After(endDate); date = date.AddDate(0, 0, 1) {
			key := date.Format("2006-01-02")
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Interval{Start: date, End: date.AddDate(0, 0, 1)})
		}
	}
	sortIntervals(out)
	return out
}

func (a *Agent) writeHoldInsert(ctx context.Context, session upstreamSession, req Request, summary string, slot Slot) (outlookcalendarwrite.MutationResponse, error) {
	return a.writeRequest(ctx, session, "insert", holdInsertRequest(req, summary, slot))
}

func holdInsertRequest(req Request, summary string, slot Slot) outlookcalendarwrite.Request {
	showAs := "busy"
	return outlookcalendarwrite.Request{
		Action:     "event-insert",
		CalendarID: "default",
		Event: outlookcalendarwrite.EventInput{
			Summary:     &summary,
			Description: &req.Agenda,
			Start:       writerDateTime(slot.Start),
			End:         writerDateTime(slot.End),
			ShowAs:      &showAs,
		},
	}
}

func (a *Agent) writeHoldMove(ctx context.Context, session upstreamSession, req Request, slot Slot) (outlookcalendarwrite.MutationResponse, error) {
	body := outlookcalendarwrite.Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    strings.TrimSpace(req.EventID),
		Event: outlookcalendarwrite.EventInput{
			Start: writerDateTime(slot.Start),
			End:   writerDateTime(slot.End),
		},
	}
	return a.writeRequest(ctx, session, "move", body)
}

func (a *Agent) writeHoldCancel(ctx context.Context, session upstreamSession, req Request) (outlookcalendarwrite.MutationResponse, error) {
	summary := outlookcalendarwrite.CancelledPrefix
	showAs := "free"
	body := outlookcalendarwrite.Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    strings.TrimSpace(req.EventID),
		Event: outlookcalendarwrite.EventInput{
			Summary: &summary,
			ShowAs:  &showAs,
		},
	}
	return a.writeRequest(ctx, session, "cancel", body)
}

func (a *Agent) writeRequest(ctx context.Context, session upstreamSession, step string, payload any) (outlookcalendarwrite.MutationResponse, error) {
	return a.writeMutationWithKey(ctx, session, step, payload, "sched-"+session.keyHash+"-"+step)
}

// writeMutationWithKey sends a write-agent mutation with an EXPLICIT
// meta.request_id (writer idempotency key). The watcher and the offsite
// booking path's replay/compensation steps need keys that are not derived
// from the session hash + step (SCHEDULER_TRAVEL_SPEC §7.1, review nit N5).
func (a *Agent) writeMutationWithKey(ctx context.Context, session upstreamSession, step string, payload any, metaRequestID string) (outlookcalendarwrite.MutationResponse, error) {
	raw, err := a.requestUpstream(ctx, session, a.cfg.CalendarWriteAgent, step, payload, metaRequestID)
	if err != nil {
		a.recordCalendarWorkBlocked("write")
		return outlookcalendarwrite.MutationResponse{}, err
	}
	var resp outlookcalendarwrite.MutationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		a.recordCalendarDependencyError(a.cfg.CalendarWriteAgent)
		a.recordCalendarWorkBlocked("write")
		return outlookcalendarwrite.MutationResponse{}, fmt.Errorf("decode writer response: %w", err)
	}
	if strings.TrimSpace(resp.Error) != "" {
		a.metrics.IncCounter("schedule_calendar_write_dependency_refusals")
	}
	return resp, nil
}

func (a *Agent) requestUpstream(ctx context.Context, session upstreamSession, target, step string, payload any, metaRequestID string) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		a.recordCalendarDependencyRequest(target)
		callCtx, cancel := context.WithTimeout(ctx, a.cfg.UpstreamTimeout)
		requestID := "sched-" + session.keyHash + "-" + step + fmt.Sprintf("-%d", attempt+1)
		meta := map[string]any(nil)
		if strings.TrimSpace(metaRequestID) != "" {
			meta = map[string]any{"request_id": metaRequestID}
		}
		if _, err := a.loop.Client().SendMessage(callCtx, a.cfg.AgentID, a.cfg.Secret, target, session.conversationID, requestID, "request", string(body), nil, meta); err != nil {
			cancel()
			a.recordCalendarDependencyError(target)
			lastErr = err
			continue
		}
		a.metrics.IncCounter("schedule_upstream_requests")
		select {
		case evt := <-session.responses:
			cancel()
			if strings.TrimSpace(target) != "" && evt.From != target {
				attempt--
				continue
			}
			a.recordCalendarDependencySuccess(target)
			return json.RawMessage(evt.Body), nil
		case <-callCtx.Done():
			cancel()
			a.recordCalendarDependencyError(target)
			lastErr = callCtx.Err()
		}
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return nil, lastErr
}

func (a *Agent) recordCalendarDependencyRequest(target string) {
	switch a.calendarDependencyKind(target) {
	case "read":
		a.metrics.IncCounter("schedule_calendar_read_dependency_requests")
	case "write":
		a.metrics.IncCounter("schedule_calendar_write_dependency_requests")
	}
}

func (a *Agent) recordCalendarDependencySuccess(target string) {
	switch a.calendarDependencyKind(target) {
	case "read":
		a.metrics.SetGauge("schedule_calendar_read_dependency_available", 1)
		a.metrics.SetGauge("schedule_calendar_read_work_blocked", 0)
	case "write":
		a.metrics.SetGauge("schedule_calendar_write_dependency_available", 1)
		a.metrics.SetGauge("schedule_calendar_write_work_blocked", 0)
	}
}

func (a *Agent) recordCalendarWorkBlocked(kind string) {
	switch kind {
	case "read":
		a.metrics.IncCounter("schedule_calendar_read_work_blocked")
		a.metrics.SetGauge("schedule_calendar_read_work_blocked", 1)
	case "write":
		a.metrics.IncCounter("schedule_calendar_write_work_blocked")
		a.metrics.SetGauge("schedule_calendar_write_work_blocked", 1)
	}
}

func (a *Agent) recordCalendarDependencyError(target string) {
	switch a.calendarDependencyKind(target) {
	case "read":
		a.metrics.IncCounter("schedule_calendar_read_dependency_errors")
		a.metrics.SetGauge("schedule_calendar_read_dependency_available", 0)
	case "write":
		a.metrics.IncCounter("schedule_calendar_write_dependency_errors")
		a.metrics.SetGauge("schedule_calendar_write_dependency_available", 0)
	}
}

func (a *Agent) calendarDependencyKind(target string) string {
	target = strings.TrimSpace(target)
	switch {
	case target != "" && target == strings.TrimSpace(a.cfg.CalendarReadAgent):
		return "read"
	case target != "" && target == strings.TrimSpace(a.cfg.CalendarWriteAgent):
		return "write"
	default:
		return ""
	}
}

func (a *Agent) newUpstreamSession(key string) upstreamSession {
	hash := idempotencyHash(key)
	conversationID := "sched-" + hash
	ch := make(chan busclient.InboxEvent, 8)
	a.mu.Lock()
	a.routes[conversationID] = ch
	a.mu.Unlock()
	return upstreamSession{conversationID: conversationID, keyHash: hash, responses: ch}
}

func (a *Agent) closeUpstreamSession(session upstreamSession) {
	a.mu.Lock()
	delete(a.routes, session.conversationID)
	a.mu.Unlock()
}

func idempotencyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func writerDateTime(value string) *outlookcalendarwrite.EventDateTime {
	return &outlookcalendarwrite.EventDateTime{DateTime: value, TimeZone: DefaultTimeZone}
}

func holdSummary(req Request, sender string) string {
	label := strings.TrimSpace(req.RequesterLabel)
	if label == "" {
		label = strings.TrimSpace(sender)
	}
	return "Joel + " + label + ": " + strings.TrimSpace(req.Purpose)
}

func bookedReply(requestID, status, fallbackSummary string, slot Slot, resp outlookcalendarwrite.MutationResponse) Reply {
	return Reply{
		Status:    status,
		RequestID: requestID,
		EventID:   writerEventID(resp),
		Start:     firstNonEmpty(writerStart(resp), slot.Start),
		End:       firstNonEmpty(writerEnd(resp), slot.End),
		Summary:   firstNonEmpty(writerSummary(resp), fallbackSummary),
	}
}

func writerRefusedReply(requestID string, resp outlookcalendarwrite.MutationResponse, passthroughNotOwned bool) Reply {
	code := strings.TrimSpace(resp.ErrorCode)
	message := strings.TrimSpace(resp.Error)
	if passthroughNotOwned && code == ErrorNotOwned {
		return errorReply(requestID, ErrorNotOwned, message)
	}
	prefix := "writer"
	if code != "" {
		prefix += ": " + code
	}
	if message != "" {
		message = prefix + ": " + message
	} else {
		message = prefix
	}
	return errorReply(requestID, ErrorBookingRefused, message)
}

func writerEventID(resp outlookcalendarwrite.MutationResponse) string {
	if resp.Event != nil {
		return strings.TrimSpace(resp.Event.ID)
	}
	if resp.WouldWrite != nil {
		return strings.TrimSpace(resp.WouldWrite.ID)
	}
	return ""
}

func writerSummary(resp outlookcalendarwrite.MutationResponse) string {
	if resp.Event != nil {
		return strings.TrimSpace(resp.Event.Summary)
	}
	if resp.WouldWrite != nil {
		return strings.TrimSpace(resp.WouldWrite.Summary)
	}
	return ""
}

func writerStart(resp outlookcalendarwrite.MutationResponse) string {
	if resp.Event != nil {
		return strings.TrimSpace(resp.Event.Start.DateTime)
	}
	if resp.WouldWrite != nil {
		return strings.TrimSpace(resp.WouldWrite.Start.DateTime)
	}
	return ""
}

func writerEnd(resp outlookcalendarwrite.MutationResponse) string {
	if resp.Event != nil {
		return strings.TrimSpace(resp.Event.End.DateTime)
	}
	if resp.WouldWrite != nil {
		return strings.TrimSpace(resp.WouldWrite.End.DateTime)
	}
	return ""
}

func slotFromWriter(resp outlookcalendarwrite.MutationResponse) Slot {
	return Slot{Start: writerStart(resp), End: writerEnd(resp)}
}

func conflictBusy(message string, fallback Slot) []BusyInterval {
	ranges := parseConflictRanges(message)
	if len(ranges) > 0 {
		return ranges
	}
	start, startErr := parseEventTime(fallback.Start)
	end, endErr := parseEventTime(fallback.End)
	if startErr == nil && endErr == nil && start.Before(end) {
		return []BusyInterval{{Start: start.In(loadLocation()), End: end.In(loadLocation())}}
	}
	return nil
}

func parseConflictRanges(message string) []BusyInterval {
	message = strings.TrimSpace(message)
	if idx := strings.Index(message, ":"); idx >= 0 {
		message = message[idx+1:]
	}
	parts := strings.Split(message, ",")
	out := make([]BusyInterval, 0, len(parts))
	for _, part := range parts {
		bounds := strings.Split(strings.TrimSpace(part), "/")
		if len(bounds) != 2 {
			continue
		}
		start, startErr := parseEventTime(bounds[0])
		end, endErr := parseEventTime(bounds[1])
		if startErr != nil || endErr != nil || !start.Before(end) {
			continue
		}
		out = append(out, BusyInterval{Start: start.In(loadLocation()), End: end.In(loadLocation())})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// --- Offsite booking (SCHEDULER_TRAVEL_SPEC §5-§6) ---------------------------

// travelInserted tracks one event written during an offsite booking so a
// failure later in the sequence can compensate (cancel-patch) it.
type travelInserted struct {
	step    string // writer step name without the "sched-<hash>-" prefix
	eventID string
}

// executeOffsiteRequest books a working hold plus BOTH flanking travel blocks
// for a schedule-request carrying a non-office, non-virtual location.
// "Atomic" means: either the hold and both travel blocks exist, or none do —
// implemented as ordered inserts with compensating cancels, since Outlook
// offers no transactions. A crash between steps can strand events until the
// watcher tick reconciles or a cancel-patch compensation runs on retry; §6.4
// spells out the exact retry semantics (see the replay guard below).
func (a *Agent) executeOffsiteRequest(ctx context.Context, session upstreamSession, job schedulerJob, windows []Interval, duration time.Duration, location string) Reply {
	req := job.req
	now := a.now()
	events, err := a.fetchCalendarEvents(ctx, session, windows, "read")
	if err != nil {
		a.recordCalendarWorkBlocked("read")
		return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
	}
	summary := holdSummary(req, job.evt.From)
	blockFor := func(start time.Time) (int, error) {
		estimate, err := a.knowledge.Estimate(start, location)
		if err != nil {
			return 0, err
		}
		return travelknowledge.BlockMinutes(estimate), nil
	}

	var extraBusy []BusyInterval
	for attempt := 0; attempt < 3; attempt++ {
		slot, blockMinutes, ok, selErr := SelectSlotWithTravel(windows, events, extraBusy, now, duration, blockFor)
		if selErr != nil {
			return errorReply(req.RequestID, ErrorEstimateUnavailable, "no origin known for the requested dates")
		}
		if !ok {
			return a.infeasibleTravelReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration, blockFor)
		}
		slotStart, startErr := parseEventTime(slot.Start)
		slotEnd, endErr := parseEventTime(slot.End)
		if startErr != nil || endErr != nil {
			return errorReply(req.RequestID, ErrorInvalidRequest, "selected slot has invalid bounds")
		}
		estimate, estErr := a.knowledge.Estimate(slotStart, location)
		if estErr != nil {
			return errorReply(req.RequestID, ErrorEstimateUnavailable, "no origin known for the requested dates")
		}

		// Step 1: insert the HOLD (existing path, conflict-retry unchanged).
		resp, err := a.writeHoldInsert(ctx, session, req, summary, slot)
		if err != nil {
			return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
		}
		if resp.ErrorCode == "conflict" {
			extraBusy = append(extraBusy, conflictBusy(resp.Error, slot)...)
			continue
		}
		if strings.TrimSpace(resp.Error) != "" {
			return writerRefusedReply(req.RequestID, resp, false)
		}
		// Replay guard (§6.1/§6.4): a replayed insert success may describe a
		// hold that a previous attempt's compensation has since cancelled.
		// Verify it; missing/cancelled => one re-insert under the alternate
		// key; a second replayed-and-missing fails the request.
		if resp.Replayed {
			live, verifyErr := a.holdPresent(ctx, session, summary, slot)
			if verifyErr != nil {
				return errorReply(req.RequestID, ErrorUpstreamUnavailable, verifyErr.Error())
			}
			if !live {
				a.metrics.IncCounter("schedule_travel_replay_reinserts")
				resp, err = a.writeMutationWithKey(ctx, session, "insert-r2", holdInsertRequest(req, summary, slot), "sched-"+session.keyHash+"-insert-r2")
				if err != nil {
					return errorReply(req.RequestID, ErrorUpstreamUnavailable, err.Error())
				}
				if strings.TrimSpace(resp.Error) != "" {
					return travelBookingFailedReply(req.RequestID, "hold re-insert", resp, false)
				}
				if resp.Replayed {
					liveRetry, retryErr := a.holdPresent(ctx, session, summary, slot)
					if retryErr != nil {
						return errorReply(req.RequestID, ErrorUpstreamUnavailable, retryErr.Error())
					}
					if !liveRetry {
						return travelBookingFailedReply(req.RequestID, "hold re-insert", resp, true)
					}
				}
			}
		}
		holdID := writerEventID(resp)
		inserted := []travelInserted{{step: "insert", eventID: holdID}}

		dest := destinationLabel(estimate, location)
		parking := strings.TrimSpace(estimate.Parking)
		if parking == "" {
			parking = "unknown"
		}
		block := time.Duration(blockMinutes) * time.Minute

		// Step 2: BEFORE travel block.
		beforeLocation := travelBlockLocation(estimate, location, "for")
		beforeReq := travelInsertRequest(dest, beforeLocation, parking, holdID, slot.Start, slotStart, slotStart.Add(-block), slotStart, "for")
		beforeResp, err := a.writeRequest(ctx, session, "travel-before", beforeReq)
		if err == nil && strings.TrimSpace(beforeResp.Error) == "" && beforeResp.Replayed {
			beforeResp, err = a.reverifyTravelStep(ctx, session, "travel-before", beforeReq, beforeResp)
		}
		if failed, reply := a.travelStepFailed(ctx, session, req.RequestID, "travel-before", beforeResp, err, inserted); failed {
			return reply
		}
		inserted = append(inserted, travelInserted{step: "travel-before", eventID: writerEventID(beforeResp)})

		// Step 3: AFTER travel block.
		afterLocation := travelBlockLocation(estimate, location, "return")
		afterReq := travelInsertRequest(dest, afterLocation, parking, holdID, slot.Start, slotStart, slotEnd, slotEnd.Add(block), "return")
		afterResp, err := a.writeRequest(ctx, session, "travel-after", afterReq)
		if err == nil && strings.TrimSpace(afterResp.Error) == "" && afterResp.Replayed {
			afterResp, err = a.reverifyTravelStep(ctx, session, "travel-after", afterReq, afterResp)
		}
		if failed, reply := a.travelStepFailed(ctx, session, req.RequestID, "travel-after", afterResp, err, inserted); failed {
			return reply
		}

		a.metrics.IncCounter("schedule_travel_bookings")
		reply := bookedReply(req.RequestID, StatusBooked, summary, slot, resp)
		reply.Travel = &TravelBooking{
			Minutes:        estimate.Minutes,
			OriginID:       estimate.OriginID,
			EstimateSource: estimate.Source,
			Before: &TravelLeg{
				EventID: writerEventID(beforeResp),
				Start:   firstNonEmpty(writerStart(beforeResp), formatLA(slotStart.Add(-block))),
				End:     firstNonEmpty(writerEnd(beforeResp), slot.Start),
			},
			After: &TravelLeg{
				EventID: writerEventID(afterResp),
				Start:   firstNonEmpty(writerStart(afterResp), slot.End),
				End:     firstNonEmpty(writerEnd(afterResp), formatLA(slotEnd.Add(block))),
			},
			Notes: []string{},
		}
		return reply
	}
	return a.infeasibleTravelReply(ctx, session, req.RequestID, windows, events, extraBusy, now, duration, blockFor)
}

// reverifyTravelStep extends the §6.1 replay guard to travel legs: a
// replayed travel-insert success may describe a block that a previous
// attempt's compensation has since cancelled. Verify it against the live
// calendar (same summary+start+end predicate as holdPresent); when missing,
// re-insert once under the step's -r2 key. The returned response replaces
// the replayed one and flows into the normal travelStepFailed handling.
func (a *Agent) reverifyTravelStep(ctx context.Context, session upstreamSession, step string, payload outlookcalendarwrite.Request, resp outlookcalendarwrite.MutationResponse) (outlookcalendarwrite.MutationResponse, error) {
	summary := ""
	if payload.Event.Summary != nil {
		summary = *payload.Event.Summary
	}
	if payload.Event.Start == nil || payload.Event.End == nil {
		return resp, fmt.Errorf("verify replayed %s: travel request missing times", step)
	}
	live, err := a.holdPresent(ctx, session, summary, Slot{Start: payload.Event.Start.DateTime, End: payload.Event.End.DateTime})
	if err != nil {
		return resp, fmt.Errorf("verify replayed %s: %w", step, err)
	}
	if live {
		return resp, nil
	}
	a.metrics.IncCounter("schedule_travel_replay_reinserts")
	return a.writeMutationWithKey(ctx, session, step+"-r2", payload, "sched-"+session.keyHash+"-"+step+"-r2")
}

// travelStepFailed inspects a travel-block insert outcome; on failure it
// compensates (cancel-patches every event inserted so far, in reverse order)
// and produces the terminal travel_booking_failed reply. A conflict on a
// travel insert does NOT loop back into hold re-selection in v1 (documented
// limitation): the whole request fails and the caller may retry with a fresh
// request_id.
func (a *Agent) travelStepFailed(ctx context.Context, session upstreamSession, requestID, step string, resp outlookcalendarwrite.MutationResponse, err error, inserted []travelInserted) (bool, Reply) {
	if err == nil && strings.TrimSpace(resp.Error) == "" {
		return false, Reply{}
	}
	compensated := a.compensateTravelBooking(ctx, session, inserted)
	if err != nil {
		message := fmt.Sprintf("travel booking failed at step %s: %v", step, err)
		if !compensated {
			message += " (compensation incomplete: orphaned events may remain on the calendar)"
		}
		return true, errorReply(requestID, ErrorTravelBooking, message)
	}
	return true, travelBookingFailedReply(requestID, step, resp, !compensated)
}

// compensateTravelBooking cancel-patches every event inserted so far in this
// request (reverse insertion order: after block, before block, hold), each
// with its own idempotency key. Returns false when any compensation cancel
// failed (orphaned events may remain; they are Joel-visible by design).
func (a *Agent) compensateTravelBooking(ctx context.Context, session upstreamSession, inserted []travelInserted) bool {
	a.metrics.IncCounter("schedule_travel_compensations")
	allOK := true
	for i := len(inserted) - 1; i >= 0; i-- {
		item := inserted[i]
		if strings.TrimSpace(item.eventID) == "" {
			allOK = false
			a.metrics.IncCounter("schedule_travel_compensation_failed")
			continue
		}
		step := item.step + "-cancel"
		resp, err := a.writeMutationWithKey(ctx, session, step, cancelPatchRequest(item.eventID), "sched-"+session.keyHash+"-"+step)
		if err != nil || strings.TrimSpace(resp.Error) != "" {
			allOK = false
			a.metrics.IncCounter("schedule_travel_compensation_failed")
			log.Printf("%s travel compensation cancel failed step=%s event_id=%s err=%v writer_error=%s", a.cfg.AgentID, step, item.eventID, err, resp.Error)
		}
	}
	return allOK
}

func travelBookingFailedReply(requestID, step string, resp outlookcalendarwrite.MutationResponse, orphansPossible bool) Reply {
	message := fmt.Sprintf("travel booking failed at step %s", step)
	if code := strings.TrimSpace(resp.ErrorCode); code != "" {
		message += ": writer: " + code
	}
	if writerMessage := strings.TrimSpace(resp.Error); writerMessage != "" {
		message += ": " + writerMessage
	}
	if orphansPossible {
		message += " (compensation incomplete: orphaned events may remain on the calendar)"
	}
	return errorReply(requestID, ErrorTravelBooking, message)
}

// holdPresent verifies a (possibly replayed) hold insert against the live
// calendar: one events-list call for the hold's local date; the hold exists
// iff an event with the expected summary, start, and end is present WITHOUT
// the cancelled prefix.
func (a *Agent) holdPresent(ctx context.Context, session upstreamSession, summary string, slot Slot) (bool, error) {
	start, err := parseEventTime(slot.Start)
	if err != nil {
		return false, err
	}
	end, err := parseEventTime(slot.End)
	if err != nil {
		return false, err
	}
	loc := loadLocation()
	day := localDateStart(start, loc)
	events, err := a.fetchCalendarEvents(ctx, session, []Interval{{Start: day, End: day.AddDate(0, 0, 1)}}, "verify-hold")
	if err != nil {
		a.recordCalendarWorkBlocked("read")
		return false, err
	}
	for _, event := range events {
		if strings.TrimSpace(event.Summary) != strings.TrimSpace(summary) {
			continue
		}
		evStart, evEnd, allDay, ok := eventBounds(event, loc)
		if !ok || allDay {
			continue
		}
		if evStart.Equal(start) && evEnd.Equal(end) {
			return true, nil
		}
	}
	return false, nil
}

// infeasibleTravelReply mirrors infeasibleReply with the travel-aware
// predicate so the nearest alternative is honest (§5.3). A blockFor error in
// the alternative horizon simply omits the alternative.
func (a *Agent) infeasibleTravelReply(ctx context.Context, session upstreamSession, requestID string, windows []Interval, events []calendarread.Event, extraBusy []BusyInterval, now time.Time, duration time.Duration, blockFor func(time.Time) (int, error)) Reply {
	reply := Reply{Status: StatusInfeasible, RequestID: requestID}
	altWindows := alternativeWindows(windows)
	if len(altWindows) == 0 {
		return reply
	}
	altEvents := append([]calendarread.Event{}, events...)
	if fetched, err := a.fetchCalendarEvents(ctx, session, altWindows, "read-alternative"); err == nil {
		altEvents = append(altEvents, fetched...)
	}
	if alt, _, ok, err := SelectSlotWithTravel(altWindows, altEvents, extraBusy, now, duration, blockFor); err == nil && ok {
		reply.NearestAlternative = &alt
	}
	return reply
}

// travelInsertRequest builds the travel-block insert payload (§6.2). The
// summary grammar is parsed back by the reconciliation watcher and must not
// change without updating travelSummaryRE:
//
//	before: "Travel: <dest> (for <HH:MM>)"
//	after:  "Travel: <dest> (return <HH:MM>)"
//
// <HH:MM> is the parent's local boundary time, zero-padded 24 h: before uses
// the parent start; return uses the parent end. The description lines are
// AUDIT ONLY: events-list does not return descriptions, so nothing may ever
// depend on reading them back.
func travelInsertRequest(dest, visibleLocation, parking, parentID, parentStartRFC string, parentStart, blockStart, blockEnd time.Time, sideWord string) outlookcalendarwrite.Request {
	labelTime := parentStart
	if sideWord == "return" {
		labelTime = blockStart
	}
	summary := travelBlockSummary(dest, sideWord, labelTime, loadLocation())
	if strings.TrimSpace(parentID) == "" {
		parentID = "unknown"
	}
	description := travelDescription(parentID, parentStartRFC, dest, visibleLocation, parking)
	showAs := "busy"
	location := strings.TrimSpace(visibleLocation)
	event := outlookcalendarwrite.EventInput{
		Summary:     &summary,
		Description: &description,
		Start:       writerDateTime(formatLA(blockStart)),
		End:         writerDateTime(formatLA(blockEnd)),
		ShowAs:      &showAs,
	}
	if location != "" {
		event.Location = &location
	}
	return outlookcalendarwrite.Request{
		Action:     "event-insert",
		CalendarID: "default",
		Event:      event,
	}
}

func travelBlockSummary(dest, sideWord string, labelTime time.Time, loc *time.Location) string {
	if loc == nil {
		loc = loadLocation()
	}
	return fmt.Sprintf("%s%s (%s %s)", outlookcalendarwrite.TravelSummaryPrefix, dest, sideWord, labelTime.In(loc).Format("15:04"))
}

func travelDescription(parentID, parentStartRFC, dest, visibleLocation, parking string) string {
	destination := strings.TrimSpace(visibleLocation)
	if destination == "" {
		destination = strings.TrimSpace(dest)
	}
	if destination == "" {
		destination = "offsite"
	}
	return fmt.Sprintf("travel_for=%s\nparent_start=%s\nDestination: %s\nParking: %s", parentID, parentStartRFC, destination, parking)
}

func cancelPatchRequest(eventID string) outlookcalendarwrite.Request {
	summary := outlookcalendarwrite.CancelledPrefix
	showAs := "free"
	return outlookcalendarwrite.Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    strings.TrimSpace(eventID),
		Event: outlookcalendarwrite.EventInput{
			Summary: &summary,
			ShowAs:  &showAs,
		},
	}
}

// destinationLabel resolves <dest> for travel-block summaries: venue name if
// matched, else the raw location whitespace-collapsed and truncated to 60
// runes, else "offsite".
func destinationLabel(estimate travelknowledge.Estimate, location string) string {
	if strings.TrimSpace(estimate.VenueName) != "" {
		return strings.TrimSpace(estimate.VenueName)
	}
	collapsed := travelknowledge.CollapseWhitespace(location)
	if collapsed != "" {
		return travelknowledge.TruncateRunes(collapsed, 60)
	}
	return "offsite"
}

// travelBlockLocation is the user-visible Outlook Location field. Summaries
// stay short, but minimized calendar cards need the endpoint for this travel
// segment: the meeting venue before the event, and the verified return/next
// target after the event.
func travelBlockLocation(estimate travelknowledge.Estimate, location, sideWord string) string {
	if sideWord == "return" {
		if target := namedAddress(estimate.OriginLabel, estimate.OriginAddress); target != "" {
			return travelknowledge.TruncateRunes(target, 200)
		}
	}
	name := travelknowledge.CollapseWhitespace(estimate.VenueName)
	address := travelknowledge.CollapseWhitespace(estimate.VenueAddress)
	if address != "" {
		if target := namedAddress(name, address); target != "" {
			return travelknowledge.TruncateRunes(target, 200)
		}
	}
	if name != "" {
		return travelknowledge.TruncateRunes(name, 200)
	}
	collapsed := travelknowledge.CollapseWhitespace(location)
	if collapsed != "" {
		return travelknowledge.TruncateRunes(collapsed, 200)
	}
	return ""
}

func namedAddress(name, address string) string {
	name = travelknowledge.CollapseWhitespace(name)
	address = travelknowledge.CollapseWhitespace(address)
	if address == "" {
		return name
	}
	if name != "" && !strings.Contains(strings.ToLower(address), strings.ToLower(name)) {
		return name + ", " + address
	}
	return address
}

func optionalSchedulerString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
