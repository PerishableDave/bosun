package calendar

// Recurrence expansion for the common RRULE shapes (RFC 5545 §3.3.10):
//
//	FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL, COUNT, UNTIL, WKST,
//	BYDAY (weekly; plain weekdays as a filter for daily; with ordinals such
//	as 2TU or -1FR for monthly and for yearly with BYMONTH), BYMONTHDAY
//	(monthly, yearly with BYMONTH), BYMONTH, BYSETPOS (within a period),
//	plus RDATE, EXDATE and RECURRENCE-ID overrides.
//
// Anything else (sub-daily frequencies, BYHOUR/BYMINUTE/BYSECOND, BYWEEKNO,
// BYYEARDAY, yearly BYDAY/BYMONTHDAY without BYMONTH) is reported as
// unsupported: the event then contributes its first occurrence only and is
// flagged.
//
// Instances keep DTSTART's wall-clock time in DTSTART's zone, so a 09:00
// America/New_York daily meeting stays at 09:00 across DST changes.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxPeriods bounds how many FREQ periods are walked for one event.
const maxPeriods = 100_000

type weekdayNum struct {
	n  int // 0 = every such weekday in the period; ±1..±53 = ordinal
	wd time.Weekday
}

type rrule struct {
	freq       string
	interval   int
	count      int
	until      time.Time
	byDay      []weekdayNum
	byMonthDay []int
	byMonth    []int
	bySetPos   []int
	wkst       time.Weekday
}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// errUnsupported marks valid but unimplemented rules.
type errUnsupported struct{ why string }

func (e errUnsupported) Error() string { return "unsupported recurrence: " + e.why }

