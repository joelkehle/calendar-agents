// Package calendaridentity centralizes live agent IDs for Joel's shared
// calendar capability.
//
// Some live IDs still carry jk or ucla-tdg prefixes because they were deployed
// before the Projects ownership taxonomy was clarified. Do not infer code or
// product ownership from these historical bus IDs.
package calendaridentity

const (
	CalendarGuardAgentID = "jk-calendar-guard-agent"

	PersonalOutlookReadAgentID     = "jk-outlook-calendar-agent"
	ProfessionalOutlookReadAgentID = "ucla-tdg-outlook-calendar-agent"

	// OutlookWriteAgentID is the shared Joel-calendar writer. The ID is
	// historical: the capability writes Joel's real Outlook calendar and is
	// consumed by both personal and professional workflows.
	OutlookWriteAgentID = "ucla-tdg-outlook-calendar-write-agent"

	// SchedulerAgentID is the shared scheduling/travel-time orchestrator. The
	// ID is historical and should not be read as UCLA-only ownership.
	SchedulerAgentID = "ucla-tdg-scheduler-agent"
)

// All returns the known shared calendar agent IDs.
func All() []string {
	return []string{
		CalendarGuardAgentID,
		PersonalOutlookReadAgentID,
		ProfessionalOutlookReadAgentID,
		OutlookWriteAgentID,
		SchedulerAgentID,
	}
}
