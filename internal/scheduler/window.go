package scheduler

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const windowGrammar = "window must be today | tomorrow | this morning | this afternoon | this evening | tomorrow morning | tomorrow afternoon | tomorrow evening | next N days (1 <= N <= 7) | YYYY-MM-DD"

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func ResolveWindow(window, earliest, latest string, now time.Time) ([]Interval, error) {
	loc := loadLocation()
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(loc)

	windows, err := parseWindow(strings.TrimSpace(window), now, loc)
	if err != nil {
		return nil, err
	}

	earliestTime, hasEarliest, err := parseOptionalRFC3339(earliest, loc, "earliest")
	if err != nil {
		return nil, err
	}
	latestTime, hasLatest, err := parseOptionalRFC3339(latest, loc, "latest")
	if err != nil {
		return nil, err
	}
	if hasEarliest && hasLatest && !latestTime.After(earliestTime) {
		return nil, requestError{Code: ErrorInvalidWindow, Message: "empty earliest/latest intersection"}
	}

	if len(windows) == 0 {
		if !hasEarliest || !hasLatest {
			return nil, requestError{Code: ErrorInvalidWindow, Message: windowGrammar}
		}
		return []Interval{{Start: earliestTime, End: latestTime}}, nil
	}

	out := make([]Interval, 0, len(windows))
	for _, w := range windows {
		if hasEarliest && earliestTime.After(w.Start) {
			w.Start = earliestTime
		}
		if hasLatest && latestTime.Before(w.End) {
			w.End = latestTime
		}
		if w.Start.Before(w.End) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, requestError{Code: ErrorInvalidWindow, Message: "empty window intersection"}
	}
	sortIntervals(out)
	return out, nil
}

func parseWindow(window string, now time.Time, loc *time.Location) ([]Interval, error) {
	if strings.TrimSpace(window) == "" {
		return nil, nil
	}
	lower := strings.ToLower(strings.TrimSpace(window))
	today := localDateStart(now, loc)

	switch lower {
	case "today":
		return clipToday([]Interval{dayWindow(today, loc)}, now)
	case "tomorrow":
		return []Interval{dayWindow(today.AddDate(0, 0, 1), loc)}, nil
	case "this morning":
		return clipToday([]Interval{segmentWindow(today, "morning", loc)}, now)
	case "this afternoon":
		return clipToday([]Interval{segmentWindow(today, "afternoon", loc)}, now)
	case "this evening":
		return clipToday([]Interval{segmentWindow(today, "evening", loc)}, now)
	case "tomorrow morning":
		return []Interval{segmentWindow(today.AddDate(0, 0, 1), "morning", loc)}, nil
	case "tomorrow afternoon":
		return []Interval{segmentWindow(today.AddDate(0, 0, 1), "afternoon", loc)}, nil
	case "tomorrow evening":
		return []Interval{segmentWindow(today.AddDate(0, 0, 1), "evening", loc)}, nil
	default:
		if strings.HasPrefix(lower, "next ") && strings.HasSuffix(lower, " days") {
			return nextNDays(lower, today, now, loc)
		}
		if dateRE.MatchString(lower) {
			date, err := time.ParseInLocation("2006-01-02", lower, loc)
			if err != nil {
				return nil, requestError{Code: ErrorInvalidWindow, Message: windowGrammar}
			}
			return []Interval{dayWindow(date, loc)}, nil
		}
		return nil, requestError{Code: ErrorInvalidWindow, Message: windowGrammar}
	}
}

func nextNDays(window string, today, now time.Time, loc *time.Location) ([]Interval, error) {
	parts := strings.Fields(window)
	if len(parts) != 3 {
		return nil, requestError{Code: ErrorInvalidWindow, Message: windowGrammar}
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 || n > 7 {
		return nil, requestError{Code: ErrorInvalidWindow, Message: windowGrammar}
	}
	out := make([]Interval, 0, n)
	for i := 0; i < n; i++ {
		w := dayWindow(today.AddDate(0, 0, i), loc)
		if i == 0 && now.After(w.Start) {
			w.Start = now
		}
		if w.Start.Before(w.End) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, requestError{Code: ErrorInvalidWindow, Message: "window already passed"}
	}
	return out, nil
}

func clipToday(windows []Interval, now time.Time) ([]Interval, error) {
	out := make([]Interval, 0, len(windows))
	for _, w := range windows {
		if now.After(w.Start) {
			w.Start = now
		}
		if w.Start.Before(w.End) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, requestError{Code: ErrorInvalidWindow, Message: "window already passed"}
	}
	return out, nil
}

func dayWindow(date time.Time, loc *time.Location) Interval {
	return Interval{
		Start: time.Date(date.Year(), date.Month(), date.Day(), 7, 0, 0, 0, loc),
		End:   time.Date(date.Year(), date.Month(), date.Day(), 21, 0, 0, 0, loc),
	}
}

func segmentWindow(date time.Time, segment string, loc *time.Location) Interval {
	switch segment {
	case "morning":
		return clockWindow(date, 7, 0, 12, 0, loc)
	case "afternoon":
		return clockWindow(date, 12, 0, 17, 0, loc)
	case "evening":
		return clockWindow(date, 17, 0, 21, 0, loc)
	default:
		return dayWindow(date, loc)
	}
}

func clockWindow(date time.Time, startHour, startMinute, endHour, endMinute int, loc *time.Location) Interval {
	return Interval{
		Start: time.Date(date.Year(), date.Month(), date.Day(), startHour, startMinute, 0, 0, loc),
		End:   time.Date(date.Year(), date.Month(), date.Day(), endHour, endMinute, 0, 0, loc),
	}
}

func localDateStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func parseOptionalRFC3339(value string, loc *time.Location, field string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false, requestError{
			Code:    ErrorInvalidWindow,
			Message: fmt.Sprintf("%s must be RFC3339 with offset", field),
		}
	}
	return parsed.In(loc), true, nil
}

func formatLA(t time.Time) string {
	return t.In(loadLocation()).Format(time.RFC3339)
}

func loadLocation() *time.Location {
	loc, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return time.Local
	}
	return loc
}

func sortIntervals(intervals []Interval) {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].Start.Equal(intervals[j].Start) {
			return intervals[i].End.Before(intervals[j].End)
		}
		return intervals[i].Start.Before(intervals[j].Start)
	})
}
