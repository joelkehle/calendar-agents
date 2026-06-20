package scheduler

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
)

type watchCommunicationProposal struct {
	Key              string
	Reason           string
	MeetingID        string
	MeetingSummary   string
	BlockingID       string
	BlockingSummary  string
	RecipientLabel   string
	RecipientEmail   string
	Subject          string
	Body             string
	DesiredLeaveTime time.Time
	CurrentLeaveTime time.Time
	RequiredMinutes  int
	AvailableMinutes int
}

func (w *travelWatcher) maybeProposeAttachedCompressedTravel(side string, meeting, block watchEvent, allDay, anchors, neighbors []watchEvent, loc *time.Location) {
	if side != "before" {
		return
	}
	location := w.estimateLocation(meeting, loc)
	estimate, err := w.estimateForSide(side, meeting, allDay, anchors, location, loc)
	if err != nil {
		return
	}
	w.maybeProposeCompressedTravel(side, meeting, neighbors, travelknowledge.BlockMinutes(estimate), Interval{Start: block.start, End: block.end}, loc)
}

func (w *travelWatcher) maybeProposeCompressedTravel(side string, meeting watchEvent, neighbors []watchEvent, blockMinutes int, interval Interval, loc *time.Location) {
	if side != "before" || blockMinutes <= 0 {
		return
	}
	desiredStart := meeting.start.Add(-time.Duration(blockMinutes) * time.Minute)
	if !interval.Start.After(desiredStart) {
		return
	}
	blocker, ok := compressedBeforeBlocker(meeting, neighbors, desiredStart, interval.Start)
	if !ok {
		return
	}
	if eventPriorityScore(meeting) <= eventPriorityScore(blocker) {
		return
	}
	availableMinutes := int(desiredStart.Sub(blocker.start) / time.Minute)
	if availableMinutes < 10 {
		return
	}
	recipientLabel, recipientEmail := communicationRecipient(blocker)
	if recipientLabel == "" {
		recipientLabel = "there"
	}
	obligationLabel := obligationLabel(meeting)
	subject := communicationSubject(blocker)
	body := fmt.Sprintf(
		"Hi %s,\n\nQuick heads-up: I can still meet at %s, but I need to drop at %s because I have an obligation with %s. I am good for the first %d minutes.\n\nJoel",
		recipientLabel,
		blocker.start.In(loc).Format("3:04 PM"),
		desiredStart.In(loc).Format("3:04 PM"),
		obligationLabel,
		availableMinutes,
	)
	key := "schedw-comm-" + idempotencyHash(strings.Join([]string{
		meeting.ev.ID,
		blocker.ev.ID,
		formatLA(meeting.start),
		formatLA(desiredStart),
		formatLA(interval.Start),
	}, "|"))
	if w.proposalKeys[key] {
		return
	}
	w.proposalKeys[key] = true
	proposal := watchCommunicationProposal{
		Key:              key,
		Reason:           "travel_compressed_by_lower_priority_meeting",
		MeetingID:        meeting.ev.ID,
		MeetingSummary:   strings.TrimSpace(meeting.ev.Summary),
		BlockingID:       blocker.ev.ID,
		BlockingSummary:  strings.TrimSpace(blocker.ev.Summary),
		RecipientLabel:   recipientLabel,
		RecipientEmail:   recipientEmail,
		Subject:          subject,
		Body:             body,
		DesiredLeaveTime: desiredStart,
		CurrentLeaveTime: interval.Start,
		RequiredMinutes:  blockMinutes,
		AvailableMinutes: availableMinutes,
	}
	w.lastCommunicationProposals = append(w.lastCommunicationProposals, proposal)
	w.agent.metrics.IncCounter("watch_travel_communication_proposed")
	log.Printf("%s watcher proposal reason=%s meeting=%s blocker=%s recipient=%q leave_by=%s current_block_start=%s required=%dm available=%dm subject=%q",
		w.agent.cfg.AgentID,
		proposal.Reason,
		proposal.MeetingID,
		proposal.BlockingID,
		proposal.RecipientLabel,
		formatLA(proposal.DesiredLeaveTime),
		formatLA(proposal.CurrentLeaveTime),
		proposal.RequiredMinutes,
		proposal.AvailableMinutes,
		proposal.Subject,
	)
}

