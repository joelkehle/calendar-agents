package calendaridentity

import "testing"

func TestAllAgentIDsAreStableAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, id := range All() {
		if id == "" {
			t.Fatal("agent id must not be empty")
		}
		if seen[id] {
			t.Fatalf("duplicate agent id %q", id)
		}
		seen[id] = true
	}

	for _, id := range []string{
		"jk-calendar-guard-agent",
		"jk-outlook-calendar-agent",
		"ucla-tdg-outlook-calendar-agent",
		"ucla-tdg-outlook-calendar-write-agent",
		"ucla-tdg-scheduler-agent",
	} {
		if !seen[id] {
			t.Fatalf("missing historical live id %q", id)
		}
	}
}
