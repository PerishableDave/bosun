package calendar

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxWindow bounds how far a single events/freebusy query may reach.
const maxWindow = 366 * 24 * time.Hour

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// addDays adds calendar days in loc (a DST day is 23 or 25 hours long).
func addDays(day time.Time, n int) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d+n, 0, 0, 0, 0, day.Location())
}

// localLayouts are accepted for wall-clock times in the user's zone.
var localLayouts = []string{
	"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04",
}

// parseWhen parses a time expression. Shorthands and plain dates denote a
// range [start, end); RFC 3339 and local date-times denote an instant
// (start == end, isRange false).
func parseWhen(s string, now time.Time, loc *time.Location) (start, end time.Time, isRange bool, err error) {
	s = strings.TrimSpace(s)
	today := startOfDay(now, loc)
	weekStart := addDays(today, -((int(today.Weekday()) + 6) % 7)) // Monday
	switch strings.ToLower(strings.Join(strings.Fields(s), " ")) {
	case "now":
		n := now.In(loc)
		return n, n, false, nil
	case "today":
		return today, addDays(today, 1), true, nil
	case "tomorrow":
		return addDays(today, 1), addDays(today, 2), true, nil
	case "yesterday":
		return addDays(today, -1), today, true, nil
	case "this week", "week":
		return weekStart, addDays(weekStart, 7), true, nil
	case "next week":
		return addDays(weekStart, 7), addDays(weekStart, 14), true, nil
	case "last week":
		return addDays(weekStart, -7), weekStart, true, nil
	case "this weekend", "weekend":
		sat := addDays(weekStart, 5)
		return sat, addDays(sat, 2), true, nil
	case "this month", "month":
		y, m, _ := today.Date()
		first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return first, time.Date(y, m+1, 1, 0, 0, 0, 0, loc), true, nil
	case "next month":
		y, m, _ := today.Date()
		return time.Date(y, m+1, 1, 0, 0, 0, 0, loc), time.Date(y, m+2, 1, 0, 0, 0, 0, loc), true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		t = t.In(loc)
		return t, t, false, nil
	}
	if d, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return d, addDays(d, 1), true, nil
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, t, false, nil
		}
	}
	return time.Time{}, time.Time{}, false, fmt.Errorf("cannot parse time %q: use RFC 3339 (2026-10-13T09:00:00-04:00), a local time (2026-10-13 09:00), a date (2026-10-13), or today/tomorrow/yesterday/this week/next week/this month", s)
}

// resolveRange turns from/to expressions into a window. Both empty means
// today; a range-valued from with no to means that whole range; an instant
// from with no to means the following 24 hours; a range-valued to means its
// end (so from=today to=tomorrow covers both days).
func resolveRange(fromS, toS string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	if fromS == "" && toS == "" {
		fromS = "today"
	}
	var from, to time.Time
	var fromEnd time.Time
	if fromS == "" {
		from = now.In(loc)
	} else {
		s, e, isRange, err := parseWhen(fromS, now, loc)
		if err != nil {
			return from, to, fmt.Errorf("from: %w", err)
		}
		from = s
		if isRange {
			fromEnd = e
		} else {
			fromEnd = s.Add(24 * time.Hour)
		}
	}
	if toS == "" {
		to = fromEnd
	} else {
		_, e, _, err := parseWhen(toS, now, loc)
		if err != nil {
			return from, to, fmt.Errorf("to: %w", err)
		}
		to = e
	}
	if !to.After(from) {
		return from, to, errors.New("the end of the window must be after its start")
	}
	if to.Sub(from) > maxWindow {
		return from, to, errors.New("window too long (at most 366 days)")
	}
	return from, to, nil
}

// parseInstant parses a start/end for create/update: RFC 3339 or a local
// date-time in loc. A plain date is midnight.
func parseInstant(s string, now time.Time, loc *time.Location) (time.Time, error) {
	t, _, _, err := parseWhen(s, now, loc)
	return t, err
}

// parseDate parses an all-day date (YYYY-MM-DD, today, tomorrow, or any
// time expression, taking its date in loc).
func parseDate(s string, now time.Time, loc *time.Location) (time.Time, error) {
	t, _, _, err := parseWhen(s, now, loc)
	if err != nil {
		return t, err
	}
	return startOfDay(t, loc), nil
}

// parseUserDuration accepts Go durations ("1h30m") and RFC 5545 ones
// ("PT1H30M", "P1D").
func parseUserDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, errors.New("duration must be positive")
		}
		return d, nil
	}
	days, d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 1h30m or PT1H30M)", s)
	}
	d += time.Duration(days) * 24 * time.Hour
	if d <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return d, nil
}
