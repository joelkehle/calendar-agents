package outlookcalendarwrite

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxDuration = 4 * time.Hour

const (
	minHoldDuration = 15 * time.Minute
	maxHoldDuration = 2 * time.Hour
)

const (
	minTravelDuration         = 10 * time.Minute
	maxTravelDuration         = 2 * time.Hour
	maxTravelDescriptionBytes = 4096
)

func BuildInsert(event EventInput) (StoredEvent, error) {
	if event.Summary == nil {
		return StoredEvent{}, errors.New("event.summary is required")
	}
	if event.Description == nil {
		return StoredEvent{}, errors.New("event.description is required")
	}
	if event.Start == nil || event.End == nil {
		return StoredEvent{}, errors.New("event.start and event.end are required")
	}
	if err := rejectProhibited(event); err != nil {
		return StoredEvent{}, err
	}
	// Forgery resistance: refuse hold_class= anywhere in a guard insert
	// description so unauthenticated guard inserts can never embed a forged
	// working-hold or travel-block marker (both marker matchers require the
	// hold_class= third line). managed_by=/owner_agent= stay permitted: the
	// live jk-calendar-guard-agent legitimately embeds them.
	if strings.Contains(strings.ToLower(*event.Description), "hold_class=") {
		return StoredEvent{}, errors.New("event.description must not contain reserved ownership marker keys")
	}
	stored := StoredEvent{
		Summary:     strings.TrimSpace(*event.Summary),
		Description: ensureOwnershipMarker(*event.Description),
		Start:       normalizeDateTime(*event.Start),
		End:         normalizeDateTime(*event.End),
		ShowAs:      normalizeShowAs(event.ShowAs),
	}
	if err := validateFinalEvent(stored); err != nil {
		return StoredEvent{}, err
	}
	return stored, nil
}

func BuildPatch(existing StoredEvent, patch EventInput) (StoredEvent, error) {
	if !HasOwnershipMarker(existing.Description) {
		return StoredEvent{}, errors.New("refusing to patch event without calendar guard ownership marker")
	}
	if err := rejectProhibited(patch); err != nil {
		return StoredEvent{}, err
	}
	merged := existing
	if patch.Summary != nil {
		merged.Summary = strings.TrimSpace(*patch.Summary)
	}
	if patch.Description != nil {
		merged.Description = ensureOwnershipMarker(*patch.Description)
	}
	if patch.Start != nil {
		merged.Start = normalizeDateTime(*patch.Start)
	}
	if patch.End != nil {
		merged.End = normalizeDateTime(*patch.End)
	}
	if patch.ShowAs != nil {
		merged.ShowAs = normalizeShowAs(patch.ShowAs)
	}
	if err := validateFinalEvent(merged); err != nil {
		return StoredEvent{}, err
	}
	return merged, nil
}

func IsHoldInsert(event EventInput) bool {
	if event.Summary == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(*event.Summary), HoldSummaryPrefix)
}

func IsHoldPatch(existing StoredEvent) bool {
	return HasHoldMarker(existing.Description)
}

func BuildHoldInsert(event EventInput, requester string) (StoredEvent, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return StoredEvent{}, errors.New("requester is required")
	}
	if event.Summary == nil {
		return StoredEvent{}, errors.New("event.summary is required")
	}
	if event.Description == nil {
		return StoredEvent{}, errors.New("event.description is required")
	}
	if event.Start == nil || event.End == nil {
		return StoredEvent{}, errors.New("event.start and event.end are required")
	}
	if err := rejectProhibited(event); err != nil {
		return StoredEvent{}, err
	}
	if containsReservedHoldMarkerKey(*event.Description) {
		return StoredEvent{}, errors.New("event.description must not contain reserved ownership marker keys")
	}
	stored := StoredEvent{
		Summary:     strings.TrimSpace(*event.Summary),
		Description: ensureHoldMarker(*event.Description, requester),
		Start:       normalizeDateTime(*event.Start),
		End:         normalizeDateTime(*event.End),
		ShowAs:      normalizeShowAs(event.ShowAs),
	}
	if err := validateHoldFinalEvent(stored, time.Now()); err != nil {
		return StoredEvent{}, err
	}
	return stored, nil
}

