package scheduler

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
)

var closeEarlyRE = regexp.MustCompile(`(?i)\bat\s+(\d{1,2})(?::([0-5]\d))?\s*([ap]m)?\b`)

func blockParentMatches(side, grammarHHMM string, meeting watchEvent, blockDate time.Time, loc *time.Location) bool {
	candidates := []time.Time{meeting.start}
	if side == "after" {
		// New return blocks use the parent END. The parent START fallback keeps
		// old live blocks reconcilable instead of orphaning them immediately.
		candidates = []time.Time{meeting.end, meeting.start}
	}
	for _, candidate := range candidates {
		local := candidate.In(loc)
		if localDateStart(local, loc).Equal(blockDate) && local.Format("15:04") == grammarHHMM {
			return true
		}
	}
	return false
}

func (w *travelWatcher) estimateForSide(side string, meeting watchEvent, allDay, anchors []watchEvent, location string, loc *time.Location) (travelknowledge.Estimate, error) {
	if side == "after" {
		if targetID := w.verifiedReturnTargetID(meeting, anchors, loc); targetID != "" {
			return w.agent.knowledge.EstimateReturnToOrigin(meeting.end, location, targetID)
		}
		if departure, err := w.estimateDeparture(meeting, allDay, anchors, location, loc); err == nil && departure.OriginID != "" {
			return w.agent.knowledge.EstimateReturnToOrigin(meeting.end, location, departure.OriginID)
		}
		return w.agent.knowledge.Estimate(meeting.end, location)
	}
	return w.estimateDeparture(meeting, allDay, anchors, location, loc)
}

func (w *travelWatcher) estimateDeparture(meeting watchEvent, allDay, anchors []watchEvent, location string, loc *time.Location) (travelknowledge.Estimate, error) {
	if originID := w.verifiedDepartureOriginID(meeting, allDay, anchors, loc); originID != "" {
		return w.agent.knowledge.EstimateFromOrigin(meeting.start, location, originID)
	}
	return w.agent.knowledge.Estimate(meeting.start, location)
}

func (w *travelWatcher) verifiedDepartureOriginID(meeting watchEvent, allDay, anchors []watchEvent, loc *time.Location) string {
	departure := meeting.start.Add(-time.Duration(w.agent.knowledge.DefaultTravelMinutes()) * time.Minute).In(loc)
	if w.officeClosedBy(allDay, departure, loc) {
		if id := w.activeResidenceID(departure, loc); id != "" {
			return id
		}
	}
	if id := w.latestAnchorOriginIDBefore(anchors, departure, loc); id != "" {
		return id
	}
	return ""
}

func (w *travelWatcher) verifiedReturnTargetID(meeting watchEvent, anchors []watchEvent, loc *time.Location) string {
	if id := w.nextAnchorOriginIDAfter(anchors, meeting.end.In(loc), loc); id != "" {
		return id
	}
	return ""
}

func (w *travelWatcher) latestAnchorOriginIDBefore(anchors []watchEvent, cutoff time.Time, loc *time.Location) string {
	var bestEnd time.Time
	bestID := ""
	cutoffDate := localDateStart(cutoff.In(loc), loc)
	for _, anchor := range anchors {
		if anchor.ev.ID == "" || anchor.end.After(cutoff) {
			continue
		}
		if !localDateStart(anchor.end.In(loc), loc).Equal(cutoffDate) {
			continue
		}
		id := w.originIDForEventLocation(anchor.ev.Location)
		if id == "" {
			continue
		}
		if bestID == "" || anchor.end.After(bestEnd) {
			bestID = id
			bestEnd = anchor.end
		}
	}
	return bestID
}

func (w *travelWatcher) nextAnchorOriginIDAfter(anchors []watchEvent, cutoff time.Time, loc *time.Location) string {
	var bestStart time.Time
	bestID := ""
	cutoffDate := localDateStart(cutoff.In(loc), loc)
	for _, anchor := range anchors {
		if anchor.ev.ID == "" || anchor.start.Before(cutoff) {
			continue
		}
		if !localDateStart(anchor.start.In(loc), loc).Equal(cutoffDate) {
			continue
		}
		id := w.originIDForEventLocation(anchor.ev.Location)
		if id == "" {
			continue
		}
		if bestID == "" || anchor.start.Before(bestStart) {
			bestID = id
			bestStart = anchor.start
		}
	}
	return bestID
}

func (w *travelWatcher) originIDForEventLocation(location string) string {
	knowledge := w.agent.knowledge
	if knowledge == nil || knowledge.Origins == nil || knowledge.Venues == nil {
		return ""
	}
	location = strings.TrimSpace(location)
	if location == "" || knowledge.IsVirtual(location) {
		return ""
	}
	if knowledge.IsOffice(location) {
		return knowledge.Origins.Work.ID
	}
	for _, residence := range knowledge.Origins.Residences {
		if locationMatchesOrigin(location, residence.Label, residence.Address) {
			return residence.ID
		}
	}
	if locationMatchesOrigin(location, knowledge.Origins.Work.Label, knowledge.Origins.Work.Address) {
		return knowledge.Origins.Work.ID
	}
	return ""
}

func locationMatchesOrigin(location, label, address string) bool {
	normalized := strings.ToLower(travelknowledge.CollapseWhitespace(location))
	for _, value := range []string{label, address} {
		value = strings.ToLower(travelknowledge.CollapseWhitespace(value))
		if value != "" && strings.Contains(normalized, value) {
			return true
		}
	}
	return false
}

func (w *travelWatcher) activeResidenceID(at time.Time, loc *time.Location) string {
	knowledge := w.agent.knowledge
	if knowledge == nil || knowledge.Origins == nil {
		return ""
	}
	residence := knowledge.Origins.ResidenceFor(at.In(loc))
	if residence == nil {
		return ""
	}
	return residence.ID
}

func (w *travelWatcher) officeClosedBy(allDay []watchEvent, at time.Time, loc *time.Location) bool {
	for _, event := range allDay {
		if !eventCovers(event, at) {
			continue
		}
		summary := strings.ToLower(strings.TrimSpace(event.ev.Summary))
		if !strings.Contains(summary, "office closes early") {
			continue
		}
		closeAt, ok := parseCloseEarlyTime(event.ev.Summary, at.In(loc), loc)
		if ok && !at.In(loc).Before(closeAt) {
			return true
		}
	}
	return false
}

func eventCovers(event watchEvent, at time.Time) bool {
	return !event.start.After(at) && event.end.After(at)
}

func parseCloseEarlyTime(summary string, at time.Time, loc *time.Location) (time.Time, bool) {
	match := closeEarlyRE.FindStringSubmatch(summary)
	if match == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(match[1])
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if match[2] != "" {
		minute, err = strconv.Atoi(match[2])
		if err != nil {
			return time.Time{}, false
		}
	}
	meridiem := strings.ToLower(match[3])
	switch meridiem {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	default:
		if hour < 0 || hour > 23 {
			return time.Time{}, false
		}
	}
	if hour < 0 || hour > 23 {
		return time.Time{}, false
	}
	return time.Date(at.Year(), at.Month(), at.Day(), hour, minute, 0, 0, loc), true
}
