package travelknowledge

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func loadTestKnowledge(t *testing.T) *Knowledge {
	t.Helper()
	origins, err := LoadOrigins(writeTempJSON(t, validLocationsJSON))
	if err != nil {
		t.Fatalf("LoadOrigins() error = %v", err)
	}
	return &Knowledge{Origins: origins, Venues: loadTestVenues(t)}
}

func laTime(t *testing.T, value string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(TimeZone)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// TestOriginRule pins §1.5: origin selection evaluates at the DEPARTURE time
// (eventStart - default_travel_minutes = 30 min), not the meeting start.
// 2026-06-12 is a Friday; 2026-06-13 a Saturday. alto-cedro window is
// 2026-05-18..2026-06-30.
func TestOriginRule(t *testing.T) {
	t.Parallel()

	knowledge := loadTestKnowledge(t)
	offsite := "200 Medical Plaza"

	cases := []struct {
		name       string
		eventStart time.Time
		location   string
		wantOrigin string
		wantErr    bool
	}{
		{name: "0900 meeting departs 0830 from residence", eventStart: laTime(t, "2026-06-12T09:00:00"), location: offsite, wantOrigin: "alto-cedro"},
		{name: "weekday 1000 meeting departs from office", eventStart: laTime(t, "2026-06-12T10:00:00"), location: offsite, wantOrigin: "ucla-tdg-office"},
		{name: "weekday 1815 meeting departs 1745 from office", eventStart: laTime(t, "2026-06-12T18:15:00"), location: offsite, wantOrigin: "ucla-tdg-office"},
		{name: "weekday 1845 meeting departs 1815 from residence", eventStart: laTime(t, "2026-06-12T18:45:00"), location: offsite, wantOrigin: "alto-cedro"},
		{name: "saturday departs from residence", eventStart: laTime(t, "2026-06-13T10:00:00"), location: offsite, wantOrigin: "alto-cedro"},
		{name: "destination office departs from residence", eventStart: laTime(t, "2026-06-12T10:00:00"), location: "UCLA TDG office", wantOrigin: "alto-cedro"},
		{name: "no residence valid on a weekday falls back to office", eventStart: laTime(t, "2026-04-01T07:00:00"), location: offsite, wantOrigin: "ucla-tdg-office"},
		{name: "date before all windows on a weekend is no_origin", eventStart: laTime(t, "2026-04-04T10:00:00"), location: offsite, wantErr: true},
	}
	for _, tc := range cases {
		est, err := knowledge.Estimate(tc.eventStart, tc.location)
		if tc.wantErr {
			if !errors.Is(err, ErrNoOrigin) {
				t.Errorf("%s: Estimate() error = %v, want ErrNoOrigin", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Estimate() error = %v", tc.name, err)
			continue
		}
		if est.OriginID != tc.wantOrigin {
			t.Errorf("%s: origin = %q, want %q", tc.name, est.OriginID, tc.wantOrigin)
		}
	}

	// Both origins absent => no_origin even on a weekday.
	bare := &Knowledge{Origins: &Origins{}, Venues: knowledge.Venues}
	if _, err := bare.Estimate(laTime(t, "2026-06-12T10:00:00"), offsite); !errors.Is(err, ErrNoOrigin) {
		t.Errorf("both absent: Estimate() error = %v, want ErrNoOrigin", err)
	}
	// Destination office with no valid residence => no_origin (rule 3).
	if _, err := knowledge.Estimate(laTime(t, "2026-04-04T10:00:00"), "UCLA TDG office"); !errors.Is(err, ErrNoOrigin) {
		t.Errorf("office destination without residence: error = %v, want ErrNoOrigin", err)
	}
}

func TestEstimate(t *testing.T) {
	t.Parallel()

	knowledge := loadTestKnowledge(t)
	officeOriginStart := laTime(t, "2026-06-12T10:00:00") // departs 09:30 weekday => office origin

	t.Run("matrix hit", func(t *testing.T) {
		est, err := knowledge.Estimate(officeOriginStart, "200 Medical Plaza")
		if err != nil {
			t.Fatalf("Estimate() error = %v", err)
		}
		if est.DriveMinutes != 5 || est.WalkMinutes != 10 || est.Minutes != 15 {
			t.Fatalf("estimate = %#v, want drive 5 walk 10 total 15", est)
		}
		if est.Source != "matrix" || est.VenueID != "200-medical-plaza" || est.Parking == "" {
			t.Fatalf("estimate = %#v", est)
		}
		if est.IsOffice || est.IsVirtual {
			t.Fatalf("estimate flags = %#v", est)
		}
	})

	t.Run("venue hit with missing origin key falls back to default drive plus venue walk", func(t *testing.T) {
		trimmed := strings.Replace(validVenuesJSON, `"ucla-tdg-office": 5, `, "", 1)
		venues, err := LoadVenues(writeTempJSON(t, trimmed))
		if err != nil {
			t.Fatalf("LoadVenues() error = %v", err)
		}
		k := &Knowledge{Origins: knowledge.Origins, Venues: venues}
		est, err := k.Estimate(officeOriginStart, "200 Medical Plaza")
		if err != nil {
			t.Fatalf("Estimate() error = %v", err)
		}
		if est.DriveMinutes != 30 || est.WalkMinutes != 10 || est.Source != "default" {
			t.Fatalf("estimate = %#v, want default 30 + walk 10", est)
		}
	})

	t.Run("explicit origin and return leg use directed matrix", func(t *testing.T) {
		outbound, err := knowledge.EstimateFromOrigin(officeOriginStart, "200 Medical Plaza", "alto-cedro")
		if err != nil {
			t.Fatalf("EstimateFromOrigin() error = %v", err)
		}
		if outbound.OriginID != "alto-cedro" || outbound.DriveMinutes != 20 || outbound.WalkMinutes != 10 || outbound.Minutes != 30 {
			t.Fatalf("outbound estimate = %#v, want alto-cedro 20 + 10", outbound)
		}
		if outbound.OriginAddress != "9121 Alto Cedro Drive" {
			t.Fatalf("outbound origin address = %q", outbound.OriginAddress)
		}

		returnLeg, err := knowledge.EstimateReturnToOrigin(officeOriginStart.Add(time.Hour), "200 Medical Plaza", "alto-cedro")
		if err != nil {
			t.Fatalf("EstimateReturnToOrigin() error = %v", err)
		}
		if returnLeg.OriginID != "alto-cedro" || returnLeg.DriveMinutes != 18 || returnLeg.WalkMinutes != 6 || returnLeg.Minutes != 24 {
			t.Fatalf("return estimate = %#v, want alto-cedro 18 + 6", returnLeg)
		}
		if returnLeg.OriginAddress != "9121 Alto Cedro Drive" {
			t.Fatalf("return target address = %q", returnLeg.OriginAddress)
		}
	})

	t.Run("unmatched location uses default and zero walk", func(t *testing.T) {
		est, err := knowledge.Estimate(officeOriginStart, "Cedars-Sinai Medical Center")
		if err != nil {
			t.Fatalf("Estimate() error = %v", err)
		}
		if est.DriveMinutes != 30 || est.WalkMinutes != 0 || est.Minutes != 30 || est.Source != "default" {
			t.Fatalf("estimate = %#v", est)
		}
		if est.VenueID != "" || est.VenueName != "" || est.Parking != "" {
			t.Fatalf("unmatched estimate carries venue: %#v", est)
		}
		if est.Minutes < 1 {
			t.Fatalf("non-virtual estimate must have Minutes >= 1: %#v", est)
		}
	})

	t.Run("virtual location yields zeros", func(t *testing.T) {
		est, err := knowledge.Estimate(officeOriginStart, "Microsoft Teams Meeting")
		if err != nil {
			t.Fatalf("Estimate() error = %v", err)
		}
		if !est.IsVirtual || est.Source != "virtual" {
			t.Fatalf("estimate = %#v", est)
		}
		if est.Minutes != 0 || est.DriveMinutes != 0 || est.WalkMinutes != 0 || est.OriginID != "" || est.VenueID != "" {
			t.Fatalf("virtual estimate must be zeroed: %#v", est)
		}
	})

	t.Run("block minutes rounding", func(t *testing.T) {
		cases := []struct {
			minutes int
			want    int
		}{
			{minutes: 4, want: 10},  // floor 10
			{minutes: 10, want: 10}, // exact multiple
			{minutes: 15, want: 15}, // matrix example
			{minutes: 16, want: 20}, // round up to 5
			{minutes: 118, want: 120},
			{minutes: 150, want: 120}, // cap 120
		}
		for _, tc := range cases {
			if got := BlockMinutes(Estimate{Minutes: tc.minutes}); got != tc.want {
				t.Errorf("BlockMinutes(%d) = %d, want %d", tc.minutes, got, tc.want)
			}
		}
	})
}
