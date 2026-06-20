// Package travelknowledge loads Joel's origin locations (data/locations.json)
// and venue knowledge (data/venues.json) and answers "how long does Joel need
// to get to X at time T" deterministically (static matrix v1, no maps API).
//
// Spec: docs/SCHEDULER_TRAVEL_SPEC.md §1. The spec names this package
// internal/travel; that import path is already occupied by the live
// trip-planning travel agent, so the knowledge package lives here instead.
package travelknowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

// Residence is one entry in data/locations.json with an inclusive validity
// window. Until == nil means open-ended.
type Residence struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Address string  `json:"address"`
	From    string  `json:"from"`
	Until   *string `json:"until"`

	from  time.Time
	until time.Time // zero when open-ended
}

// Work is the office entry in data/locations.json.
type Work struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Address string `json:"address"`
	Note    string `json:"note,omitempty"`
}

// Origins is the parsed, validated contents of data/locations.json.
type Origins struct {
	Residences []Residence `json:"residences"`
	Work       Work        `json:"work"`
}

// LoadOrigins reads and validates data/locations.json. Any violation is a
// load error: the caller (scheduler agent) refuses travel features but
// otherwise runs (SCHEDULER_TRAVEL_SPEC §1.6 degradation).
func LoadOrigins(path string) (*Origins, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read locations file: %w", err)
	}
	var origins Origins
	if err := json.Unmarshal(blob, &origins); err != nil {
		return nil, fmt.Errorf("decode locations file: %w", err)
	}
	if err := origins.validate(); err != nil {
		return nil, err
	}
	return &origins, nil
}

func (o *Origins) validate() error {
	seen := make(map[string]bool)
	for i := range o.Residences {
		r := &o.Residences[i]
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("residence %d: id is required", i)
		}
		if strings.TrimSpace(r.Label) == "" {
			return fmt.Errorf("residence %q: label is required", r.ID)
		}
		if strings.TrimSpace(r.Address) == "" {
			return fmt.Errorf("residence %q: address is required", r.ID)
		}
		from, err := time.Parse(dateLayout, strings.TrimSpace(r.From))
		if err != nil {
			return fmt.Errorf("residence %q: invalid from date %q", r.ID, r.From)
		}
		r.from = from
		if r.Until != nil {
			until, err := time.Parse(dateLayout, strings.TrimSpace(*r.Until))
			if err != nil {
				return fmt.Errorf("residence %q: invalid until date %q", r.ID, *r.Until)
			}
			if until.Before(from) {
				return fmt.Errorf("residence %q: until %q precedes from %q", r.ID, *r.Until, r.From)
			}
			r.until = until
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate location id %q", r.ID)
		}
		seen[r.ID] = true
	}
	// Validity windows must not overlap (inclusive day granularity).
	for i := range o.Residences {
		for j := i + 1; j < len(o.Residences); j++ {
			a, b := o.Residences[i], o.Residences[j]
			aOpen := a.until.IsZero()
			bOpen := b.until.IsZero()
			overlap := (aOpen || !a.until.Before(b.from)) && (bOpen || !b.until.Before(a.from))
			if overlap {
				return fmt.Errorf("residence windows overlap: %q and %q", a.ID, b.ID)
			}
		}
	}
	if strings.TrimSpace(o.Work.ID) == "" {
		return fmt.Errorf("work.id is required")
	}
	if seen[o.Work.ID] {
		return fmt.Errorf("duplicate location id %q", o.Work.ID)
	}
	return nil
}

// ResidenceFor returns the residence whose inclusive [from, until] window
// contains the given local date (until == nil means open-ended), or nil when
// no window contains it.
func (o *Origins) ResidenceFor(localDate time.Time) *Residence {
	day := time.Date(localDate.Year(), localDate.Month(), localDate.Day(), 0, 0, 0, 0, time.UTC)
	for i := range o.Residences {
		r := &o.Residences[i]
		if day.Before(r.from) {
			continue
		}
		if !r.until.IsZero() && day.After(r.until) {
			continue
		}
		return r
	}
	return nil
}

func (o *Origins) LabelForID(id string) (string, bool) {
	label, _, ok := o.LocationForID(id)
	return label, ok
}

func (o *Origins) LocationForID(id string) (string, string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", false
	}
	if o.Work.ID == id {
		return o.Work.Label, o.Work.Address, true
	}
	for i := range o.Residences {
		if o.Residences[i].ID == id {
			return o.Residences[i].Label, o.Residences[i].Address, true
		}
	}
	return "", "", false
}
