package outlookcalendarwrite

import "strings"

func ensureTravelMarker(description, requester string) string {
	description = strings.TrimSpace(description)
	marker := travelMarkerBlock(requester)
	if description == "" {
		return marker
	}
	return description + "\n\n" + marker
}

func travelMarkerBlock(requester string) string {
	return "managed_by=" + strings.TrimSpace(requester) + "\n" + OwnerAgentMarker + "\n" + TravelClassMarker
}

func travelMarkerRequester(description string) (string, bool) {
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
		if lines[i+1] == OwnerAgentMarker && lines[i+2] == TravelClassMarker {
			return requester, true
		}
	}
	return "", false
}

func stripTravelMarker(description string) string {
	lines := descriptionLines(description)
	for i := 0; i+2 < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "managed_by=") && strings.TrimPrefix(line, "managed_by=") != "" && lines[i+1] == OwnerAgentMarker && lines[i+2] == TravelClassMarker {
			out := append([]string{}, lines[:i]...)
			out = append(out, lines[i+3:]...)
			return strings.TrimSpace(strings.Join(out, "\n"))
		}
	}
	return strings.TrimSpace(description)
}

func HasTravelMarker(description string) bool {
	_, ok := travelMarkerRequester(description)
	return ok
}
