package outlookcalendarwrite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const outlookWritePowerShell = `
$ErrorActionPreference = "Stop"
[System.Threading.Thread]::CurrentThread.CurrentCulture = [System.Globalization.CultureInfo]::GetCultureInfo("en-US")

function Parse-EventTime($value) {
  return [DateTimeOffset]::Parse([string]$value, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::RoundtripKind).LocalDateTime
}

function Parse-EventDate($value) {
  return [DateTime]::ParseExact([string]$value, "yyyy-MM-dd", [System.Globalization.CultureInfo]::InvariantCulture)
}

function ShowAs-ToBusyStatus($showAs) {
  switch ([string]$showAs) {
    "free" { return 0 }
    "tentative" { return 1 }
    "busy" { return 2 }
    "oof" { return 3 }
    default { return 2 }
  }
}

function Description-Lines($value) {
  $crlf = [string][char]13 + [string][char]10
  $lf = [string][char]10
  $cr = [string][char]13
  # Outlook appends trailing whitespace to body lines on round-trip; trim so
  # marker matching survives (observed live 2026-06-11).
  return (([string]$value).Replace($crlf, $lf).Replace($cr, $lf)).Split($lf) | ForEach-Object { ([string]$_).Trim() }
}

function Test-HoldMarker($body) {
  $lines = Description-Lines $body
  for ($i = 0; $i -le ($lines.Count - 3); $i++) {
    $line = [string]$lines[$i]
    if (-not $line.StartsWith("managed_by=")) { continue }
    $requester = $line.Substring(11)
    if ($requester.Length -eq 0) { continue }
    if ([string]$lines[$i + 1] -eq "owner_agent=ucla-tdg-outlook-calendar-write-agent" -and [string]$lines[$i + 2] -eq "hold_class=working-hold") {
      return $true
    }
  }
  return $false
}

function Test-TravelMarker($body) {
  $lines = Description-Lines $body
  for ($i = 0; $i -le ($lines.Count - 3); $i++) {
    $line = [string]$lines[$i]
    if (-not $line.StartsWith("managed_by=")) { continue }
    $requester = $line.Substring(11)
    if ($requester.Length -eq 0) { continue }
    if ([string]$lines[$i + 1] -eq "owner_agent=ucla-tdg-outlook-calendar-write-agent" -and [string]$lines[$i + 2] -eq "hold_class=travel-block") {
      return $true
    }
  }
  return $false
}

function Test-GuardOwnershipMarker($body) {
  $hasManagedBy = $false
  $hasOwnerAgent = $false
  foreach ($line in (Description-Lines $body)) {
    if ([string]$line -eq "managed_by=jk-calendar-guard-agent") {
      $hasManagedBy = $true
    } elseif ([string]$line -eq "owner_agent=ucla-tdg-outlook-calendar-write-agent") {
      $hasOwnerAgent = $true
    }
  }
  return ($hasManagedBy -and $hasOwnerAgent)
}

function Test-MeetingBufferPlaceholder($item) {
  try {
    return ([string]$item.Subject).Trim() -eq "Meeting Buffer"
  } catch {
    return $false
  }
}

function Assert-NoBusyConflict($namespace, $excludeEntryId, $startValue, $endValue, $allowMeetingBuffer) {
  if (-not $startValue -or -not $endValue) { return }
  $rangeStart = Parse-EventTime $startValue
  $rangeEnd = Parse-EventTime $endValue
  $calendar = $namespace.GetDefaultFolder(9)
  $items = $calendar.Items
  $items.IncludeRecurrences = $true
  $items.Sort("[Start]")
  $startFilter = $rangeStart.ToString("MM/dd/yyyy hh:mm tt", [System.Globalization.CultureInfo]::InvariantCulture)
  $endFilter = $rangeEnd.ToString("MM/dd/yyyy hh:mm tt", [System.Globalization.CultureInfo]::InvariantCulture)
  $filter = "[End] > '$startFilter' AND [Start] < '$endFilter'"
  try {
    $candidates = $items.Restrict($filter)
  } catch {
    $candidates = $items
  }
  $ranges = New-Object System.Collections.Generic.List[string]
  foreach ($candidate in $candidates) {
    try {
      if ($excludeEntryId -and [string]$candidate.EntryID -eq [string]$excludeEntryId) { continue }
      if ([int]$candidate.BusyStatus -eq 0) { continue }
      $candidateStart = [DateTime]$candidate.Start
      $candidateEnd = [DateTime]$candidate.End
      if ($candidateEnd -gt $rangeStart -and $candidateStart -lt $rangeEnd) {
        $candidateBody = ""
        try {
          $candidateBody = [string]$candidate.Body
        } catch {
          $candidateBody = ""
        }
        if (Test-GuardOwnershipMarker $candidateBody) { continue }
        if ($allowMeetingBuffer -and (Test-MeetingBufferPlaceholder $candidate)) { continue }
        $ranges.Add($candidateStart.ToString("o") + "/" + $candidateEnd.ToString("o"))
      }
    } catch {}
  }
  if ($ranges.Count -gt 0) {
    throw ("conflict: " + [string]::Join(", ", $ranges))
  }
}

function Event-FromItem($item) {
  $showAs = "busy"
  try {
    if ([int]$item.BusyStatus -eq 0) { $showAs = "free" }
    elseif ([int]$item.BusyStatus -eq 1) { $showAs = "tentative" }
    elseif ([int]$item.BusyStatus -eq 3) { $showAs = "oof" }
  } catch {}

  $start = @{}
  $end = @{}
  if ([bool]$item.AllDayEvent) {
    $start = @{
      date = ([DateTime]$item.Start).ToString("yyyy-MM-dd")
      time_zone = $env:JK_OUTLOOK_WRITE_TIME_ZONE
    }
    $end = @{
      date = ([DateTime]$item.End).ToString("yyyy-MM-dd")
      time_zone = $env:JK_OUTLOOK_WRITE_TIME_ZONE
    }
  } else {
    $start = @{
      date_time = ([DateTime]$item.Start).ToString("o")
      time_zone = $env:JK_OUTLOOK_WRITE_TIME_ZONE
    }
    $end = @{
      date_time = ([DateTime]$item.End).ToString("o")
      time_zone = $env:JK_OUTLOOK_WRITE_TIME_ZONE
    }
  }

  [pscustomobject]@{
    id = [string]$item.EntryID
    summary = [string]$item.Subject
    description = [string]$item.Body
    location = [string]$item.Location
    start = $start
    end = $end
    show_as = $showAs
  }
}

$action = $env:JK_OUTLOOK_WRITE_ACTION
$payload = $env:JK_OUTLOOK_WRITE_EVENT_JSON | ConvertFrom-Json
$outlook = New-Object -ComObject Outlook.Application
$namespace = $outlook.GetNamespace("MAPI")

if ($action -eq "get") {
  $item = $namespace.GetItemFromID($env:JK_OUTLOOK_WRITE_EVENT_ID)
  Event-FromItem $item | ConvertTo-Json -Depth 8 -Compress
  exit 0
}

if ($action -eq "insert") {
  $isTravelPayload = Test-TravelMarker ([string]$payload.description)
  if (((Test-HoldMarker ([string]$payload.description)) -or $isTravelPayload) -and [string]$payload.show_as -ne "free") {
    Assert-NoBusyConflict $namespace "" $payload.start.date_time $payload.end.date_time $isTravelPayload
  }
  $item = $outlook.CreateItem(1)
} elseif ($action -eq "patch") {
  $item = $namespace.GetItemFromID($env:JK_OUTLOOK_WRITE_EVENT_ID)
  $body = [string]$item.Body
  $hasManagedOwnership = (Test-HoldMarker $body) -or (Test-TravelMarker $body)
  if (-not $hasManagedOwnership) {
    if (-not $body.Contains("managed_by=jk-calendar-guard-agent") -or -not $body.Contains("owner_agent=ucla-tdg-outlook-calendar-write-agent")) {
      throw "refusing to patch event without calendar guard ownership marker"
    }
  }
  if ($env:JK_OUTLOOK_WRITE_EXPECT_START -and $env:JK_OUTLOOK_WRITE_EXPECT_END) {
    $liveStart = [DateTime]$item.Start
    $liveEnd = [DateTime]$item.End
    if (((Parse-EventTime $env:JK_OUTLOOK_WRITE_EXPECT_START) -ne $liveStart) -or ((Parse-EventTime $env:JK_OUTLOOK_WRITE_EXPECT_END) -ne $liveEnd)) {
      throw ("conflict: stale snapshot " + $liveStart.ToString("o") + "/" + $liveEnd.ToString("o"))
    }
  }
  $isTravelPayload = Test-TravelMarker ([string]$payload.description)
  if (((Test-HoldMarker ([string]$payload.description)) -or $isTravelPayload) -and [string]$payload.show_as -ne "free") {
    Assert-NoBusyConflict $namespace ([string]$item.EntryID) $payload.start.date_time $payload.end.date_time $isTravelPayload
  }
} else {
  throw "unsupported write action"
}

$item.Subject = [string]$payload.summary
$item.Body = [string]$payload.description
if ($payload.PSObject.Properties.Name -contains "location") {
  $item.Location = [string]$payload.location
}
# Tag fleet-created events with Joel's category taxonomy so they are
# color-coded like his own: travel blocks get the travel category, every
# managed (hold/travel) event gets the agent category. Guard blocks keep
# today's behavior exactly (no category writes).
if (Test-TravelMarker ([string]$payload.description)) {
  if ($env:JK_OUTLOOK_WRITE_TRAVEL_CATEGORIES) { $item.Categories = $env:JK_OUTLOOK_WRITE_TRAVEL_CATEGORIES }
} elseif (Test-HoldMarker ([string]$payload.description)) {
  if ($env:JK_OUTLOOK_WRITE_HOLD_CATEGORIES) { $item.Categories = $env:JK_OUTLOOK_WRITE_HOLD_CATEGORIES }
}
if ($payload.start.date -and $payload.end.date) {
  $item.Start = Parse-EventDate $payload.start.date
  $item.End = Parse-EventDate $payload.end.date
  $item.AllDayEvent = $true
} else {
  $item.Start = Parse-EventTime $payload.start.date_time
  $item.End = Parse-EventTime $payload.end.date_time
  $item.AllDayEvent = $false
}
$item.BusyStatus = ShowAs-ToBusyStatus $payload.show_as
$item.ReminderSet = $false
$item.Save()

Event-FromItem $item | ConvertTo-Json -Depth 8 -Compress
`