func BuildHoldPatch(existing StoredEvent, patch EventInput, requester string) (StoredEvent, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return StoredEvent{}, errors.New("requester is required")
	}
	managedBy, ok := holdMarkerRequester(existing.Description)
	if !ok {
		return StoredEvent{}, errors.New("refusing to patch event without working-hold ownership marker")
	}
	if managedBy != requester {
		return StoredEvent{}, errors.New("working holds can only be patched by their creating agent")
	}
	if err := rejectProhibited(patch); err != nil {
		return StoredEvent{}, err
	}
	if cancelled, ok, err := buildHoldCancelPatch(existing, patch); ok {
		return cancelled, err
	}
	if strings.HasPrefix(strings.TrimSpace(existing.Summary), CancelledPrefix) {
		return StoredEvent{}, errors.New("cancelled working holds cannot be patched")
	}
	merged := existing
	if patch.Summary != nil {
		merged.Summary = strings.TrimSpace(*patch.Summary)
	}
	if patch.Description != nil {
		if containsReservedHoldMarkerKey(*patch.Description) {
			return StoredEvent{}, errors.New("event.description must not contain reserved ownership marker keys")
		}
		merged.Description = ensureHoldMarker(*patch.Description, requester)
	}
	if patch.Start != nil {
		merged.Start = normalizeDateTime(*patch.Start)
	}
	if patch.End != nil {
		merged.End = normalizeDateTime(*patch.End)
	}
	if patch.ShowAs != nil {
		merged.ShowAs = normalizeShowAs(patch.ShowAs)
	}
	if err := validateHoldFinalEvent(merged, time.Now()); err != nil {
		return StoredEvent{}, err
	}
	return merged, nil
}

func IsTravelInsert(event EventInput) bool {
	if event.Summary == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(*event.Summary), TravelSummaryPrefix)
}

func IsTravelPatch(existing StoredEvent) bool {
	return HasTravelMarker(existing.Description)
}

func BuildTravelInsert(event EventInput, requester string) (StoredEvent, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return StoredEvent{}, errors.New("requester is required")
	}
	if event.Summary == nil {
		return StoredEvent{}, errors.New("event.summary is required")
	}
	if event.Description == nil {
		return StoredEvent{}, errors.New("event.description is required")
	}
	if event.Start == nil || event.End == nil {
		return StoredEvent{}, errors.New("event.start and event.end are required")
	}
	if err := rejectProhibited(event); err != nil {
		return StoredEvent{}, err
	}
	if len(*event.Description) > maxTravelDescriptionBytes {
		return StoredEvent{}, errors.New("event.description must not exceed 4096 characters")
	}
	if containsReservedHoldMarkerKey(*event.Description) {
		return StoredEvent{}, errors.New("event.description must not contain reserved ownership marker keys")
	}
	stored := StoredEvent{
		Summary:     strings.TrimSpace(*event.Summary),
		Description: ensureTravelMarker(*event.Description, requester),
		Location:    optionalTrimmedString(event.Location),
		Start:       normalizeDateTime(*event.Start),
		End:         normalizeDateTime(*event.End),
		ShowAs:      normalizeShowAs(event.ShowAs),
	}
	if err := validateTravelFinalEvent(stored, time.Now()); err != nil {
		return StoredEvent{}, err
	}
	return stored, nil
}

func BuildTravelPatch(existing StoredEvent, patch EventInput, requester string) (StoredEvent, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return StoredEvent{}, errors.New("requester is required")
	}
	managedBy, ok := travelMarkerRequester(existing.Description)
	if !ok {
		return StoredEvent{}, errors.New("refusing to patch event without travel-block ownership marker")
	}
	if managedBy != requester {
		return StoredEvent{}, errors.New("travel blocks can only be patched by their creating agent")
	}
	if err := rejectProhibited(patch); err != nil {
		return StoredEvent{}, err
	}
	if cancelled, ok, err := buildClassCancelPatch(existing, patch, "travel-block"); ok {
		return cancelled, err
	}
	if strings.HasPrefix(strings.TrimSpace(existing.Summary), CancelledPrefix) {
		return StoredEvent{}, errors.New("cancelled travel blocks cannot be patched")
	}
	merged := existing
	if patch.Summary != nil {
		merged.Summary = strings.TrimSpace(*patch.Summary)
	}
	if patch.Description != nil {
		if len(*patch.Description) > maxTravelDescriptionBytes {
			return StoredEvent{}, errors.New("event.description must not exceed 4096 characters")
		}
		if containsReservedHoldMarkerKey(*patch.Description) {
			return StoredEvent{}, errors.New("event.description must not contain reserved ownership marker keys")
		}
		merged.Description = ensureTravelMarker(*patch.Description, requester)
	}
	if patch.Location != nil {
		merged.Location = strings.TrimSpace(*patch.Location)
	}
	if patch.Start != nil {
		merged.Start = normalizeDateTime(*patch.Start)
	}
	if patch.End != nil {
		merged.End = normalizeDateTime(*patch.End)
	}
	if patch.ShowAs != nil {
		merged.ShowAs = normalizeShowAs(patch.ShowAs)
	}
	if err := validateTravelFinalEvent(merged, time.Now()); err != nil {
		return StoredEvent{}, err
	}
	return merged, nil
}

