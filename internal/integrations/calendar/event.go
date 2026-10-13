package calendar

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Person is an ORGANIZER or ATTENDEE (read-only).
type Person struct {
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"`
	PartStat string `json:"status,omitempty"`
}

// Event is a parsed VEVENT.
type Event struct {
	UID          string
	Summary      string
	Description  string
	Location     string
	Status       string // TENTATIVE | CONFIRMED | CANCELLED
	Transp       string // OPAQUE (default) | TRANSPARENT
	Start, End   time.Time
	AllDay       bool
	RRule        string
	RDates       []time.Time
	ExDates      []time.Time
	RecurrenceID time.Time // non-zero for an overridden instance
	Organizer    *Person
	Attendees    []Person
	Sequence     int

	comp *Component
}

// Recurring reports whether the event is a recurring master or an override
// of one instance.
func (e *Event) Recurring() bool {
	return e.RRule != "" || len(e.RDates) > 0 || !e.RecurrenceID.IsZero()
}

// tzCtx resolves TZIDs for one iCalendar object.
type tzCtx struct {
	floating *time.Location    // zone for floating times and all-day dates
	fallback *time.Location    // zone for unknown TZIDs (daemon zone)
	vtz      map[string]string // TZID -> IANA name hinted by VTIMEZONE
}

var (
	locCacheMu sync.Mutex
	locCache   = map[string]*time.Location{}
)

func loadLocation(name string) (*time.Location, bool) {
	if name == "" || name == "Local" || len(name) > 128 {
		return nil, false
	}
	if strings.EqualFold(name, "UTC") || strings.EqualFold(name, "GMT") || name == "Z" {
		return time.UTC, true
	}
	locCacheMu.Lock()
	defer locCacheMu.Unlock()
	if l, ok := locCache[name]; ok {
		return l, l != nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		l = nil
	}
	if len(locCache) < 512 {
		locCache[name] = l
	}
	return l, l != nil
}

// windowsZones maps common Windows / Outlook zone names to IANA names.
var windowsZones = map[string]string{
	"Eastern Standard Time":          "America/New_York",
	"Central Standard Time":          "America/Chicago",
	"Mountain Standard Time":         "America/Denver",
	"US Mountain Standard Time":      "America/Phoenix",
	"Pacific Standard Time":          "America/Los_Angeles",
	"Alaskan Standard Time":          "America/Anchorage",
	"Hawaiian Standard Time":         "Pacific/Honolulu",
	"Atlantic Standard Time":         "America/Halifax",
	"GMT Standard Time":              "Europe/London",
	"Greenwich Standard Time":        "Atlantic/Reykjavik",
	"W. Europe Standard Time":        "Europe/Berlin",
	"Romance Standard Time":          "Europe/Paris",
	"Central Europe Standard Time":   "Europe/Budapest",
	"Central European Standard Time": "Europe/Warsaw",
	"E. Europe Standard Time":        "Europe/Chisinau",
	"FLE Standard Time":              "Europe/Kiev",
	"GTB Standard Time":              "Europe/Bucharest",
	"Russian Standard Time":          "Europe/Moscow",
	"India Standard Time":            "Asia/Kolkata",
	"China Standard Time":            "Asia/Shanghai",
	"Tokyo Standard Time":            "Asia/Tokyo",
	"AUS Eastern Standard Time":      "Australia/Sydney",
	"E. South America Standard Time": "America/Sao_Paulo",
	"UTC":                            "UTC",
}

// newTZCtx collects VTIMEZONE hints (X-LIC-LOCATION) from a VCALENDAR.
func newTZCtx(cal *Component, floating, fallback *time.Location) *tzCtx {
	t := &tzCtx{floating: floating, fallback: fallback, vtz: map[string]string{}}
	if cal != nil {
		for _, vt := range cal.ChildrenNamed("VTIMEZONE") {
			id := vt.Text("TZID")
			if lic := vt.Text("X-LIC-LOCATION"); id != "" && lic != "" {
				t.vtz[id] = lic
			}
		}
	}
	return t
}

