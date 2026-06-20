package travelknowledge

import (
	"strings"
	"testing"
)

const validVenuesJSON = `{
  "schema": "venues.v1",
  "default_travel_minutes": 30,
  "office_aliases": ["10889 wilshire", "wilshire center", "ucla tdg"],
  "virtual_aliases": ["http://", "https://", "microsoft teams", "teams meeting",
                      "zoom", "webex", "meet.google", "dial-in", "conference call", "messenger"],
  "venues": [
    {
      "id": "200-medical-plaza",
      "name": "200 Medical Plaza",
      "match": ["200 medical plaza", "200 med plaza"],
      "walk_minutes": 10,
      "return_buffer_minutes": 6,
      "parking": "Patient drop-off loop.",
      "travel_minutes": {"ucla-tdg-office": 5, "alto-cedro": 20, "orange-drive": 25},
      "return_minutes": {"alto-cedro": 18}
    },
    {
      "id": "ucla-tdg-office",
      "name": "UCLA TDG office",
      "match": ["10889 wilshire", "wilshire center", "ucla tdg"],
      "walk_minutes": 5,
      "parking": "UNVERIFIED.",
      "travel_minutes": {"alto-cedro": 20, "orange-drive": 20}
    }
  ]
}`

func loadTestVenues(t *testing.T) *Venues {
	t.Helper()
	venues, err := LoadVenues(writeTempJSON(t, validVenuesJSON))
	if err != nil {
		t.Fatalf("LoadVenues() error = %v", err)
	}
	return venues
}

func TestLoadVenues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{name: "valid seed", mutate: func(s string) string { return s }},
		{
			name:    "bad schema string",
			mutate:  func(s string) string { return strings.Replace(s, `"venues.v1"`, `"venues.v2"`, 1) },
			wantErr: "unsupported venues schema",
		},
		{
			name: "duplicate venue ids",
			mutate: func(s string) string {
				return strings.Replace(s, `"id": "200-medical-plaza"`, `"id": "ucla-tdg-office"`, 1)
			},
			wantErr: "duplicate venue id",
		},
		{
			name:    "match alias under 4 runes",
			mutate:  func(s string) string { return strings.Replace(s, `"200 med plaza"`, `"2mp"`, 1) },
			wantErr: "match alias",
		},
		{
			name: "office alias under 6 runes",
			mutate: func(s string) string {
				return strings.Replace(s, `"office_aliases": ["10889 wilshire"`, `"office_aliases": ["offic"`, 1)
			},
			wantErr: "office alias",
		},
		{
			name:    "virtual alias under 4 runes",
			mutate:  func(s string) string { return strings.Replace(s, `"zoom"`, `"zm"`, 1) },
			wantErr: "virtual alias",
		},
		{
			name: "default minutes out of range",
			mutate: func(s string) string {
				return strings.Replace(s, `"default_travel_minutes": 30`, `"default_travel_minutes": 200`, 1)
			},
			wantErr: "default_travel_minutes",
		},
		{
			name:    "matrix value out of range",
			mutate:  func(s string) string { return strings.Replace(s, `"ucla-tdg-office": 5,`, `"ucla-tdg-office": 0,`, 1) },
			wantErr: "travel_minutes",
		},
		{
			name:    "return matrix value out of range",
			mutate:  func(s string) string { return strings.Replace(s, `"alto-cedro": 18`, `"alto-cedro": 181`, 1) },
			wantErr: "return_minutes",
		},
		{
			name: "return buffer out of range",
			mutate: func(s string) string {
				return strings.Replace(s, `"return_buffer_minutes": 6`, `"return_buffer_minutes": 61`, 1)
			},
			wantErr: "return_buffer_minutes",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadVenues(writeTempJSON(t, tc.mutate(validVenuesJSON)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadVenues() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadVenues() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestMatchVenue(t *testing.T) {
	t.Parallel()

	venues := loadTestVenues(t)
	cases := []struct {
		name     string
		location string
		wantID   string
	}{
		{name: "exact alias", location: "200 medical plaza", wantID: "200-medical-plaza"},
		{name: "alias substring inside longer location", location: "Suite 540, 200 Medical Plaza, Westwood", wantID: "200-medical-plaza"},
		{name: "case and whitespace normalization", location: "  200   MEDICAL\nPLAZA  ", wantID: "200-medical-plaza"},
		{name: "no match", location: "Cedars-Sinai", wantID: ""},
		{name: "empty location", location: "", wantID: ""},
		{name: "whitespace-only location", location: "   \n  ", wantID: ""},
	}
	for _, tc := range cases {
		got := venues.MatchVenue(tc.location)
		gotID := ""
		if got != nil {
			gotID = got.ID
		}
		if gotID != tc.wantID {
			t.Errorf("%s: MatchVenue(%q) = %q, want %q", tc.name, tc.location, gotID, tc.wantID)
		}
	}

	// First match in file order wins: give both venues an overlapping alias.
	overlapping := strings.Replace(validVenuesJSON, `"match": ["200 medical plaza", "200 med plaza"]`,
		`"match": ["shared alias", "200 medical plaza"]`, 1)
	overlapping = strings.Replace(overlapping, `"match": ["10889 wilshire", "wilshire center", "ucla tdg"]`,
		`"match": ["shared alias"]`, 1)
	ordered, err := LoadVenues(writeTempJSON(t, overlapping))
	if err != nil {
		t.Fatalf("LoadVenues(overlapping) error = %v", err)
	}
	if got := ordered.MatchVenue("shared alias somewhere"); got == nil || got.ID != "200-medical-plaza" {
		t.Fatalf("first-match-wins: MatchVenue = %#v, want 200-medical-plaza", got)
	}
}

func TestIsOffice(t *testing.T) {
	t.Parallel()

	venues := loadTestVenues(t)
	for _, alias := range []string{"10889 Wilshire Blvd", "Wilshire Center conference room", "UCLA TDG office"} {
		if !venues.IsOffice(alias) {
			t.Errorf("IsOffice(%q) = false, want true", alias)
		}
	}
	if venues.IsOffice("Marc's office") {
		t.Error(`IsOffice("Marc's office") = true, want false`)
	}
	if venues.IsOffice("") {
		t.Error(`IsOffice("") = true, want false`)
	}
}

func TestIsVirtual(t *testing.T) {
	t.Parallel()

	venues := loadTestVenues(t)
	for _, location := range []string{
		"Microsoft Teams Meeting",
		"https://ucla.zoom.us/j/123456789",
		"https://example.com/meeting",
		"Dial-In: +1 555 0100",
		"Messenger",
	} {
		if !venues.IsVirtual(location) {
			t.Errorf("IsVirtual(%q) = false, want true", location)
		}
	}
	if venues.IsVirtual("200 Medical Plaza, Los Angeles") {
		t.Error("IsVirtual(street address) = true, want false")
	}
	if venues.IsVirtual("") {
		t.Error(`IsVirtual("") = true, want false`)
	}
}