func validateTravelFinalEvent(event StoredEvent, now time.Time) error {
	if !strings.HasPrefix(strings.TrimSpace(event.Summary), TravelSummaryPrefix) {
		return fmt.Errorf("event.summary must start with %q", TravelSummaryPrefix)
	}
	if !HasTravelMarker(event.Description) {
		return errors.New("event.description must contain travel-block ownership marker")
	}
	if strings.TrimSpace(stripTravelMarker(event.Description)) == "" {
		return errors.New("event.description agenda is required")
	}
	start, end, err := validateClassTimes(event.Start, event.End, "travel blocks must be timed events")
	if err != nil {
		return err
	}
	if !sameLocalDate(start, end, DefaultTimeZone) {
		return errors.New("event.start and event.end must be on the same local date")
	}
	duration := end.Sub(start)
	if duration < minTravelDuration || duration > maxTravelDuration {
		return errors.New("travel-block duration must be at least 10 minutes and no more than 2 hours")
	}
	// Travel blocks may be backfilled up to 7 days into the past: Joel uses
	// them as a record of where time actually went, not only as future
	// protection (Joel's ruling 2026-06-11). Holds remain future-only.
	if start.Before(now.AddDate(0, 0, -7)) {
		return errors.New("travel-block start must be within the past 7 days or the next 30 days")
	}
	if start.After(now.AddDate(0, 0, 30)) {
		return errors.New("travel-block start must be within the next 30 days")
	}
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return err
	}
	localStart := start.In(location)
	startMinutes := localStart.Hour()*60 + localStart.Minute()
	if startMinutes < 5*60 || startMinutes > 23*60 {
		return errors.New("travel-block local start must be between 05:00 and 23:00")
	}
	if showAs := strings.ToLower(strings.TrimSpace(event.ShowAs)); showAs != "busy" {
		return errors.New("event.show_as must be busy")
	}
	return nil
}

func HasHoldMarker(description string) bool {
	_, ok := holdMarkerRequester(description)
	return ok
}

func HoldEventLocalDate(event EventInput) (string, bool) {
	if event.Start == nil || strings.TrimSpace(event.Start.DateTime) == "" {
		return "", false
	}
	start, err := parseDateTime(event.Start.DateTime)
	if err != nil {
		return "", false
	}
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return "", false
	}
	return start.In(location).Format("2006-01-02"), true
}

func HasOwnershipMarker(description string) bool {
	description = strings.TrimSpace(description)
	return strings.Contains(description, ManagedByMarker) && strings.Contains(description, OwnerAgentMarker)
}

func ensureOwnershipMarker(description string) string {
	description = strings.TrimSpace(description)
	marker := ManagedByMarker + "\n" + OwnerAgentMarker
	if description == "" {
		return marker
	}
	if HasOwnershipMarker(description) {
		return description
	}
	return description + "\n\n" + marker
}

func validateFinalEvent(event StoredEvent) error {
	if !allowedSummary(event.Summary) {
		return fmt.Errorf("event.summary must start with %q or equal %q", AllowedSummaryPrefix, AllowedDaySummary)
	}
	if !HasOwnershipMarker(event.Description) {
		return errors.New("event.description must contain calendar guard ownership marker")
	}
	start, end, allDay, err := validateTimes(event.Start, event.End)
	if err != nil {
		return err
	}
	if allDay {
		if !end.Equal(start.AddDate(0, 0, 1)) {
			return errors.New("all-day guard events must span exactly one local day")
		}
	} else {
		if !sameLocalDate(start, end, event.Start.TimeZone) {
			return errors.New("event.start and event.end must be on the same local date")
		}
		if duration := end.Sub(start); duration <= 0 || duration > maxDuration {
			return errors.New("event duration must be greater than 0 and no more than 4 hours")
		}
	}
	if showAs := strings.ToLower(strings.TrimSpace(event.ShowAs)); showAs != "" && showAs != "busy" {
		return errors.New("event.show_as must be busy")
	}
	return nil
}

