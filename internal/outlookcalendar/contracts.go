package outlookcalendar

import (
	"github.com/joelkehle/calendar-agents/pkg/calendarcontract"
	"github.com/joelkehle/calendar-agents/pkg/calendaridentity"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

const (
	DefaultAgentID  = calendaridentity.PersonalOutlookReadAgentID
	DefaultHTTPAddr = ":8220"
	DefaultTimeZone = calendarcontract.DefaultTimeZone
)

type Request struct {
	Action string                   `json:"action"`
	Query  calendarread.EventsQuery `json:"query,omitempty"`
}

type Extractor interface {
	ListEvents(query calendarread.EventsQuery) ([]calendarread.Event, error)
}
