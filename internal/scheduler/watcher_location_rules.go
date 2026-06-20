package scheduler

import (
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
)

const davidKTivertonLocation = "805 Tiverton Ave, Los Angeles, CA 90024"

// inferredFaceToFaceLocation applies durable, Joel-provided calendar facts
// when an event is visible enough to identify but does not carry a usable
// physical location.
func (w *travelWatcher) inferredFaceToFaceLocation(event watchEvent, loc *time.Location) string {
	if w.agent == nil || w.agent.knowledge == nil {
		return ""
	}
	if loc == nil {
		loc = loadLocation()
	}
	if event.start.In(loc).Weekday() != time.Wednesday {
		return ""
	}
	if !isDavidKText(eventSearchText(event)) {
		return ""
	}

	location := strings.TrimSpace(event.ev.Location)
	if location == "" {
		return davidKTivertonLocation
	}
	if !w.agent.knowledge.IsVirtual(location) {
		return ""
	}
	if w.hasYellowCategory(event.ev.Categories) || hasFaceToFaceHint(event) {
		return davidKTivertonLocation
	}
	return ""
}

func isDavidKText(text string) bool {
	normalized := " " + normalizeLocationRuleText(text) + " "
	return strings.Contains(normalized, " kronemyer ") ||
		strings.Contains(normalized, " dkronemyer ") ||
		strings.Contains(normalized, " david k ")
}

func hasFaceToFaceHint(event watchEvent) bool {
	normalized := " " + normalizeLocationRuleText(eventSearchText(event)+" "+strings.Join(event.ev.Categories, " ")) + " "
	return strings.Contains(normalized, " face to face ") ||
		strings.Contains(normalized, " f2f ") ||
		strings.Contains(normalized, " in person ")
}

func normalizeLocationRuleText(text string) string {
	replacer := strings.NewReplacer(
		".", " ",
		",", " ",
		";", " ",
		":", " ",
		"@", " ",
		"-", " ",
		"_", " ",
		"/", " ",
		"\\", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
	)
	return strings.ToLower(travelknowledge.CollapseWhitespace(replacer.Replace(text)))
}
