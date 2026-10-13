package routines

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression. Times are matched against the wall
// clock of a time.Location (the daemon's daemon.timezone).
//
// Supported syntax: the standard five fields
//
//	minute (0-59)  hour (0-23)  day-of-month (1-31)  month (1-12 or JAN-DEC)  day-of-week (0-7 or SUN-SAT; 0 and 7 are Sunday)
//
// each a comma-separated list of "*", "N", "N-M", with an optional "/step"
// ("*/15", "8-18/2", "5/10" meaning 5-max/10). The aliases @yearly
// (@annually), @monthly, @weekly, @daily (@midnight) and @hourly are
// accepted. As in Vixie cron, when both day-of-month and day-of-week are
// restricted (neither starts with "*"), a day matches if either matches.
type Schedule struct {
	expr                         string
	minute, hour, dom, month, dw uint64
	domStar, dowStar             bool
}

// String returns the expression the schedule was parsed from.
func (s *Schedule) String() string { return s.expr }

var cronAliases = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
	dowNames = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
)

type fieldSpec struct {
	name     string
	min, max int
	names    map[string]int
}

var fieldSpecs = [5]fieldSpec{
	{"minute", 0, 59, nil},
	{"hour", 0, 23, nil},
	{"day-of-month", 1, 31, nil},
	{"month", 1, 12, monthNames},
	{"day-of-week", 0, 7, dowNames},
}

// MaxCronLen bounds cron expressions.
const MaxCronLen = 200

// ParseCron parses a cron expression (see Schedule). Expressions that can
// never fire (such as "0 0 30 2 *") are rejected.
func ParseCron(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("cron: empty expression")
	}
	if len(expr) > MaxCronLen {
		return nil, fmt.Errorf("cron: expression longer than %d bytes", MaxCronLen)
	}
	spec := expr
	if strings.HasPrefix(expr, "@") {
		a, ok := cronAliases[strings.ToLower(expr)]
		if !ok {
			return nil, fmt.Errorf("cron: unknown alias %q (want @yearly, @monthly, @weekly, @daily or @hourly)", expr)
		}
		spec = a
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", expr, len(fields))
	}
	s := &Schedule{expr: expr}
	dst := [5]*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dw}
	for i, f := range fields {
		set, err := parseField(f, fieldSpecs[i])
		if err != nil {
			return nil, fmt.Errorf("cron: %w", err)
		}
		*dst[i] = set
	}
	s.domStar = strings.HasPrefix(fields[2], "*")
	s.dowStar = strings.HasPrefix(fields[4], "*")
	if s.dw&(1<<7) != 0 { // 7 is Sunday too
		s.dw = s.dw&^(1<<7) | 1
	}
	if _, ok := s.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); !ok {
		return nil, fmt.Errorf("cron: %q never fires", expr)
	}
	return s, nil
}

