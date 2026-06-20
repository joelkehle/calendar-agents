package outlookcalendar

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

const outlookPowerShell = `
$ErrorActionPreference = "Stop"
[System.Threading.Thread]::CurrentThread.CurrentCulture = [System.Globalization.CultureInfo]::GetCultureInfo("en-US")

$timeMin = [DateTime]::Parse($env:JK_OUTLOOK_TIME_MIN, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::RoundtripKind)
$timeMax = [DateTime]::Parse($env:JK_OUTLOOK_TIME_MAX, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::RoundtripKind)
$maxResults = [int]$env:JK_OUTLOOK_MAX_RESULTS
$includePrivate = $env:JK_OUTLOOK_INCLUDE_PRIVATE -eq "true"

$outlook = New-Object -ComObject Outlook.Application
$namespace = $outlook.GetNamespace("MAPI")
$calendar = $namespace.GetDefaultFolder(9)
$items = $calendar.Items
$items.IncludeRecurrences = $true
$items.Sort("[Start]")

$startText = $timeMin.ToString("MM/dd/yyyy hh:mm tt", [System.Globalization.CultureInfo]::InvariantCulture)
$endText = $timeMax.ToString("MM/dd/yyyy hh:mm tt", [System.Globalization.CultureInfo]::InvariantCulture)
$filter = "[Start] < '$endText' AND [End] > '$startText'"
$restricted = $items.Restrict($filter)

$rows = @()
$count = 0
foreach ($item in $restricted) {
  if ($count -ge $maxResults) { break }
  $sensitivity = 0
  $busyStatus = 0
  try { $sensitivity = [int]$item.Sensitivity } catch {}
  try { $busyStatus = [int]$item.BusyStatus } catch {}

  $subject = [string]$item.Subject
  $location = [string]$item.Location
  $bodyPreview = ""
  if (-not $includePrivate -and $sensitivity -eq 2) {
    $subject = "Private appointment"
    $location = ""
  }

  $rows += [pscustomobject]@{
    ID = [string]$item.GlobalAppointmentID
    EntryID = [string]$item.EntryID
    Start = ([DateTime]$item.Start).ToString("o")
    End = ([DateTime]$item.End).ToString("o")
    Subject = $subject
    Location = $location
    IsAllDay = [bool]$item.AllDayEvent
    BusyStatus = $busyStatus
    Sensitivity = $sensitivity
    Organizer = [string]$item.Organizer
    RequiredAttendees = [string]$item.RequiredAttendees
    OptionalAttendees = [string]$item.OptionalAttendees
    Categories = [string]$item.Categories
    BodyPreview = $bodyPreview
  }
  $count++
}

$rows | ConvertTo-Json -Depth 5 -Compress
`

type PowerShellExtractor struct {
	Command               string
	Timeout               time.Duration
	TimeZone              string
	IncludePrivateDetails bool
}

type outlookRow struct {
	ID                string `json:"ID"`
	EntryID           string `json:"EntryID"`
	Start             string `json:"Start"`
	End               string `json:"End"`
	Subject           string `json:"Subject"`
	Location          string `json:"Location"`
	IsAllDay          bool   `json:"IsAllDay"`
	BusyStatus        int    `json:"BusyStatus"`
	Sensitivity       int    `json:"Sensitivity"`
	Organizer         string `json:"Organizer"`
	RequiredAttendees string `json:"RequiredAttendees"`
	OptionalAttendees string `json:"OptionalAttendees"`
	Categories        string `json:"Categories"`
}

func NewPowerShellExtractor() *PowerShellExtractor {
	return &PowerShellExtractor{
		Command:               defaultPowerShellCommand(),
		Timeout:               20 * time.Second,
		TimeZone:              DefaultTimeZone,
		IncludePrivateDetails: true,
	}
}

func (e *PowerShellExtractor) ListEvents(query calendarread.EventsQuery) ([]calendarread.Event, error) {
	start, end, err := eventWindow(query)
	if err != nil {
		return nil, err
	}
	maxResults := query.MaxResults
	if maxResults <= 0 {
		maxResults = 50
	}
	if maxResults > 200 {
		maxResults = 200
	}

	ctx, cancel := context.WithTimeout(context.Background(), firstDuration(e.Timeout, 20*time.Second))
	defer cancel()

	command := strings.TrimSpace(e.Command)
	if command == "" {
		command = defaultPowerShellCommand()
	}
	cmd := exec.CommandContext(ctx, command, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", outlookPowerShell)
	cmd.Env = append(os.Environ(),
		"JK_OUTLOOK_TIME_MIN="+start.Format(time.RFC3339),
		"JK_OUTLOOK_TIME_MAX="+end.Format(time.RFC3339),
		"JK_OUTLOOK_MAX_RESULTS="+strconv.Itoa(maxResults),
		"JK_OUTLOOK_INCLUDE_PRIVATE="+strconv.FormatBool(e.IncludePrivateDetails),
	)

	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("outlook calendar extraction timed out after %s", firstDuration(e.Timeout, 20*time.Second))
	}
	if err != nil {
		return nil, fmt.Errorf("outlook calendar extraction failed: %w: %s", err, sanitizePowerShellOutput(output))
	}
	rows, err := decodeRows(output)
	if err != nil {
		return nil, err
	}
	events := rowsToEvents(rows, strings.TrimSpace(query.Query), firstNonEmpty(e.TimeZone, DefaultTimeZone))
	sort.Slice(events, func(i, j int) bool {
		return events[i].Start.DateTime < events[j].Start.DateTime
	})
	return events, nil
}