// location resolves a TZID: the IANA name itself, the VTIMEZONE's
// X-LIC-LOCATION, a Windows zone name, or a "/vendor/.../Area/City" path;
// otherwise the daemon zone.
func (t *tzCtx) location(tzid string) *time.Location {
	tzid = strings.Trim(strings.TrimSpace(tzid), `"`)
	if l, ok := loadLocation(tzid); ok {
		return l
	}
	if hint, ok := t.vtz[tzid]; ok {
		if l, ok := loadLocation(hint); ok {
			return l
		}
	}
	if w, ok := windowsZones[tzid]; ok {
		if l, ok := loadLocation(w); ok {
			return l
		}
	}
	if strings.Contains(tzid, "/") {
		parts := strings.Split(strings.Trim(tzid, "/"), "/")
		for i := 1; i < len(parts); i++ {
			if l, ok := loadLocation(strings.Join(parts[i:], "/")); ok {
				return l
			}
		}
	}
	return t.fallback
}

// parseValue parses one DATE or DATE-TIME value. tzid is the TZID
// parameter; isDate forces DATE (VALUE=DATE).
func (t *tzCtx) parseValue(v, tzid string, isDate bool) (time.Time, bool, error) {
	v = strings.TrimSpace(v)
	if isDate || len(v) == 8 {
		d, err := time.ParseInLocation("20060102", v, t.floating)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("invalid date %q", v)
		}
		return d, true, nil
	}
	if strings.HasSuffix(v, "Z") {
		d, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("invalid date-time %q", v)
		}
		return d, false, nil
	}
	loc := t.floating
	if tzid != "" {
		loc = t.location(tzid)
	}
	d, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("invalid date-time %q", v)
	}
	return d, false, nil
}

// parseProp parses a single-valued DATE/DATE-TIME property.
func (t *tzCtx) parseProp(p *Property) (time.Time, bool, error) {
	return t.parseValue(p.Value, p.Param("TZID"), strings.EqualFold(p.Param("VALUE"), "DATE"))
}

// parseList parses a multi-valued date property (EXDATE, RDATE). PERIOD
// values in RDATE use their start.
func (t *tzCtx) parseList(p *Property) ([]time.Time, error) {
	var out []time.Time
	for _, v := range splitList(p.Value) {
		if i := strings.IndexByte(v, '/'); i > 0 {
			v = v[:i]
		}
		d, _, err := t.parseValue(v, p.Param("TZID"), strings.EqualFold(p.Param("VALUE"), "DATE"))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// parseDuration parses an RFC 5545 duration ([+-]PnW or [+-]PnDTnHnMnS).
// Days are returned separately so all-day arithmetic stays calendar-based.
func parseDuration(s string) (days int, d time.Duration, err error) {
	orig := s
	s = strings.ToUpper(strings.TrimSpace(s))
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	if !strings.HasPrefix(s, "P") || len(s) < 3 {
		return 0, 0, fmt.Errorf("invalid duration %q", orig)
	}
	s = s[1:]
	inTime := false
	num := ""
	seen, timeSeen := false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'T':
			if inTime || num != "" {
				return 0, 0, fmt.Errorf("invalid duration %q", orig)
			}
			inTime = true
		default:
			if num == "" {
				return 0, 0, fmt.Errorf("invalid duration %q", orig)
			}
			n, err := strconv.Atoi(num)
			if err != nil || n > 1_000_000 {
				return 0, 0, fmt.Errorf("invalid duration %q", orig)
			}
			num = ""
			seen = true
			timeSeen = timeSeen || inTime
			switch {
			case r == 'W' && !inTime:
				days += 7 * n
			case r == 'D' && !inTime:
				days += n
			case r == 'H' && inTime:
				d += time.Duration(n) * time.Hour
			case r == 'M' && inTime:
				d += time.Duration(n) * time.Minute
			case r == 'S' && inTime:
				d += time.Duration(n) * time.Second
			default:
				return 0, 0, fmt.Errorf("invalid duration %q", orig)
			}
		}
	}
	if num != "" || !seen || (inTime && !timeSeen) {
		return 0, 0, fmt.Errorf("invalid duration %q", orig)
	}
	if neg {
		days, d = -days, -d
	}
	return days, d, nil
}