func parseField(f string, fs fieldSpec) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(f, ",") {
		if part == "" {
			return 0, fmt.Errorf("%s field %q: empty list item", fs.name, f)
		}
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 || n > fs.max {
				return 0, fmt.Errorf("%s field %q: invalid step %q", fs.name, f, stepStr)
			}
			step = n
		}
		var lo, hi int
		switch {
		case rng == "*":
			lo, hi = fs.min, fs.max
			if fs.name == "day-of-week" {
				hi = 6 // "*" means every day once; 7 is an alias of 0
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = fieldValue(a, fs); err != nil {
				return 0, fmt.Errorf("%s field %q: %w", fs.name, f, err)
			}
			if hi, err = fieldValue(b, fs); err != nil {
				return 0, fmt.Errorf("%s field %q: %w", fs.name, f, err)
			}
			if lo > hi {
				return 0, fmt.Errorf("%s field %q: range %d-%d is backwards", fs.name, f, lo, hi)
			}
		default:
			v, err := fieldValue(rng, fs)
			if err != nil {
				return 0, fmt.Errorf("%s field %q: %w", fs.name, f, err)
			}
			lo, hi = v, v
			if hasStep { // "N/step" means N through max
				hi = fs.max
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

func fieldValue(s string, fs fieldSpec) (int, error) {
	if fs.names != nil {
		if v, ok := fs.names[strings.ToUpper(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < fs.min || v > fs.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", v, fs.min, fs.max)
	}
	return v, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dw&(1<<uint(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// searchYears bounds how far ahead Next looks (Feb 29 schedules need up to
// 8 years; the day-of-week OR rule never needs more).
const searchYears = 30

// Next returns the first activation time strictly after `after`, evaluated
// on the wall clock of loc, or the zero time if there is none.
//
// Daylight-saving transitions are handled as follows:
//   - A wall time skipped by a spring-forward transition (e.g. 02:30 on the
//     night clocks jump from 02:00 to 03:00) fires once, at the first
//     instant after the gap (03:00), so a daily job still runs that day.
//   - A wall time repeated by a fall-back transition fires once, at its
//     first occurrence; the repeated hour does not fire again.
func (s *Schedule) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	t, _ := s.next(after, loc)
	return t
}

func (s *Schedule) next(after time.Time, loc *time.Location) (time.Time, bool) {
	// Walk candidate wall-clock times as "naive" UTC values, which have no
	// DST discontinuities, then map each match back to an instant in loc.
	w := after.In(loc)
	naive := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, time.UTC).Add(time.Minute)
	limit := naive.AddDate(searchYears, 0, 0)
	for naive.Before(limit) {
		if s.month&(1<<uint(naive.Month())) == 0 {
			naive = time.Date(naive.Year(), naive.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(naive) {
			naive = time.Date(naive.Year(), naive.Month(), naive.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(naive.Hour())) == 0 {
			naive = naive.Truncate(time.Hour).Add(time.Hour)
			continue
		}
		if s.minute&(1<<uint(naive.Minute())) == 0 {
			// Jump to the next allowed minute in this hour, if any.
			rest := s.minute >> uint(naive.Minute()+1)
			if rest == 0 {
				naive = naive.Truncate(time.Hour).Add(time.Hour)
			} else {
				naive = naive.Add(time.Duration(bits.TrailingZeros64(rest)+1) * time.Minute)
			}
			continue
		}
		if inst := wallToInstant(naive, loc); inst.After(after) {
			return inst, true
		}
		naive = naive.Add(time.Minute)
	}
	return time.Time{}, false
}

// wallToInstant returns the earliest instant whose wall clock in loc reads
// naive (a UTC value carrying the wall-clock fields). If that wall time does
// not exist (DST gap), it returns the first instant after the gap.
func wallToInstant(naive time.Time, loc *time.Location) time.Time {
	offsets := map[int]bool{}
	for _, d := range []time.Duration{-36 * time.Hour, 0, 36 * time.Hour} {
		_, off := naive.Add(d).In(loc).Zone()
		offsets[off] = true
	}
	var best time.Time
	minOff, maxOff := 1<<31-1, -1<<31
	for off := range offsets {
		minOff, maxOff = min(minOff, off), max(maxOff, off)
		inst := naive.Add(-time.Duration(off) * time.Second)
		if wallOf(inst, loc).Equal(naive) && (best.IsZero() || inst.Before(best)) {
			best = inst
		}
	}
	if !best.IsZero() {
		return best.In(loc)
	}
	// Gap: wall(lo) < naive < wall(hi). Binary-search the first instant whose
	// wall clock is at or past naive, i.e. the transition.
	lo := naive.Add(-time.Duration(maxOff) * time.Second)
	hi := naive.Add(-time.Duration(minOff) * time.Second)
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
		if !mid.After(lo) {
			break
		}
		if wallOf(mid, loc).Before(naive) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi.In(loc)
}

// wallOf returns t's wall clock in loc as a naive UTC value.
func wallOf(t time.Time, loc *time.Location) time.Time {
	w := t.In(loc)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
}
