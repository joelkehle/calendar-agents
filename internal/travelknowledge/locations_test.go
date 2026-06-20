package travelknowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

const validLocationsJSON = `{
  "residences": [
    {"id": "alto-cedro", "label": "Monte & Jacqueline's", "address": "9121 Alto Cedro Drive", "from": "2026-05-18", "until": "2026-06-30"},
    {"id": "orange-drive", "label": "Home", "address": "322 S. Orange Drive", "from": "2026-07-01", "until": null}
  ],
  "work": {"id": "ucla-tdg-office", "label": "UCLA TDG office", "address": "10889 Wilshire Blvd"}
}`

func TestLoadLocations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		json    string
		wantErr string
	}{
		{name: "valid file", json: validLocationsJSON},
		{
			name: "open-ended until",
			json: `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`,
		},
		{
			name:    "missing id",
			json:    `{"residences":[{"label":"A","address":"addr","from":"2026-01-01","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`,
			wantErr: "id is required",
		},
		{
			name:    "duplicate ids",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":"2026-01-31"},{"id":"a","label":"B","address":"addr2","from":"2026-02-01","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`,
			wantErr: "duplicate location id",
		},
		{
			name:    "work id duplicates residence id",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":null}],"work":{"id":"a","label":"W","address":"waddr"}}`,
			wantErr: "duplicate location id",
		},
		{
			name:    "overlapping residence windows",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":"2026-02-15"},{"id":"b","label":"B","address":"addr2","from":"2026-02-15","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`,
			wantErr: "overlap",
		},
		{
			name:    "bad date",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"not-a-date","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`,
			wantErr: "invalid from date",
		},
		{
			name:    "until precedes from",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-03-01","until":"2026-02-01"}],"work":{"id":"w","label":"W","address":"waddr"}}`,
			wantErr: "precedes from",
		},
		{
			name:    "missing work id",
			json:    `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":null}],"work":{"label":"W","address":"waddr"}}`,
			wantErr: "work.id is required",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			origins, err := LoadOrigins(writeTempJSON(t, tc.json))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadOrigins() error = %v", err)
				}
				if origins == nil {
					t.Fatal("LoadOrigins() returned nil origins")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadOrigins() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResidenceForDate(t *testing.T) {
	t.Parallel()

	origins, err := LoadOrigins(writeTempJSON(t, validLocationsJSON))
	if err != nil {
		t.Fatalf("LoadOrigins() error = %v", err)
	}
	loc, _ := time.LoadLocation(TimeZone)

	cases := []struct {
		name   string
		date   time.Time
		wantID string
	}{
		{name: "inside first window", date: time.Date(2026, 6, 1, 12, 0, 0, 0, loc), wantID: "alto-cedro"},
		{name: "from boundary inclusive", date: time.Date(2026, 5, 18, 0, 30, 0, 0, loc), wantID: "alto-cedro"},
		{name: "until boundary inclusive", date: time.Date(2026, 6, 30, 23, 30, 0, 0, loc), wantID: "alto-cedro"},
		{name: "before all windows", date: time.Date(2026, 4, 1, 12, 0, 0, 0, loc), wantID: ""},
		{name: "after open-ended from", date: time.Date(2027, 3, 15, 9, 0, 0, 0, loc), wantID: "orange-drive"},
	}
	for _, tc := range cases {
		got := origins.ResidenceFor(tc.date)
		gotID := ""
		if got != nil {
			gotID = got.ID
		}
		if gotID != tc.wantID {
			t.Errorf("%s: ResidenceFor(%s) = %q, want %q", tc.name, tc.date, gotID, tc.wantID)
		}
	}

	// Gap between windows => none.
	gap := `{"residences":[{"id":"a","label":"A","address":"addr","from":"2026-01-01","until":"2026-01-31"},{"id":"b","label":"B","address":"addr2","from":"2026-03-01","until":null}],"work":{"id":"w","label":"W","address":"waddr"}}`
	gapped, err := LoadOrigins(writeTempJSON(t, gap))
	if err != nil {
		t.Fatalf("LoadOrigins(gap) error = %v", err)
	}
	if got := gapped.ResidenceFor(time.Date(2026, 2, 14, 12, 0, 0, 0, loc)); got != nil {
		t.Fatalf("ResidenceFor(gap date) = %#v, want nil", got)
	}
}