func parseIntList(v string, min, max int, allowNeg bool) ([]int, error) {
	var out []int
	for _, s := range splitList(v) {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 || n > max || (n < 0 && (!allowNeg || -n > max)) || (n > 0 && n < min) {
			return nil, fmt.Errorf("invalid value %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseRRule parses an RRULE value. dtstart supplies the zone for UNTIL
// values given as dates or floating times.
func parseRRule(v string, dtstart time.Time, allDay bool, tz *tzCtx) (*rrule, error) {
	r := &rrule{interval: 1, wkst: time.Monday}
	for _, part := range strings.Split(v, ";") {
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid RRULE part %q", part)
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		val = strings.ToUpper(strings.TrimSpace(val))
		var err error
		switch k {
		case "FREQ":
			r.freq = val
		case "INTERVAL":
			if r.interval, err = strconv.Atoi(val); err != nil || r.interval < 1 {
				return nil, fmt.Errorf("invalid INTERVAL %q", val)
			}
		case "COUNT":
			if r.count, err = strconv.Atoi(val); err != nil || r.count < 1 {
				return nil, fmt.Errorf("invalid COUNT %q", val)
			}
		case "UNTIL":
			u, isDate, err := tz.parseValue(val, "", false)
			if err != nil {
				return nil, fmt.Errorf("invalid UNTIL %q", val)
			}
			if isDate {
				// Inclusive of the whole day in DTSTART's zone.
				u = time.Date(u.Year(), u.Month(), u.Day(), 23, 59, 59, 0, dtstart.Location())
			} else if !strings.HasSuffix(val, "Z") {
				u = time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), u.Second(), 0, dtstart.Location())
			}
			r.until = u
		case "WKST":
			wd, ok := weekdays[val]
			if !ok {
				return nil, fmt.Errorf("invalid WKST %q", val)
			}
			r.wkst = wd
		case "BYDAY":
			for _, s := range splitList(val) {
				if len(s) < 2 {
					return nil, fmt.Errorf("invalid BYDAY %q", s)
				}
				wd, ok := weekdays[s[len(s)-2:]]
				if !ok {
					return nil, fmt.Errorf("invalid BYDAY %q", s)
				}
				n := 0
				if num := s[:len(s)-2]; num != "" {
					if n, err = strconv.Atoi(strings.TrimPrefix(num, "+")); err != nil || n == 0 || n > 53 || n < -53 {
						return nil, fmt.Errorf("invalid BYDAY %q", s)
					}
				}
				r.byDay = append(r.byDay, weekdayNum{n: n, wd: wd})
			}
		case "BYMONTHDAY":
			if r.byMonthDay, err = parseIntList(val, 1, 31, true); err != nil {
				return nil, fmt.Errorf("BYMONTHDAY: %w", err)
			}
		case "BYMONTH":
			if r.byMonth, err = parseIntList(val, 1, 12, false); err != nil {
				return nil, fmt.Errorf("BYMONTH: %w", err)
			}
		case "BYSETPOS":
			if r.bySetPos, err = parseIntList(val, 1, 366, true); err != nil {
				return nil, fmt.Errorf("BYSETPOS: %w", err)
			}
		case "BYHOUR", "BYMINUTE", "BYSECOND", "BYWEEKNO", "BYYEARDAY":
			return r, errUnsupported{k}
		default:
			if !strings.HasPrefix(k, "X-") {
				return nil, fmt.Errorf("unknown RRULE part %q", k)
			}
		}
	}
	switch r.freq {
	case "DAILY", "WEEKLY":
		for _, d := range r.byDay {
			if d.n != 0 {
				return r, errUnsupported{"BYDAY ordinal with FREQ=" + r.freq}
			}
		}
		if r.freq == "WEEKLY" && len(r.byMonthDay) > 0 {
			return r, errUnsupported{"BYMONTHDAY with FREQ=WEEKLY"}
		}
	case "MONTHLY":
	case "YEARLY":
		if len(r.byMonth) == 0 && (len(r.byDay) > 0 || len(r.byMonthDay) > 0) {
			return r, errUnsupported{"yearly BYDAY/BYMONTHDAY without BYMONTH"}
		}
	case "":
		return nil, fmt.Errorf("RRULE without FREQ")
	default:
		return r, errUnsupported{"FREQ=" + r.freq}
	}
	return r, nil
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// monthDays returns the candidate days (1-based) of month y/m.
func (r *rrule) monthDays(y int, m time.Month, defaultDay int) []int {
	n := daysIn(y, m)
	var set map[int]bool
	if len(r.byMonthDay) > 0 {
		set = map[int]bool{}
		for _, d := range r.byMonthDay {
			if d < 0 {
				d = n + 1 + d
			}
			if d >= 1 && d <= n {
				set[d] = true
			}
		}
	}
	if len(r.byDay) > 0 {
		bd := map[int]bool{}
		first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Weekday()
		for _, wn := range r.byDay {
			// days of month with weekday wn.wd
			var ds []int
			for d := 1 + (int(wn.wd)-int(first)+7)%7; d <= n; d += 7 {
				ds = append(ds, d)
			}
			switch {
			case wn.n == 0:
				for _, d := range ds {
					bd[d] = true
				}
			case wn.n > 0 && wn.n <= len(ds):
				bd[ds[wn.n-1]] = true
			case wn.n < 0 && -wn.n <= len(ds):
				bd[ds[len(ds)+wn.n]] = true
			}
		}
		if set == nil {
			set = bd
		} else {
			for d := range set {
				if !bd[d] {
					delete(set, d)
				}
			}
		}
	}
	if set == nil {
		if defaultDay > n {
			return nil // e.g. the 31st in a 30-day month: skipped (RFC 5545)
		}
		return []int{defaultDay}
	}
	out := make([]int, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// candidates returns the instances of period k, sorted.
func (r *rrule) candidates(dtstart time.Time, k int) []time.Time {
	loc := dtstart.Location()
	y, mo, d := dtstart.Date()
	h, mi, s := dtstart.Clock()
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, h, mi, s, 0, loc) }
	var out []time.Time
	switch r.freq {
	case "DAILY":
		t := at(y, mo, d+k*r.interval)
		if len(r.byMonth) > 0 && !containsInt(r.byMonth, int(t.Month())) {
			return nil
		}
		if len(r.byMonthDay) > 0 {
			n := daysIn(t.Year(), t.Month())
			ok := false
			for _, md := range r.byMonthDay {
				if md == t.Day() || (md < 0 && n+1+md == t.Day()) {
					ok = true
				}
			}
			if !ok {
				return nil
			}
		}
		if len(r.byDay) > 0 {
			ok := false
			for _, wn := range r.byDay {
				ok = ok || wn.wd == t.Weekday()
			}
			if !ok {
				return nil
			}
		}
		out = []time.Time{t}
	case "WEEKLY":
		offset := (int(dtstart.Weekday()) - int(r.wkst) + 7) % 7
		weekStart := d - offset + 7*k*r.interval
		days := r.byDay
		if len(days) == 0 {
			days = []weekdayNum{{wd: dtstart.Weekday()}}
		}
		for _, wn := range days {
			t := at(y, mo, weekStart+(int(wn.wd)-int(r.wkst)+7)%7)
			if len(r.byMonth) > 0 && !containsInt(r.byMonth, int(t.Month())) {
				continue
			}
			out = append(out, t)
		}
	case "MONTHLY":
		first := time.Date(y, mo+time.Month(k*r.interval), 1, 0, 0, 0, 0, time.UTC)
		if len(r.byMonth) > 0 && !containsInt(r.byMonth, int(first.Month())) {
			return nil
		}
		for _, day := range r.monthDays(first.Year(), first.Month(), d) {
			out = append(out, at(first.Year(), first.Month(), day))
		}
	case "YEARLY":
		yr := y + k*r.interval
		months := r.byMonth
		if len(months) == 0 {
			months = []int{int(mo)}
		}
		sorted := append([]int(nil), months...)
		sort.Ints(sorted)
		for _, m := range sorted {
			for _, day := range r.monthDays(yr, time.Month(m), d) {
				out = append(out, at(yr, time.Month(m), day))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	if len(r.bySetPos) > 0 && len(out) > 0 {
		var sel []time.Time
		for _, p := range r.bySetPos {
			i := p - 1
			if p < 0 {
				i = len(out) + p
			}
			if i >= 0 && i < len(out) {
				sel = append(sel, out[i])
			}
		}
		sort.Slice(sel, func(i, j int) bool { return sel[i].Before(sel[j]) })
		out = sel
	}
	return out
}

// periodStart returns a time no later than any instance of period k.
func (r *rrule) periodStart(dtstart time.Time, k int) time.Time {
	y, mo, d := dtstart.Date()
	loc := dtstart.Location()
	switch r.freq {
	case "DAILY":
		return time.Date(y, mo, d+k*r.interval, 0, 0, 0, 0, loc)
	case "WEEKLY":
		offset := (int(dtstart.Weekday()) - int(r.wkst) + 7) % 7
		return time.Date(y, mo, d-offset+7*k*r.interval, 0, 0, 0, 0, loc)
	case "MONTHLY":
		return time.Date(y, mo+time.Month(k*r.interval), 1, 0, 0, 0, 0, loc)
	default:
		return time.Date(y+k*r.interval, 1, 1, 0, 0, 0, 0, loc)
	}
}

// each calls fn with every instance start in order (DTSTART first) until fn
// returns false, COUNT or UNTIL is reached, or a period starts after stop.
func (r *rrule) each(dtstart, stop time.Time, fn func(time.Time) bool) {
	emitted := 0
	if !fn(dtstart) {
		return
	}
	emitted++
	if r.count > 0 && emitted >= r.count {
		return
	}
	for k := 0; k < maxPeriods; k++ {
		if r.periodStart(dtstart, k).After(stop) {
			return
		}
		for _, t := range r.candidates(dtstart, k) {
			if !t.After(dtstart) {
				continue // DTSTART was already emitted
			}
			if !r.until.IsZero() && t.After(r.until) {
				return
			}
			if !fn(t) {
				return
			}
			emitted++
			if r.count > 0 && emitted >= r.count {
				return
			}
		}
	}
}

// Instance is one occurrence of an event in a queried window.
type Instance struct {
	Event       *Event
	Start, End  time.Time
	Recurring   bool
	Unsupported string // non-empty if the RRULE could not be fully expanded
}

func sameInstant(a, b time.Time, allDay bool) bool {
	if allDay {
		ay, am, ad := a.Date()
		by, bm, bd := b.Date()
		return ay == by && am == bm && ad == bd
	}
	return a.Equal(b)
}

func overlaps(s, e, from, to time.Time) bool {
	if e.Equal(s) {
		return !s.Before(from) && s.Before(to)
	}
	return s.Before(to) && e.After(from)
}

// expand returns the instances of the events of one calendar object resource
// (a master plus RECURRENCE-ID overrides sharing its UID) overlapping
// [from, to), at most limit of them.
func expand(events []*Event, from, to time.Time, tz *tzCtx, limit int) []Instance {
	var out []Instance
	byUID := map[string][]*Event{}
	var order []string
	for _, e := range events {
		if _, ok := byUID[e.UID]; !ok {
			order = append(order, e.UID)
		}
		byUID[e.UID] = append(byUID[e.UID], e)
	}
	for _, uid := range order {
		var master *Event
		var overrides []*Event
		for _, e := range byUID[uid] {
			if e.RecurrenceID.IsZero() {
				if master == nil {
					master = e
				}
			} else {
				overrides = append(overrides, e)
			}
		}
		for _, o := range overrides {
			if o.Status != "CANCELLED" && overlaps(o.Start, o.End, from, to) {
				out = append(out, Instance{Event: o, Start: o.Start, End: o.End, Recurring: true})
			}
		}
		if master == nil {
			continue
		}
		out = append(out, expandMaster(master, overrides, from, to, tz, limit)...)
		if len(out) >= limit {
			return out[:limit]
		}
	}
	return out
}

func expandMaster(m *Event, overrides []*Event, from, to time.Time, tz *tzCtx, limit int) []Instance {
	var out []Instance
	days := 0
	dur := m.End.Sub(m.Start)
	if m.AllDay {
		days = int((m.End.Sub(m.Start) + 12*time.Hour) / (24 * time.Hour))
	}
	endOf := func(s time.Time) time.Time {
		if m.AllDay {
			return s.AddDate(0, 0, days)
		}
		return s.Add(dur)
	}
	skip := func(s time.Time) bool {
		for _, x := range m.ExDates {
			if sameInstant(s, x, m.AllDay) {
				return true
			}
		}
		for _, o := range overrides {
			if sameInstant(s, o.RecurrenceID, m.AllDay) {
				return true
			}
		}
		return false
	}
	recurring := m.Recurring()
	add := func(s time.Time) {
		if e := endOf(s); !skip(s) && overlaps(s, e, from, to) {
			out = append(out, Instance{Event: m, Start: s, End: e, Recurring: recurring})
		}
	}
	unsupported := ""
	if m.RRule == "" {
		add(m.Start)
	} else {
		r, err := parseRRule(m.RRule, m.Start, m.AllDay, tz)
		switch {
		case err != nil:
			if u, ok := err.(errUnsupported); ok {
				unsupported = u.why
			} else {
				unsupported = err.Error()
			}
			add(m.Start)
		default:
			r.each(m.Start, to, func(s time.Time) bool {
				if !s.Before(to) {
					return false
				}
				add(s)
				return len(out) < limit
			})
		}
	}
	for _, rd := range m.RDates {
		if !sameInstant(rd, m.Start, m.AllDay) {
			add(rd)
		}
	}
	if unsupported != "" {
		for i := range out {
			out[i].Unsupported = unsupported
		}
	}
	return out
}
