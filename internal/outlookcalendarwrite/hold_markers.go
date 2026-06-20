package outlookcalendarwrite

import "strings"

func ensureHoldMarker(description, requester string) string {
	description = strings.TrimSpace(description)
	marker := holdMarkerBlock(requester)
	if description == "" {
		return marker
	}
	return description + "\n\n" + marker
}

func holdMarkerBlock(requester string) string {
	return "managed_by=" + strings.TrimSpace(requester) + "\n" + OwnerAgentMarker + "\n" + HoldClassMarker
}

func holdMarkerRequester(description string) (string, bool) {
	lines := descriptionLines(description)
	for i := 0; i+2 < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "managed_by=") {
			continue
		}
		requester := strings.TrimPrefix(line, "managed_by=")
		if requester == "" {
			continue
		}
		if lines[i+1] == OwnerAgentMarker && lines[i+2] == HoldClassMarker {
			return requester, true
		}
	}
	return "", false
}

func stripHoldMarker(description string) string {
	lines := descriptionLines(description)
	for i := 0; i+2 < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "managed_by=") && strings.TrimPrefix(line, "managed_by=") != "" && lines[i+1] == OwnerAgentMarker && lines[i+2] == HoldClassMarker {
			out := append([]string{}, lines[:i]...)
			out = append(out, lines[i+3:]...)
			return strings.TrimSpace(strings.Join(out, "\n"))
		}
	}
	return strings.TrimSpace(description)
}

func descriptionLines(description string) []string {
	description = strings.ReplaceAll(description, "\r\n", "\n")
	description = strings.ReplaceAll(description, "\r", "\n")
	lines := strings.Split(description, "\n")
	// Outlook appends trailing whitespace to body lines on round-trip
	// (observed live 2026-06-11); marker matching must survive it.
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return lines
}

func containsReservedHoldMarkerKey(description string) bool {
	description = strings.ToLower(description)
	for _, key := range ReservedHoldMarkerKeys {
		if strings.Contains(description, key) {
			return true
		}
	}
	return false
}

func isCancelSummary(summary string) bool {
	return summary == strings.TrimSpace(CancelledPrefix) || strings.HasPrefix(summary, CancelledPrefix)
}
