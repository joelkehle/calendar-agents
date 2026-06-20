package main

import "testing"

func TestEnvHelpers(t *testing.T) {
	t.Setenv("OUTLOOK_CALENDAR_WRITE_HTTP_ADDR", "")
	if got := envOrDefault("OUTLOOK_CALENDAR_WRITE_HTTP_ADDR", ":8219"); got != ":8219" {
		t.Fatalf("envOrDefault() = %q", got)
	}
	t.Setenv("OUTLOOK_CALENDAR_WRITE_HTTP_ADDR", ":9001")
	if got := envOrDefault("OUTLOOK_CALENDAR_WRITE_HTTP_ADDR", ":8219"); got != ":9001" {
		t.Fatalf("envOrDefault() = %q", got)
	}

	t.Setenv("OUTLOOK_CALENDAR_WRITE_DRY_RUN", "false")
	if envBool("OUTLOOK_CALENDAR_WRITE_DRY_RUN", true) {
		t.Fatal("envBool() = true")
	}
}
