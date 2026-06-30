package scheduler

import (
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/pkg/schedulercontract"
)

const (
	DefaultAgentID            = schedulercontract.DefaultAgentID
	DefaultBusURL             = schedulercontract.DefaultBusURL
	DefaultHTTPAddr           = schedulercontract.DefaultHTTPAddr
	DefaultCalendarReadAgent  = schedulercontract.DefaultCalendarReadAgent
	DefaultCalendarWriteAgent = schedulercontract.DefaultCalendarWriteAgent
	DefaultTimeZone           = schedulercontract.DefaultTimeZone

	CapabilityRequest  = schedulercontract.CapabilityRequest
	CapabilityMove     = schedulercontract.CapabilityMove
	CapabilityCancel   = schedulercontract.CapabilityCancel
	CapabilityEstimate = schedulercontract.CapabilityEstimate

	StatusBooked     = schedulercontract.StatusBooked
	StatusMoved      = schedulercontract.StatusMoved
	StatusCancelled  = schedulercontract.StatusCancelled
	StatusInfeasible = schedulercontract.StatusInfeasible
	StatusRefused    = schedulercontract.StatusRefused
	StatusError      = schedulercontract.StatusError
	StatusEstimated  = schedulercontract.StatusEstimated

	ErrorInvalidWindow       = schedulercontract.ErrorInvalidWindow
	ErrorInvalidRequest      = schedulercontract.ErrorInvalidRequest
	ErrorUpstreamUnavailable = schedulercontract.ErrorUpstreamUnavailable
	ErrorNotOwned            = schedulercontract.ErrorNotOwned
	ErrorBookingRefused      = schedulercontract.ErrorBookingRefused
	ErrorOtherPeople         = schedulercontract.ErrorOtherPeople
	ErrorEstimateUnavailable = schedulercontract.ErrorEstimateUnavailable
	ErrorTravelBooking       = schedulercontract.ErrorTravelBooking

	// DefaultOffsiteCategory is Outlook's default display name for the yellow
	// category; Joel's 2026-06-11 ruling: yellow = offsite. The
	// contains-"yellow" fallback in the watcher covers renames.
	DefaultOffsiteCategory = schedulercontract.DefaultOffsiteCategory
)

type Config struct {
	BusURL             string
	AgentID            string
	Secret             string
	PollWaitSec        int
	AllowedRequesters  []string
	CalendarReadAgent  string
	CalendarWriteAgent string
	UpstreamTimeout    time.Duration
	Workers            int
	Now                func() time.Time

	// Travel knowledge (SCHEDULER_TRAVEL_SPEC §1.6). Paths resolve relative to
	// the process working directory; the deploy unit must set absolute paths.
	// Files are read once at startup; no hot reload in v1.
	LocationsPath string
	VenuesPath    string
	// OffsiteCategory is the Outlook category name that marks a meeting
	// offsite (default "Yellow category"; matching is case-insensitive with a
	// contains-"yellow" fallback).
	OffsiteCategory string
	// WatchIntervalMin is the reconciliation-watcher tick interval in
	// minutes. Zero or negative disables the watcher entirely (the env
	// wiring in cmd/scheduler-agent defaults the value to 15 when unset).
	WatchIntervalMin int
	// WatchHorizonDays is how many days beyond today each watcher tick
	// scans (default 3, i.e. four local dates including today).
	WatchHorizonDays int
}

type Request = schedulercontract.Request
type Reply = schedulercontract.Reply
type EstimateOrigin = schedulercontract.EstimateOrigin
type EstimateVenue = schedulercontract.EstimateVenue
type EstimateResult = schedulercontract.EstimateResult
type TravelBooking = schedulercontract.TravelBooking
type TravelLeg = schedulercontract.TravelLeg
type Slot = schedulercontract.Slot
type Interval = schedulercontract.Interval
type requestError = schedulercontract.RequestError

func DecodeRequest(body string) (Request, error) {
	return schedulercontract.DecodeRequest(body)
}

func errorReply(requestID, code, message string) Reply {
	return schedulercontract.ErrorReply(requestID, code, message)
}

func refusedReply(requestID string) Reply {
	return schedulercontract.RefusedReply(requestID)
}

func withDefaults(cfg Config) Config {
	if strings.TrimSpace(cfg.BusURL) == "" {
		cfg.BusURL = DefaultBusURL
	}
	if strings.TrimSpace(cfg.AgentID) == "" {
		cfg.AgentID = DefaultAgentID
	}
	if strings.TrimSpace(cfg.CalendarReadAgent) == "" {
		cfg.CalendarReadAgent = DefaultCalendarReadAgent
	}
	if strings.TrimSpace(cfg.CalendarWriteAgent) == "" {
		cfg.CalendarWriteAgent = DefaultCalendarWriteAgent
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 60 * time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Workers > 4 {
		cfg.Workers = 4
	}
	cfg.AllowedRequesters = ParseRequesters(cfg.AllowedRequesters)
	if strings.TrimSpace(cfg.LocationsPath) == "" {
		cfg.LocationsPath = "data/locations.json"
	}
	if strings.TrimSpace(cfg.VenuesPath) == "" {
		cfg.VenuesPath = "data/venues.json"
	}
	if strings.TrimSpace(cfg.OffsiteCategory) == "" {
		cfg.OffsiteCategory = DefaultOffsiteCategory
	}
	// WatchIntervalMin <= 0 disables the watcher (SCHEDULER_TRAVEL_SPEC §7.1,
	// review nit N3); the 15-minute default is applied by the env wiring, not
	// here, so tests and embedders can disable it with the zero value.
	if cfg.WatchIntervalMin < 0 {
		cfg.WatchIntervalMin = 0
	}
	if cfg.WatchHorizonDays <= 0 {
		cfg.WatchHorizonDays = 3
	}
	return cfg
}

func ParseRequesters(requesters []string) []string {
	out := make([]string, 0, len(requesters))
	seen := make(map[string]struct{}, len(requesters))
	for _, requester := range requesters {
		requester = strings.TrimSpace(requester)
		if requester == "" {
			continue
		}
		if _, ok := seen[requester]; ok {
			continue
		}
		seen[requester] = struct{}{}
		out = append(out, requester)
	}
	return out
}
