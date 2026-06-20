package travelknowledge

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const TimeZone = "America/Los_Angeles"

// ErrNoOrigin is the PER-CALL error returned when no origin can be resolved
// for the departure date (distinct from a load failure). Callers' behavior is
// normative (SCHEDULER_TRAVEL_SPEC §1.5): travel-estimate and offsite booking
// reply estimate_unavailable (zero writes); the watcher skips the meeting for
// the tick.
var ErrNoOrigin = errors.New("no_origin: no valid origin for departure date")

// Estimate is the door-to-door travel answer for one (eventStart, location)
// pair. Non-virtual estimates always have Minutes >= 1.
type Estimate struct {
	Minutes       int // drive + walk, total door-to-door
	DriveMinutes  int
	WalkMinutes   int
	OriginID      string
	OriginLabel   string
	OriginAddress string
	VenueID       string // "" when unmatched
	VenueName     string // "" when unmatched
	VenueAddress  string // "" when unmatched
	Parking       string // "" when unmatched
	Source        string // "matrix" | "default" | "virtual"
	IsOffice      bool   // destination matched office_aliases
	IsVirtual     bool   // destination matched virtual_aliases
}

// Knowledge bundles origins + venues. Files are read ONCE at startup; no hot
// reload in v1 (restart to pick up edits).
type Knowledge struct {
	Origins *Origins
	Venues  *Venues
}

// Load reads both knowledge files. Either failure is a load failure: the
// agent serves scheduler.v1 as today but refuses travel features (§1.6).
func Load(locationsPath, venuesPath string) (*Knowledge, error) {
	origins, err := LoadOrigins(locationsPath)
	if err != nil {
		return nil, err
	}
	venues, err := LoadVenues(venuesPath)
	if err != nil {
		return nil, err
	}
	return &Knowledge{Origins: origins, Venues: venues}, nil
}

// IsOffice / IsVirtual expose the venue matchers for callers that only need
// the offsite question.
func (k *Knowledge) IsOffice(location string) bool  { return k.Venues.IsOffice(location) }
func (k *Knowledge) IsVirtual(location string) bool { return k.Venues.IsVirtual(location) }

// Estimate answers "how long to <location> for an event starting at
// <eventStart>". Origin selection is evaluated at the approximate DEPARTURE
// time (eventStart - default_travel_minutes), not the meeting start: a 09:15
// offsite meeting is departed-for from home, not the office (§1.5).
//
// Origin rule (deterministic, v1):
//  1. departure = eventStart - default_travel_minutes, in America/Los_Angeles.
//  2. residence = the residence whose [from, until] window (inclusive) contains
//     departure's local date.
//  3. destination is the office => origin = residence; none => ErrNoOrigin.
//  4. otherwise origin = office when departure is Mon-Fri and its local time is
//     in [09:00, 18:00); else residence. When the chosen residence is absent the
//     office fallback applies on WEEKDAYS only (a weekend date outside all
//     residence windows has no plausible origin); both absent => ErrNoOrigin.
//     (Implementation note: spec §1.5 stated an unconditional fallback, which
//     would make no_origin unreachable for non-office destinations given the
//     loader requires a work entry — the weekday restriction is the minimal
//     refinement that keeps §9's TestWatcherNoOrigin/TestNoOriginBooking
//     satisfiable. Recorded in the spec's implementation-notes appendix.)
func (k *Knowledge) Estimate(eventStart time.Time, location string) (Estimate, error) {
	if k.Venues.IsVirtual(location) {
		return Estimate{Source: "virtual", IsVirtual: true}, nil
	}

	loc, err := time.LoadLocation(TimeZone)
	if err != nil {
		loc = time.Local
	}
	departure := eventStart.Add(-time.Duration(k.Venues.DefaultTravelMinutes) * time.Minute).In(loc)
	residence := k.Origins.ResidenceFor(departure)
	isOffice := k.Venues.IsOffice(location)
	weekday := departure.Weekday() >= time.Monday && departure.Weekday() <= time.Friday

	var originID, originLabel, originAddress string
	switch {
	case isOffice:
		if residence == nil {
			return Estimate{}, ErrNoOrigin
		}
		originID, originLabel, originAddress = residence.ID, residence.Label, residence.Address
	case weekday && departure.Hour() >= 9 && departure.Hour() < 18:
		originID, originLabel, originAddress = k.Origins.Work.ID, k.Origins.Work.Label, k.Origins.Work.Address
	case residence != nil:
		originID, originLabel, originAddress = residence.ID, residence.Label, residence.Address
	case weekday:
		originID, originLabel, originAddress = k.Origins.Work.ID, k.Origins.Work.Label, k.Origins.Work.Address
	default:
		return Estimate{}, ErrNoOrigin
	}
	return k.estimateWithOrigin(location, originID, originLabel, originAddress, false)
}