func buildHoldCancelPatch(existing StoredEvent, patch EventInput) (StoredEvent, bool, error) {
	return buildClassCancelPatch(existing, patch, "working-hold")
}

func buildClassCancelPatch(existing StoredEvent, patch EventInput, label string) (StoredEvent, bool, error) {
	existingSummary := strings.TrimSpace(existing.Summary)
	summary := ""
	if patch.Summary != nil {
		summary = strings.TrimSpace(*patch.Summary)
	}
	showAs := ""
	if patch.ShowAs != nil {
		showAs = normalizeShowAs(patch.ShowAs)
	}
	cancelSignal := strings.HasPrefix(summary, CancelledPrefix) || showAs == "free"
	if !cancelSignal {
		return StoredEvent{}, false, nil
	}
	if patch.Summary == nil || patch.ShowAs == nil || patch.Description != nil || patch.Location != nil || patch.Start != nil || patch.End != nil {
		return StoredEvent{}, true, fmt.Errorf("%s cancel patch may only set summary and show_as", label)
	}
	if strings.HasPrefix(existingSummary, CancelledPrefix) {
		if !isCancelSummary(summary) {
			return StoredEvent{}, true, fmt.Errorf("%s cancel summary must gain the cancelled prefix", label)
		}
		if showAs != "free" {
			return StoredEvent{}, true, fmt.Errorf("%s cancel patch must set show_as to free", label)
		}
		return existing, true, nil
	}
	if !isCancelSummary(summary) || (summary != strings.TrimSpace(CancelledPrefix) && summary != CancelledPrefix+existingSummary) {
		return StoredEvent{}, true, fmt.Errorf("%s cancel summary must gain the cancelled prefix", label)
	}
	if showAs != "free" {
		return StoredEvent{}, true, fmt.Errorf("%s cancel patch must set show_as to free", label)
	}
	cancelled := existing
	cancelled.Summary = CancelledPrefix + existingSummary
	cancelled.ShowAs = "free"
	return cancelled, true, nil
}

func validateHoldFinalEvent(event StoredEvent, now time.Time) error {
	if !strings.HasPrefix(strings.TrimSpace(event.Summary), HoldSummaryPrefix) {
		return fmt.Errorf("event.summary must start with %q", HoldSummaryPrefix)
	}
	if !HasHoldMarker(event.Description) {
		return errors.New("event.description must contain working-hold ownership marker")
	}
	if strings.TrimSpace(stripHoldMarker(event.Description)) == "" {
		return errors.New("event.description agenda is required")
	}
	start, end, err := validateHoldTimes(event.Start, event.End)
	if err != nil {
		return err
	}
	if !sameLocalDate(start, end, DefaultTimeZone) {
		return errors.New("event.start and event.end must be on the same local date")
	}
	duration := end.Sub(start)
	if duration < minHoldDuration || duration > maxHoldDuration {
		return errors.New("working-hold duration must be at least 15 minutes and no more than 2 hours")
	}
	if !start.After(now) {
		return errors.New("working-hold start must be in the future")
	}
	if start.After(now.AddDate(0, 0, 30)) {
		return errors.New("working-hold start must be within the next 30 days")
	}
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return err
	}
	localStart := start.In(location)
	startMinutes := localStart.Hour()*60 + localStart.Minute()
	if startMinutes < 7*60 || startMinutes > 21*60 {
		return errors.New("working-hold local start must be between 07:00 and 21:00")
	}
	if showAs := strings.ToLower(strings.TrimSpace(event.ShowAs)); showAs != "busy" {
		return errors.New("event.show_as must be busy")
	}
	return nil
}

func optionalTrimmedString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func validateHoldTimes(startInput, endInput EventDateTime) (time.Time, time.Time, error) {
	return validateClassTimes(startInput, endInput, "working holds must be timed events")
}

