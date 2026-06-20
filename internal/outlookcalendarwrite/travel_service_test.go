package outlookcalendarwrite

import (
	"strings"
	"testing"
)

func TestPowerShellScriptAddsTravelDefense(t *testing.T) {
	t.Parallel()

	required := []string{
		`function Test-TravelMarker($body) {`,
		`hold_class=travel-block`,
		`$isTravelPayload = Test-TravelMarker ([string]$payload.description)`,
		// Insert and patch conflict gates extended to travel-marked payloads.
		`if (((Test-HoldMarker ([string]$payload.description)) -or $isTravelPayload) -and [string]$payload.show_as -ne "free") {`,
		`Assert-NoBusyConflict $namespace "" $payload.start.date_time $payload.end.date_time $isTravelPayload`,
		`Assert-NoBusyConflict $namespace ([string]$item.EntryID) $payload.start.date_time $payload.end.date_time $isTravelPayload`,
		`if ($allowMeetingBuffer -and (Test-MeetingBufferPlaceholder $candidate)) { continue }`,
		// Patch ownership accepts hold or travel markers; the guard refusal
		// stays the fallback.
		`$hasManagedOwnership = (Test-HoldMarker $body) -or (Test-TravelMarker $body)`,
		// Stale-snapshot check on snapshot-checked patches.
		`JK_OUTLOOK_WRITE_EXPECT_START`,
		`JK_OUTLOOK_WRITE_EXPECT_END`,
		`conflict: stale snapshot `,
	}
	for _, text := range required {
		if !strings.Contains(outlookWritePowerShell, text) {
			t.Fatalf("PowerShell script missing %q", text)
		}
	}
	if strings.Contains(outlookWritePowerShell, "$ranges.Add($candidate.Subject") {
		t.Fatal("PowerShell conflict errors must not include overlapping subjects")
	}

	// PowerShellService must implement the optional snapshot-checked
	// capability interface.
	var service Service = NewPowerShellService()
	if _, ok := service.(SnapshotCheckedService); !ok {
		t.Fatal("PowerShellService does not implement SnapshotCheckedService")
	}
}

func TestPowerShellScriptTagsManagedEventCategories(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`if ($env:JK_OUTLOOK_WRITE_TRAVEL_CATEGORIES) { $item.Categories = $env:JK_OUTLOOK_WRITE_TRAVEL_CATEGORIES }`,
		`if ($env:JK_OUTLOOK_WRITE_HOLD_CATEGORIES) { $item.Categories = $env:JK_OUTLOOK_WRITE_HOLD_CATEGORIES }`,
	} {
		if !strings.Contains(outlookWritePowerShell, want) {
			t.Fatalf("PowerShell script missing category tagging line %q", want)
		}
	}
}