func (k *Knowledge) EstimateFromOrigin(eventStart time.Time, location, originID string) (Estimate, error) {
	if k.Venues.IsVirtual(location) {
		return Estimate{Source: "virtual", IsVirtual: true}, nil
	}
	label, address, ok := k.Origins.LocationForID(originID)
	if !ok {
		return Estimate{}, ErrNoOrigin
	}
	return k.estimateWithOrigin(location, strings.TrimSpace(originID), label, address, false)
}

func (k *Knowledge) EstimateReturnToOrigin(eventEnd time.Time, location, destinationOriginID string) (Estimate, error) {
	if k.Venues.IsVirtual(location) {
		return Estimate{Source: "virtual", IsVirtual: true}, nil
	}
	label, address, ok := k.Origins.LocationForID(destinationOriginID)
	if !ok {
		return Estimate{}, ErrNoOrigin
	}
	return k.estimateWithOrigin(location, strings.TrimSpace(destinationOriginID), label, address, true)
}

func (k *Knowledge) DefaultTravelMinutes() int {
	if k == nil || k.Venues == nil {
		return 30
	}
	return k.Venues.DefaultTravelMinutes
}

func (k *Knowledge) estimateWithOrigin(location, originID, originLabel, originAddress string, returnLeg bool) (Estimate, error) {
	if originID == "" {
		return Estimate{}, ErrNoOrigin
	}
	isOffice := k.Venues.IsOffice(location)
	est := Estimate{OriginID: originID, OriginLabel: originLabel, OriginAddress: originAddress, IsOffice: isOffice}
	venue := k.Venues.MatchVenue(location)
	switch {
	case venue != nil:
		est.VenueID = venue.ID
		est.VenueName = venue.Name
		est.VenueAddress = venue.Address
		est.Parking = venue.Parking
		est.WalkMinutes = venue.WalkMinutes
		minutes, ok := venue.TravelMinutes[originID]
		if returnLeg {
			if returnMinutes, returnOK := venue.ReturnMinutes[originID]; returnOK {
				minutes, ok = returnMinutes, true
			}
			if venue.ReturnBufferMinutes != nil {
				est.WalkMinutes = *venue.ReturnBufferMinutes
			}
		}
		if ok {
			est.DriveMinutes = minutes
			est.Source = "matrix"
		} else {
			est.DriveMinutes = k.Venues.DefaultTravelMinutes
			est.Source = "default"
		}
	default:
		est.DriveMinutes = k.Venues.DefaultTravelMinutes
		est.WalkMinutes = 0
		est.Source = "default"
	}
	est.Minutes = est.DriveMinutes + est.WalkMinutes
	return est, nil
}

// BlockMinutes is the travel-block duration derived from an estimate:
// clamp(roundUpToMultipleOf5(Minutes), 10, 120).
func BlockMinutes(est Estimate) int {
	minutes := est.Minutes
	if rem := minutes % 5; rem != 0 {
		minutes += 5 - rem
	}
	if minutes < 10 {
		minutes = 10
	}
	if minutes > 120 {
		minutes = 120
	}
	return minutes
}

// CollapseWhitespace collapses whitespace runs (incl. newlines) to single
// spaces and trims, preserving case — used for destination labels in
// travel-block summaries (§6.2; the caller handles truncation).
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TruncateRunes shortens s to at most n runes.
func TruncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}