// formatDuration renders a positive duration as an RFC 5545 value.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	var sb strings.Builder
	sb.WriteString("P")
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	if days > 0 {
		fmt.Fprintf(&sb, "%dD", days)
	}
	if d > 0 {
		sb.WriteString("T")
		if h := d / time.Hour; h > 0 {
			fmt.Fprintf(&sb, "%dH", h)
			d -= h * time.Hour
		}
		if m := d / time.Minute; m > 0 {
			fmt.Fprintf(&sb, "%dM", m)
			d -= m * time.Minute
		}
		if s := d / time.Second; s > 0 {
			fmt.Fprintf(&sb, "%dS", s)
		}
	}
	return sb.String()
}

func parsePerson(p *Property) Person {
	email := p.Value
	if len(email) > 7 && strings.EqualFold(email[:7], "mailto:") {
		email = email[7:]
	}
	return Person{Email: email, Name: p.Param("CN"), Role: p.Param("ROLE"), PartStat: p.Param("PARTSTAT")}
}

// parseEvent extracts an Event from a VEVENT component.
func parseEvent(c *Component, tz *tzCtx) (*Event, error) {
	e := &Event{
		UID:         c.Text("UID"),
		Summary:     c.Text("SUMMARY"),
		Description: c.Text("DESCRIPTION"),
		Location:    c.Text("LOCATION"),
		Status:      strings.ToUpper(c.Text("STATUS")),
		Transp:      strings.ToUpper(c.Text("TRANSP")),
		comp:        c,
	}
	ds := c.Prop("DTSTART")
	if ds == nil {
		return nil, errors.New("VEVENT without DTSTART")
	}
	var err error
	if e.Start, e.AllDay, err = tz.parseProp(ds); err != nil {
		return nil, err
	}
	if de := c.Prop("DTEND"); de != nil {
		if e.End, _, err = tz.parseProp(de); err != nil {
			return nil, err
		}
	} else if du := c.Prop("DURATION"); du != nil {
		days, d, err := parseDuration(du.Value)
		if err != nil {
			return nil, err
		}
		e.End = e.Start.AddDate(0, 0, days).Add(d)
	} else if e.AllDay {
		e.End = e.Start.AddDate(0, 0, 1)
	} else {
		e.End = e.Start
	}
	if e.End.Before(e.Start) {
		e.End = e.Start
	}
	if r := c.Prop("RRULE"); r != nil {
		e.RRule = r.Value
	}
	for _, p := range c.PropsNamed("RDATE") {
		ts, err := tz.parseList(p)
		if err != nil {
			return nil, err
		}
		e.RDates = append(e.RDates, ts...)
	}
	for _, p := range c.PropsNamed("EXDATE") {
		ts, err := tz.parseList(p)
		if err != nil {
			return nil, err
		}
		e.ExDates = append(e.ExDates, ts...)
	}
	if p := c.Prop("RECURRENCE-ID"); p != nil {
		if e.RecurrenceID, _, err = tz.parseProp(p); err != nil {
			return nil, err
		}
	}
	if p := c.Prop("ORGANIZER"); p != nil {
		o := parsePerson(p)
		e.Organizer = &o
	}
	for _, p := range c.PropsNamed("ATTENDEE") {
		e.Attendees = append(e.Attendees, parsePerson(p))
	}
	if p := c.Prop("SEQUENCE"); p != nil {
		e.Sequence, _ = strconv.Atoi(strings.TrimSpace(p.Value))
	}
	return e, nil
}

// parseEvents parses every VEVENT in an iCalendar object.
func parseEvents(data string, floating, fallback *time.Location) (*Component, []*Event, error) {
	cal, err := ParseICS(data)
	if err != nil {
		return nil, nil, err
	}
	tz := newTZCtx(cal, floating, fallback)
	var out []*Event
	for _, c := range cal.ChildrenNamed("VEVENT") {
		e, err := parseEvent(c, tz)
		if err != nil {
			return nil, nil, fmt.Errorf("event %q: %w", c.Text("UID"), err)
		}
		out = append(out, e)
	}
	return cal, out, nil
}

// --- serialization helpers ---

func utcStamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// dateTimeProp returns a DTSTART/DTEND-style property: VALUE=DATE for
// all-day, otherwise UTC.
func dateTimeProp(name string, t time.Time, allDay bool) *Property {
	if allDay {
		return &Property{Name: name, Params: []Param{{Name: "VALUE", Values: []string{"DATE"}}}, Value: t.Format("20060102")}
	}
	return &Property{Name: name, Value: utcStamp(t)}
}