const outlookHostTimeZonePowerShell = `
$ErrorActionPreference = "Stop"
[System.TimeZoneInfo]::Local.Id
`

type PowerShellService struct {
	Command  string
	Timeout  time.Duration
	TimeZone string
	// Outlook category strings (comma-separated) applied to events the
	// agent creates/patches: TravelCategories for travel blocks,
	// HoldCategories for working holds. Empty disables tagging.
	TravelCategories string
	HoldCategories   string
}

func NewPowerShellService() *PowerShellService {
	return &PowerShellService{
		Command:  defaultPowerShellCommand(),
		Timeout:  20 * time.Second,
		TimeZone: DefaultTimeZone,
	}
}

func (s *PowerShellService) CheckHostTimeZone(ctx context.Context, expected string) error {
	hostTimeZone, err := s.HostTimeZone(ctx)
	if err != nil {
		return err
	}
	if !HostTimeZoneMatches(hostTimeZone, expected) {
		return fmt.Errorf("outlook host timezone %q does not match %s", hostTimeZone, expected)
	}
	return nil
}

func (s *PowerShellService) HostTimeZone(ctx context.Context) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := strings.TrimSpace(s.Command)
	if command == "" {
		command = defaultPowerShellCommand()
	}
	cmd := exec.CommandContext(runCtx, command, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", outlookHostTimeZonePowerShell)
	output, err := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		return "", fmt.Errorf("outlook calendar timezone probe timed out after %s", timeout)
	}
	if err != nil {
		return "", fmt.Errorf("outlook calendar timezone probe failed: %w: %s", err, sanitizePowerShellOutput(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func HostTimeZoneMatches(hostTimeZone, expected string) bool {
	hostTimeZone = strings.TrimSpace(hostTimeZone)
	expected = strings.TrimSpace(expected)
	if strings.EqualFold(hostTimeZone, expected) {
		return true
	}
	if expected == DefaultTimeZone {
		switch strings.ToLower(hostTimeZone) {
		case "pacific standard time", "us pacific standard time":
			return true
		}
	}
	return false
}

func (s *PowerShellService) GetEvent(ctx context.Context, calendarID, eventID string) (StoredEvent, error) {
	return s.run(ctx, "get", eventID, StoredEvent{})
}

func (s *PowerShellService) InsertEvent(ctx context.Context, calendarID string, event StoredEvent) (StoredEvent, error) {
	return s.run(ctx, "insert", "", event)
}

func (s *PowerShellService) PatchEvent(ctx context.Context, calendarID, eventID string, event StoredEvent) (StoredEvent, error) {
	return s.run(ctx, "patch", eventID, event)
}

// PatchEventExpecting implements SnapshotCheckedService: the PowerShell patch
// branch compares the expected start/end against the live Outlook item and
// throws a "conflict: stale snapshot ..." error on mismatch before any field
// is written.
func (s *PowerShellService) PatchEventExpecting(ctx context.Context, calendarID, eventID string, expectedStart, expectedEnd EventDateTime, event StoredEvent) (StoredEvent, error) {
	return s.runExpecting(ctx, "patch", eventID, event, expectedStart.DateTime, expectedEnd.DateTime)
}

func (s *PowerShellService) run(ctx context.Context, action, eventID string, event StoredEvent) (StoredEvent, error) {
	return s.runExpecting(ctx, action, eventID, event, "", "")
}

func (s *PowerShellService) runExpecting(ctx context.Context, action, eventID string, event StoredEvent, expectStart, expectEnd string) (StoredEvent, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	blob, _ := json.Marshal(event)
	command := strings.TrimSpace(s.Command)
	if command == "" {
		command = defaultPowerShellCommand()
	}
	cmd := exec.CommandContext(runCtx, command, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", outlookWritePowerShell)
	cmd.Env = append(os.Environ(),
		"JK_OUTLOOK_WRITE_ACTION="+action,
		"JK_OUTLOOK_WRITE_EVENT_ID="+eventID,
		"JK_OUTLOOK_WRITE_EVENT_JSON="+string(blob),
		"JK_OUTLOOK_WRITE_TIME_ZONE="+firstNonEmpty(s.TimeZone, DefaultTimeZone),
		"JK_OUTLOOK_WRITE_EXPECT_START="+strings.TrimSpace(expectStart),
		"JK_OUTLOOK_WRITE_EXPECT_END="+strings.TrimSpace(expectEnd),
		"JK_OUTLOOK_WRITE_TRAVEL_CATEGORIES="+strings.TrimSpace(s.TravelCategories),
		"JK_OUTLOOK_WRITE_HOLD_CATEGORIES="+strings.TrimSpace(s.HoldCategories),
	)
	output, err := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		return StoredEvent{}, fmt.Errorf("outlook calendar write operation timed out after %s", timeout)
	}
	if err != nil {
		return StoredEvent{}, fmt.Errorf("outlook calendar write operation failed: %w: %s", err, sanitizePowerShellOutput(output))
	}
	var out StoredEvent
	if err := json.Unmarshal(output, &out); err != nil {
		return StoredEvent{}, fmt.Errorf("decode outlook calendar write JSON: %w", err)
	}
	return out, nil
}

func sanitizePowerShellOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) > 1200 {
		text = text[:1200] + "..."
	}
	return text
}

func defaultPowerShellCommand() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
}