func validateClassTimes(startInput, endInput EventDateTime, timedMessage string) (time.Time, time.Time, error) {
	if strings.TrimSpace(startInput.Date) != "" || strings.TrimSpace(endInput.Date) != "" {
		return time.Time{}, time.Time{}, errors.New(timedMessage)
	}
	if strings.TrimSpace(startInput.DateTime) == "" || strings.TrimSpace(endInput.DateTime) == "" {
		return time.Time{}, time.Time{}, errors.New("event.start.date_time and event.end.date_time are required")
	}
	start, err := validateHoldDateTime(startInput, "event.start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := validateHoldDateTime(endInput, "event.end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, end, nil
}

func validateHoldDateTime(input EventDateTime, label string) (time.Time, error) {
	timeZone := strings.TrimSpace(input.TimeZone)
	if timeZone != "" && timeZone != DefaultTimeZone {
		return time.Time{}, fmt.Errorf("%s.time_zone must be absent or %q", label, DefaultTimeZone)
	}
	value, err := parseDateTime(input.DateTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s.date_time: %w", label, err)
	}
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return time.Time{}, err
	}
	_, gotOffset := value.Zone()
	_, wantOffset := value.In(location).Zone()
	if gotOffset != wantOffset {
		return time.Time{}, fmt.Errorf("%s.date_time offset must match %s", label, DefaultTimeZone)
	}
	return value, nil
}

func allowedSummary(summary string) bool {
	summary = strings.TrimSpace(summary)
	return strings.HasPrefix(summary, AllowedSummaryPrefix) || summary == AllowedDaySummary
}

func validateTimes(startInput, endInput EventDateTime) (time.Time, time.Time, bool, error) {
	startDate := strings.TrimSpace(startInput.Date)
	endDate := strings.TrimSpace(endInput.Date)
	startDateTime := strings.TrimSpace(startInput.DateTime)
	endDateTime := strings.TrimSpace(endInput.DateTime)
	if startDate != "" || endDate != "" {
		if startDate == "" || endDate == "" || startDateTime != "" || endDateTime != "" {
			return time.Time{}, time.Time{}, false, errors.New("all-day calendar events require start.date and end.date only")
		}
		start, err := parseDate(startDate, startInput.TimeZone)
		if err != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("invalid event.start.date: %w", err)
		}
		end, err := parseDate(endDate, endInput.TimeZone)
		if err != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("invalid event.end.date: %w", err)
		}
		return start, end, true, nil
	}
	if startDateTime == "" || endDateTime == "" {
		return time.Time{}, time.Time{}, false, errors.New("event.start.date_time and event.end.date_time are required")
	}
	start, err := parseDateTime(startDateTime)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("invalid event.start.date_time: %w", err)
	}
	end, err := parseDateTime(endDateTime)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("invalid event.end.date_time: %w", err)
	}
	return start, end, false, nil
}

func parseDateTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("expected RFC3339 timestamp")
}

func parseDate(value, timeZone string) (time.Time, error) {
	location, err := time.LoadLocation(firstNonEmpty(timeZone, DefaultTimeZone))
	if err != nil {
		location = time.Local
	}
	value = strings.TrimSpace(value)
	out, err := time.ParseInLocation("2006-01-02", value, location)
	if err != nil {
		return time.Time{}, errors.New("expected YYYY-MM-DD date")
	}
	return out, nil
}

func sameLocalDate(start, end time.Time, timeZone string) bool {
	location, err := time.LoadLocation(firstNonEmpty(timeZone, DefaultTimeZone))
	if err == nil {
		start = start.In(location)
		end = end.In(location)
	}
	sy, sm, sd := start.Date()
	ey, em, ed := end.Date()
	return sy == ey && sm == em && sd == ed
}

func rejectProhibited(event EventInput) error {
	if fields := event.ProhibitedFields(); len(fields) > 0 {
		return fmt.Errorf("event field %q is not allowed", fields[0])
	}
	return nil
}

func normalizeDateTime(value EventDateTime) EventDateTime {
	return EventDateTime{
		Date:     strings.TrimSpace(value.Date),
		DateTime: strings.TrimSpace(value.DateTime),
		TimeZone: firstNonEmpty(value.TimeZone, DefaultTimeZone),
	}
}

func normalizeShowAs(value *string) string {
	if value == nil {
		return "busy"
	}
	return strings.ToLower(strings.TrimSpace(*value))
}
