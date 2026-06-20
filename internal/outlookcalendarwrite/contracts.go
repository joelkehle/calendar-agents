package outlookcalendarwrite

import (
	"context"
	"strings"

	"github.com/joelkehle/calendar-agents/pkg/calendarcontract"
	"github.com/joelkehle/calendar-agents/pkg/calendaridentity"
	"github.com/joelkehle/calendar-agents/pkg/outlookwritecontract"
)

const (
	DefaultAgentID       = calendaridentity.OutlookWriteAgentID
	DefaultHTTPAddr      = ":8219"
	DefaultTimeZone      = calendarcontract.DefaultTimeZone
	ManagedByMarker      = calendarcontract.GuardManagedByMarker
	OwnerAgentMarker     = calendarcontract.OwnerAgentMarker
	AllowedSummaryPrefix = calendarcontract.GuardSummaryPrefix
	AllowedDaySummary    = calendarcontract.GuardDaySummary
	HoldSummaryPrefix    = calendarcontract.HoldSummaryPrefix
	CancelledPrefix      = calendarcontract.CancelledPrefix
	HoldClassMarker      = calendarcontract.HoldClassMarker
	TravelSummaryPrefix  = calendarcontract.TravelSummaryPrefix
	TravelClassMarker    = calendarcontract.TravelClassMarker
)

var ReservedHoldMarkerKeys = calendarcontract.ReservedMarkerKeys()

type Request = outlookwritecontract.Request
type EventDateTime = outlookwritecontract.EventDateTime
type EventInput = outlookwritecontract.EventInput
type StoredEvent = outlookwritecontract.StoredEvent
type MutationResponse = outlookwritecontract.MutationResponse

type Service interface {
	GetEvent(ctx context.Context, calendarID, eventID string) (StoredEvent, error)
	InsertEvent(ctx context.Context, calendarID string, event StoredEvent) (StoredEvent, error)
	PatchEvent(ctx context.Context, calendarID, eventID string, event StoredEvent) (StoredEvent, error)
}

// SnapshotCheckedService is an optional capability interface. Services that
// implement it verify, at write time, that the event's live start/end still
// match the snapshot the patch was built from, refusing with a
// "conflict: stale snapshot ..." error otherwise (lost-update protection for
// the travel-block patch path). Services that do not implement it fall back
// to plain PatchEvent.
type SnapshotCheckedService interface {
	PatchEventExpecting(ctx context.Context, calendarID, eventID string,
		expectedStart, expectedEnd EventDateTime, event StoredEvent) (StoredEvent, error)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