func eventWindow(query calendarread.EventsQuery) (time.Time, time.Time, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.Add(24 * time.Hour)
	var err error
	if strings.TrimSpace(query.TimeMin) != "" {
		start, err = parseTime(query.TimeMin)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid time_min: %w", err)
		}
	}
	if strings.TrimSpace(query.TimeMax) != "" {
		end, err = parseTime(query.TimeMax)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid time_max: %w", err)
		}
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("time_max must be after time_min")
	}
	if end.Sub(start) > 31*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("calendar query window must be 31 days or less")
	}
	return start, end, nil
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp")
}

func decodeRows(output []byte) ([]outlookRow, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil, nil
	}
	var rows []outlookRow
	if err := json.Unmarshal([]byte(text), &rows); err == nil {
		return rows, nil
	}
	var single outlookRow
	if err := json.Unmarshal([]byte(text), &single); err != nil {
		return nil, fmt.Errorf("decode outlook calendar JSON: %w", err)
	}
	return []outlookRow{single}, nil
}

func rowsToEvents(rows []outlookRow, textFilter, timeZone string) []calendarread.Event {
	filter := strings.ToLower(strings.TrimSpace(textFilter))
	events := make([]calendarread.Event, 0, len(rows))
	for _, row := range rows {
		subject := strings.TrimSpace(row.Subject)
		if filter != "" && !strings.Contains(strings.ToLower(subject+" "+row.Location+" "+row.Categories), filter) {
			continue
		}
		start, ok := parseOutlookLocalTime(row.Start, timeZone)
		if !ok {
			continue
		}
		end, ok := parseOutlookLocalTime(row.End, timeZone)
		if !ok {
			continue
		}
		event := calendarread.Event{
			ID:           outlookEventID(row),
			Status:       "confirmed",
			Summary:      firstNonEmpty(subject, "Calendar event"),
			Location:     strings.TrimSpace(row.Location),
			Categories:   splitCategories(row.Categories),
			Transparency: transparency(row.BusyStatus),
			Visibility:   visibility(row.Sensitivity),
			Start: calendarread.EventDateTime{
				DateTime: start.Format(time.RFC3339),
				TimeZone: timeZone,
			},
			End: calendarread.EventDateTime{
				DateTime: end.Format(time.RFC3339),
				TimeZone: timeZone,
			},
			Attendees: attendees(row),
			ExtendedProperties: &calendarread.ExtendedProperties{Private: map[string]string{
				"source":       "outlook",
				"busy_status":  strconv.Itoa(row.BusyStatus),
				"sensitivity":  strconv.Itoa(row.Sensitivity),
				"all_day":      strconv.FormatBool(row.IsAllDay),
				"categories":   strings.TrimSpace(row.Categories),
				"source_entry": shortHash(row.EntryID),
			}},
		}
		// Raw Outlook EntryID (additive, SCHEDULER_TRAVEL_SPEC §0): the write
		// agent's patch path resolves targets via GetItemFromID, which needs
		// the raw MAPI EntryID — the synthetic event id and the hashed
		// source_entry are useless for mutations. EntryID is an opaque
		// identifier, not content; Joel's 2026-06-11 ruling makes private
		// events fully fleet-visible, so it is exposed for masked private
		// events too (the mask hides subject/location, not identity).
		if entryID := strings.TrimSpace(row.EntryID); entryID != "" {
			event.ExtendedProperties.Private["entry_id"] = entryID
		}
		if row.IsAllDay {
			event.Start = calendarread.EventDateTime{Date: start.Format("2006-01-02"), TimeZone: timeZone}
			event.End = calendarread.EventDateTime{Date: end.Format("2006-01-02"), TimeZone: timeZone}
		}
		events = append(events, event)
	}
	return events
}

func parseOutlookLocalTime(value, timeZone string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	location, err := time.LoadLocation(firstNonEmpty(timeZone, DefaultTimeZone))
	if err != nil {
		location = time.Local
	}
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, value, location); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func outlookEventID(row outlookRow) string {
	source := firstNonEmpty(row.ID, row.EntryID, row.Start+"|"+row.End+"|"+row.Subject)
	return "outlook_" + shortHash(source)
}

func shortHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func transparency(busyStatus int) string {
	if busyStatus == 0 {
		return "transparent"
	}
	return "opaque"
}

func visibility(sensitivity int) string {
	if sensitivity == 2 {
		return "private"
	}
	return "default"
}

func attendees(row outlookRow) []calendarread.EventAttendee {
	var out []calendarread.EventAttendee
	if organizer := strings.TrimSpace(row.Organizer); organizer != "" {
		out = append(out, calendarread.EventAttendee{DisplayName: organizer, Organizer: true})
	}
	for _, value := range splitAttendees(row.RequiredAttendees) {
		out = append(out, calendarread.EventAttendee{DisplayName: value, ResponseStatus: "needsAction"})
	}
	for _, value := range splitAttendees(row.OptionalAttendees) {
		out = append(out, calendarread.EventAttendee{DisplayName: value, ResponseStatus: "needsAction", Optional: true})
	}
	return out
}

func splitAttendees(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == ','
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitCategories(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sanitizePowerShellOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) > 1200 {
		text = text[:1200] + "..."
	}
	return text
}

func firstDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func defaultPowerShellCommand() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
}
