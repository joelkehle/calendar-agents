package main

import "testing"

func TestEnvHelpers(t *testing.T) {
	t.Setenv("OUTLOOK_CALENDAR_HTTP_ADDR", "")
	if got := envOrDefault("OUTLOOK_CALENDAR_HTTP_ADDR", ":8220"); got != ":8220" {
		t.Fatalf("envOrDefault() = %q", got)
	}
	t.Setenv("OUTLOOK_CALENDAR_HTTP_ADDR", ":9000")
	if got := envOrDefault("OUTLOOK_CALENDAR_HTTP_ADDR", ":8220"); got != ":9000" {
		t.Fatalf("envOrDefault() = %q", got)
	}

	t.Setenv("OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS", "true")
	if !envBool("OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS", false) {
		t.Fatal("envBool() = false")
	}
	t.Setenv("OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS", "")
	if !envBool("OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS", true) {
		t.Fatal("private details default should be true")
	}
}
