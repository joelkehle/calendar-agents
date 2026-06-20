package calendarcontract

import "testing"

func TestEventClassConstantsPreserveLiveWireValues(t *testing.T) {
	tests := map[string]string{
		"DefaultTimeZone":      DefaultTimeZone,
		"GuardManagedByMarker": GuardManagedByMarker,
		"OwnerAgentMarker":     OwnerAgentMarker,
		"GuardSummaryPrefix":   GuardSummaryPrefix,
		"GuardDaySummary":      GuardDaySummary,
		"HoldSummaryPrefix":    HoldSummaryPrefix,
		"CancelledPrefix":      CancelledPrefix,
		"HoldClassMarker":      HoldClassMarker,
		"TravelSummaryPrefix":  TravelSummaryPrefix,
		"TravelClassMarker":    TravelClassMarker,
		"WorkingHoldClass":     WorkingHoldClass,
		"TravelBlockClass":     TravelBlockClass,
	}

	want := map[string]string{
		"DefaultTimeZone":      "America/Los_Angeles",
		"GuardManagedByMarker": "managed_by=jk-calendar-guard-agent",
		"OwnerAgentMarker":     "owner_agent=ucla-tdg-outlook-calendar-write-agent",
		"GuardSummaryPrefix":   "No more meetings",
		"GuardDaySummary":      "Meeting Quota Reached",
		"HoldSummaryPrefix":    "Joel + ",
		"CancelledPrefix":      "[CANCELLED] ",
		"HoldClassMarker":      "hold_class=working-hold",
		"TravelSummaryPrefix":  "Travel: ",
		"TravelClassMarker":    "hold_class=travel-block",
		"WorkingHoldClass":     "working-hold",
		"TravelBlockClass":     "travel-block",
	}

	for name, got := range tests {
		if got != want[name] {
			t.Fatalf("%s = %q, want %q", name, got, want[name])
		}
	}
}

func TestReservedMarkerKeysReturnsFreshSlice(t *testing.T) {
	keys := ReservedMarkerKeys()
	want := []string{"managed_by=", "owner_agent=", "hold_class="}
	if len(keys) != len(want) {
		t.Fatalf("len(ReservedMarkerKeys()) = %d, want %d", len(keys), len(want))
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("ReservedMarkerKeys()[%d] = %q, want %q", i, keys[i], want[i])
		}
	}

	keys[0] = "mutated="
	if got := ReservedMarkerKeys()[0]; got != "managed_by=" {
		t.Fatalf("ReservedMarkerKeys()[0] after caller mutation = %q", got)
	}
}
