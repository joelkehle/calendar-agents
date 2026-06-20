package travelknowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const VenuesSchema = "venues.v1"

// Venue is one destination in data/venues.json. TravelMinutes is keyed by
// origin location id (data/locations.json ids); missing keys fall back to the
// file-level default (SCHEDULER_TRAVEL_SPEC §1.5).
type Venue struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Address             string         `json:"address,omitempty"`
	Match               []string       `json:"match"`
	WalkMinutes         int            `json:"walk_minutes"`
	ReturnBufferMinutes *int           `json:"return_buffer_minutes,omitempty"`
	Parking             string         `json:"parking,omitempty"`
	TravelMinutes       map[string]int `json:"travel_minutes,omitempty"`
	ReturnMinutes       map[string]int `json:"return_minutes,omitempty"`
}

// Venues is the parsed, validated contents of data/venues.json.
type Venues struct {
	Schema               string   `json:"schema"`
	DefaultTravelMinutes int      `json:"default_travel_minutes"`
	OfficeAliases        []string `json:"office_aliases"`
	VirtualAliases       []string `json:"virtual_aliases"`
	Venues               []Venue  `json:"venues"`
}

// LoadVenues reads and validates data/venues.json.
func LoadVenues(path string) (*Venues, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read venues file: %w", err)
	}
	var venues Venues
	if err := json.Unmarshal(blob, &venues); err != nil {
		return nil, fmt.Errorf("decode venues file: %w", err)
	}
	if err := venues.validate(); err != nil {
		return nil, err
	}
	return &venues, nil
}

func (v *Venues) validate() error {
	if v.Schema != VenuesSchema {
		return fmt.Errorf("unsupported venues schema %q (want %q)", v.Schema, VenuesSchema)
	}
	if v.DefaultTravelMinutes < 5 || v.DefaultTravelMinutes > 180 {
		return fmt.Errorf("default_travel_minutes %d out of range [5, 180]", v.DefaultTravelMinutes)
	}
	for _, alias := range v.OfficeAliases {
		if utf8.RuneCountInString(normalize(alias)) < 6 {
			return fmt.Errorf("office alias %q too short (min 6 runes; guards against substring false positives)", alias)
		}
	}
	for _, alias := range v.VirtualAliases {
		if utf8.RuneCountInString(normalize(alias)) < 4 {
			return fmt.Errorf("virtual alias %q too short (min 4 runes)", alias)
		}
	}
	seen := make(map[string]bool)
	for i, venue := range v.Venues {
		if strings.TrimSpace(venue.ID) == "" {
			return fmt.Errorf("venue %d: id is required", i)
		}
		if seen[venue.ID] {
			return fmt.Errorf("duplicate venue id %q", venue.ID)
		}
		seen[venue.ID] = true
		if strings.TrimSpace(venue.Name) == "" {
			return fmt.Errorf("venue %q: name is required", venue.ID)
		}
		for _, alias := range venue.Match {
			if utf8.RuneCountInString(normalize(alias)) < 4 {
				return fmt.Errorf("venue %q: match alias %q too short (min 4 runes)", venue.ID, alias)
			}
		}
		if venue.WalkMinutes < 0 || venue.WalkMinutes > 60 {
			return fmt.Errorf("venue %q: walk_minutes %d out of range [0, 60]", venue.ID, venue.WalkMinutes)
		}
		for origin, minutes := range venue.TravelMinutes {
			if minutes < 1 || minutes > 180 {
				return fmt.Errorf("venue %q: travel_minutes[%q] = %d out of range [1, 180]", venue.ID, origin, minutes)
			}
		}
		for origin, minutes := range venue.ReturnMinutes {
			if minutes < 1 || minutes > 180 {
				return fmt.Errorf("venue %q: return_minutes[%q] = %d out of range [1, 180]", venue.ID, origin, minutes)
			}
		}
		if venue.ReturnBufferMinutes != nil && (*venue.ReturnBufferMinutes < 0 || *venue.ReturnBufferMinutes > 60) {
			return fmt.Errorf("venue %q: return_buffer_minutes %d out of range [0, 60]", venue.ID, *venue.ReturnBufferMinutes)
		}
	}
	return nil
}

// normalize lowercases, collapses all whitespace runs (incl. newlines) to a
// single space, and trims (SCHEDULER_TRAVEL_SPEC §1.4).
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// IsVirtual reports whether the location string matches any virtual alias.
// Checked FIRST wherever "is this offsite?" is the question: Outlook
// auto-populates Location with Teams/Zoom/Webex strings and join URLs, which
// must never be treated as places Joel drives to.
func (v *Venues) IsVirtual(location string) bool {
	return matchesAnyAlias(location, v.VirtualAliases)
}

// IsOffice reports whether the location string matches any office alias.
func (v *Venues) IsOffice(location string) bool {
	return matchesAnyAlias(location, v.OfficeAliases)
}

// MatchVenue returns the first venue (file order) one of whose match aliases
// is a substring of the normalized location, or nil. No fuzzy matching.
func (v *Venues) MatchVenue(location string) *Venue {
	normalized := normalize(location)
	if normalized == "" {
		return nil
	}
	for i := range v.Venues {
		for _, alias := range v.Venues[i].Match {
			aliasNorm := normalize(alias)
			if aliasNorm != "" && strings.Contains(normalized, aliasNorm) {
				return &v.Venues[i]
			}
		}
	}
	return nil
}

func matchesAnyAlias(location string, aliases []string) bool {
	normalized := normalize(location)
	if normalized == "" {
		return false
	}
	for _, alias := range aliases {
		aliasNorm := normalize(alias)
		if aliasNorm != "" && strings.Contains(normalized, aliasNorm) {
			return true
		}
	}
	return false
}
