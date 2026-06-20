// Package calendarcontract contains shared wire-level constants for Joel's
// calendar event classes.
package calendarcontract

import "github.com/joelkehle/calendar-agents/pkg/calendaridentity"

const (
	DefaultTimeZone = "America/Los_Angeles"

	ManagedByKey  = "managed_by"
	OwnerAgentKey = "owner_agent"
	EventClassKey = "hold_class"

	GuardManagedByMarker = ManagedByKey + "=" + calendaridentity.CalendarGuardAgentID
	OwnerAgentMarker     = OwnerAgentKey + "=" + calendaridentity.OutlookWriteAgentID

	GuardSummaryPrefix  = "No more meetings"
	GuardDaySummary     = "Meeting Quota Reached"
	HoldSummaryPrefix   = "Joel + "
	TravelSummaryPrefix = "Travel: "
	CancelledPrefix     = "[CANCELLED] "

	WorkingHoldClass = "working-hold"
	TravelBlockClass = "travel-block"

	HoldClassMarker   = EventClassKey + "=" + WorkingHoldClass
	TravelClassMarker = EventClassKey + "=" + TravelBlockClass
)

// ReservedMarkerKeys returns description-marker prefixes owned by the shared
// calendar write contract. Callers receive a fresh slice.
func ReservedMarkerKeys() []string {
	return []string{
		ManagedByKey + "=",
		OwnerAgentKey + "=",
		EventClassKey + "=",
	}
}
