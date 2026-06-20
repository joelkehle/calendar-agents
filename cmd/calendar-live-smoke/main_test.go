package main

import (
	"strings"
	"testing"
)

func TestValidateEvents(t *testing.T) {
	if err := validateEvents(`{"events":[]}`); err != nil {
		t.Fatalf("validateEvents empty response: %v", err)
	}
	if err := validateEvents(`{"error":"calendar unavailable"}`); err == nil || !strings.Contains(err.Error(), "calendar unavailable") {
		t.Fatalf("validateEvents error = %v, want calendar unavailable", err)
	}
}

func TestValidateEstimate(t *testing.T) {
	body := `{"status":"estimated","request_id":"r1","estimate":{"minutes":30,"drive_minutes":20,"walk_minutes":10,"source":"matrix","is_office":false,"is_virtual":false}}`
	if err := validateEstimate(body); err != nil {
		t.Fatalf("validateEstimate success: %v", err)
	}
	if err := validateEstimate(`{"status":"error","error_code":"estimate_unavailable"}`); err == nil || !strings.Contains(err.Error(), "estimate_unavailable") {
		t.Fatalf("validateEstimate error = %v, want estimate_unavailable", err)
	}
}

func TestValidateRefusal(t *testing.T) {
	if err := validateRefusal(`{"dry_run":false,"error_code":"not_allowlisted","error":"requesting agent is not allowlisted"}`); err != nil {
		t.Fatalf("validateRefusal success: %v", err)
	}
	if err := validateRefusal(`{"dry_run":false,"event":{"id":"evt"}}`); err == nil || !strings.Contains(err.Error(), "error_code") {
		t.Fatalf("validateRefusal event = %v, want error_code failure", err)
	}
}

func TestParseConfigRequiresSecret(t *testing.T) {
	t.Setenv("CALENDAR_SMOKE_AGENT_SECRET", "")
	_, err := parseConfig([]string{"-date", "2026-06-20"})
	if err == nil || !strings.Contains(err.Error(), "CALENDAR_SMOKE_AGENT_SECRET") {
		t.Fatalf("parseConfig err = %v, want missing secret", err)
	}
}

func TestBuildChecksCanSkipSubsets(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-secret", "secret",
		"-date", "2026-06-20",
		"-skip-reads",
		"-skip-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	checks, err := buildChecks(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].name != "scheduler-estimate" {
		t.Fatalf("checks = %#v, want scheduler-estimate only", checks)
	}
}
