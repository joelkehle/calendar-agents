package scheduler

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
)

const maxLocationRunes = 200

// estimateReply implements the travel-estimate capability (scheduler.v1
// addition, SCHEDULER_TRAVEL_SPEC §2). One travel brain for the fleet: the
// daily-briefing agent calls this over the UCLA bus; it must not grow its own
// matrix. Replies are synchronous and deterministic; nothing is cached and no
// job is enqueued (the prohibited-field refusal is handled by the caller in
// handleEvent before this runs).
func (a *Agent) estimateReply(req Request) Reply {
	a.metrics.IncCounter("travel_estimate_requests")
	if strings.TrimSpace(req.RequestID) == "" {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply("", ErrorInvalidRequest, "request_id is required")
	}
	eventStartRaw := strings.TrimSpace(req.EventStart)
	if eventStartRaw == "" {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply(req.RequestID, ErrorInvalidRequest, "event_start is required")
	}
	eventStart, err := time.Parse(time.RFC3339, eventStartRaw)
	if err != nil {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply(req.RequestID, ErrorInvalidRequest, "event_start must be RFC3339 with offset")
	}
	location := strings.TrimSpace(req.Location)
	if location == "" {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply(req.RequestID, ErrorInvalidRequest, "location is required")
	}
	if utf8.RuneCountInString(location) > maxLocationRunes {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply(req.RequestID, ErrorInvalidRequest, "location must be 200 characters or fewer")
	}
	if a.knowledge == nil {
		a.metrics.IncCounter("travel_estimate_errors")
		return errorReply(req.RequestID, ErrorEstimateUnavailable, "travel knowledge is not loaded")
	}
	estimate, err := a.knowledge.Estimate(eventStart, location)
	if err != nil {
		a.metrics.IncCounter("travel_estimate_errors")
		if errors.Is(err, travelknowledge.ErrNoOrigin) {
			return errorReply(req.RequestID, ErrorEstimateUnavailable, "no origin known for the requested date")
		}
		return errorReply(req.RequestID, ErrorEstimateUnavailable, err.Error())
	}
	return Reply{
		Status:    StatusEstimated,
		RequestID: req.RequestID,
		Estimate:  estimateResult(estimate),
	}
}

func estimateResult(estimate travelknowledge.Estimate) *EstimateResult {
	out := &EstimateResult{
		Minutes:      estimate.Minutes,
		DriveMinutes: estimate.DriveMinutes,
		WalkMinutes:  estimate.WalkMinutes,
		Source:       estimate.Source,
		IsOffice:     estimate.IsOffice,
		IsVirtual:    estimate.IsVirtual,
	}
	if estimate.OriginID != "" {
		out.Origin = &EstimateOrigin{ID: estimate.OriginID, Label: estimate.OriginLabel}
	}
	if estimate.VenueID != "" {
		out.Venue = &EstimateVenue{ID: estimate.VenueID, Name: estimate.VenueName, Parking: estimate.Parking}
	}
	return out
}
