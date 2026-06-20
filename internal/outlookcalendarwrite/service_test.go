package outlookcalendarwrite

import (
	"strings"
	"testing"
)

func TestHostTimeZoneMatchesDefaultWindowsPacificID(t *testing.T) {
	t.Parallel()

	for _, value := range []string{DefaultTimeZone, "Pacific Standard Time", "US Pacific Standard Time"} {
		if !HostTimeZoneMatches(value, DefaultTimeZone) {
			t.Fatalf("HostTimeZoneMatches(%q, %q) = false", value, DefaultTimeZone)
		}
	}
	if HostTimeZoneMatches("Eastern Standard Time", DefaultTimeZone) {
		t.Fatal("Eastern Standard Time matched DefaultTimeZone")
	}
}

func TestPowerShellScriptKeepsGuardDefenseAndAddsHoldConflictCheck(t *testing.T) {
	t.Parallel()

	required := []string{
		`if (-not $body.Contains("managed_by=jk-calendar-guard-agent") -or -not $body.Contains("owner_agent=ucla-tdg-outlook-calendar-write-agent"))`,
		`Test-HoldMarker $body`,
		`function Test-GuardOwnershipMarker($body) {`,
		`function Test-MeetingBufferPlaceholder($item) {`,
		`if ([string]$line -eq "managed_by=jk-calendar-guard-agent")`,
		`} elseif ([string]$line -eq "owner_agent=ucla-tdg-outlook-calendar-write-agent")`,
		`Assert-NoBusyConflict`,
		`if (Test-GuardOwnershipMarker $candidateBody) { continue }`,
		`if ($allowMeetingBuffer -and (Test-MeetingBufferPlaceholder $candidate)) { continue }`,
		`throw ("conflict: " + [string]::Join(", ", $ranges))`,
		`location = [string]$item.Location`,
		`if ($payload.PSObject.Properties.Name -contains "location") {`,
		`$item.Location = [string]$payload.location`,
	}
	for _, text := range required {
		if !strings.Contains(outlookWritePowerShell, text) {
			t.Fatalf("PowerShell script missing %q", text)
		}
	}
	if strings.Contains(outlookWritePowerShell, "$ranges.Add($candidate.Subject") {
		t.Fatal("PowerShell conflict errors must not include overlapping subjects")
	}
}