func compressedBeforeBlocker(meeting watchEvent, neighbors []watchEvent, desiredStart, actualStart time.Time) (watchEvent, bool) {
	var best watchEvent
	found := false
	for _, neighbor := range neighbors {
		if neighbor.ev.ID == meeting.ev.ID {
			continue
		}
		summary := strings.TrimSpace(neighbor.ev.Summary)
		if strings.HasPrefix(summary, outlookcalendarwrite.TravelSummaryPrefix) || summary == maskedPrivateSummary {
			continue
		}
		if !intervalsOverlap(desiredStart, meeting.start, neighbor.start, neighbor.end) {
			continue
		}
		if neighbor.end.After(meeting.start) || neighbor.end.Before(desiredStart) || neighbor.end.After(actualStart) {
			continue
		}
		if !found || neighbor.end.After(best.end) {
			best = neighbor
			found = true
		}
	}
	return best, found
}

func eventPriorityScore(event watchEvent) int {
	text := eventSearchText(event)
	switch {
	case strings.Contains(text, "josh") || strings.Contains(text, "jeanson"):
		return 100
	case strings.Contains(text, "boss") || strings.Contains(text, "manager"):
		return 90
	case strings.Contains(text, "carol") && (strings.Contains(text, "1:1") || strings.Contains(text, "1-1") || strings.Contains(text, "one-on-one") || strings.Contains(text, "one on one")):
		return 30
	case strings.Contains(text, "1:1") || strings.Contains(text, "1-1") || strings.Contains(text, "one-on-one") || strings.Contains(text, "one on one"):
		return 40
	default:
		return 50
	}
}

func eventSearchText(event watchEvent) string {
	parts := []string{event.ev.Summary, event.ev.Location}
	for _, attendee := range event.ev.Attendees {
		parts = append(parts, attendee.DisplayName, attendee.Email)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func communicationRecipient(blocker watchEvent) (string, string) {
	summaryText := strings.ToLower(blocker.ev.Summary)
	for _, attendee := range blocker.ev.Attendees {
		if attendee.Self {
			continue
		}
		display := strings.TrimSpace(attendee.DisplayName)
		email := strings.TrimSpace(attendee.Email)
		search := strings.ToLower(display + " " + email)
		if strings.Contains(summaryText, "carol") && strings.Contains(search, "carol") {
			return "Carol", email
		}
	}
	for _, attendee := range blocker.ev.Attendees {
		if attendee.Self {
			continue
		}
		display := strings.TrimSpace(attendee.DisplayName)
		email := strings.TrimSpace(attendee.Email)
		if display == "" {
			display = labelFromEmail(email)
		}
		if display != "" {
			return firstName(display), email
		}
	}
	return recipientFromSummary(blocker.ev.Summary), ""
}

func recipientFromSummary(summary string) string {
	clean := strings.TrimSpace(summary)
	if clean == "" {
		return ""
	}
	lower := strings.ToLower(clean)
	for _, sep := range []string{" & ", " and ", " with "} {
		if idx := strings.Index(lower, sep); idx > 0 {
			return firstName(clean[:idx])
		}
	}
	fields := strings.Fields(clean)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "joel") && len(fields) > 1 {
		return firstName(fields[1])
	}
	return firstName(fields[0])
}

func obligationLabel(meeting watchEvent) string {
	for _, attendee := range meeting.ev.Attendees {
		search := strings.ToLower(attendee.DisplayName + " " + attendee.Email)
		if strings.Contains(search, "josh") || strings.Contains(search, "jeanson") {
			return "Josh"
		}
	}
	text := eventSearchText(meeting)
	if strings.Contains(text, "josh") || strings.Contains(text, "jeanson") {
		return "Josh"
	}
	return "the next obligation"
}

func communicationSubject(blocker watchEvent) string {
	subject := strings.TrimSpace(blocker.ev.Summary)
	if subject == "" {
		return "Schedule adjustment"
	}
	runes := []rune(subject)
	if len(runes) > 80 {
		return string(runes[:77]) + "..."
	}
	return subject
}

func labelFromEmail(email string) string {
	local, _, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok {
		return strings.TrimSpace(email)
	}
	return strings.ReplaceAll(local, ".", " ")
}

func firstName(label string) string {
	fields := strings.Fields(strings.Trim(label, " ,;:"))
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], " ,;:")
}
