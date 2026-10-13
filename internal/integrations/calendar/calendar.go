// Package calendar is the first-party calendar integration: each user's own
// calendars over CalDAV (RFC 4791) with iCalendar (RFC 5545) data. It works
// with Google Calendar (OAuth bearer token), iCloud (app-specific password),
// Fastmail, Nextcloud, Radicale, Baïkal and other CalDAV servers.
//
// Accounts are per user and live entirely in the vault:
//
//   - "oauth2:google": Secret is a valid Google access token and
//     Fields["email"] the account (written by `bosun connect google --scopes
//     caldav`, which grants the full calendar scope CalDAV needs).
//   - "password": Secret is the (app-specific) password, Fields["url"] the
//     server, principal, calendar-home or calendar URL, Fields["user"] the
//     login, and optionally Fields["calendar"] the default calendar name.
//
// Credentials are only sent over https (plain http only to loopback), and
// only to the configured host's site, even if the server redirects or points
// its principal elsewhere.
//
// Trust: event text comes from whoever sent the invitation, so every result
// is Untrusted.
package calendar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Credential kinds.
const (
	KindGoogle   = "oauth2:google"
	KindPassword = "password"
)

// GoogleCalendarScope is the OAuth scope Google's CalDAV endpoint requires
// (calendar.events and calendar.readonly are not enough).
const GoogleCalendarScope = "https://www.googleapis.com/auth/calendar"

// hasCalendarScope reports whether a granted-scopes field (space or comma
// separated) includes the full calendar scope. An absent field is accepted:
// the server's answer is then authoritative.
func hasCalendarScope(scopes string) bool {
	if strings.TrimSpace(scopes) == "" {
		return true
	}
	for _, s := range strings.FieldsFunc(scopes, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' }) {
		if s == GoogleCalendarScope {
			return true
		}
	}
	return false
}

// GoogleCalDAVURL is Google's CalDAV endpoint.
const GoogleCalDAVURL = "https://apidata.googleusercontent.com/caldav/v2/"

const (
	capRead   = integration.Capability("calendar.read")
	capWrite  = integration.Capability("calendar.write")
	capDelete = integration.Capability("calendar.delete")

	maxEvents     = 200
	cacheTTL      = 5 * time.Minute
	httpTimeout   = 30 * time.Second
	maxTextField  = 20_000
	maxDescOutput = 4000
)

// Settings is the "calendar" section of bosun.yaml.
type Settings struct {
	// Backend must be "caldav" (the default) if set.
	Backend string `json:"backend,omitempty"`
}

// Integration implements integration.Integration.
type Integration struct {
	creds     integration.CredentialSource
	loc       *time.Location // daemon zone
	hc        *http.Client
	googleURL string
	now       func() time.Time

	mu    sync.Mutex
	cache map[int64]cacheEntry
}

type cacheEntry struct {
	key  string
	cals []Calendar
	at   time.Time
}

var _ integration.Integration = (*Integration)(nil)

// New returns an uninitialised calendar integration.
func New() *Integration {
	return &Integration{
		loc:       time.Local,
		googleURL: GoogleCalDAVURL,
		now:       time.Now,
		cache:     map[int64]cacheEntry{},
		hc: &http.Client{
			Timeout:       httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// SetLocation sets the daemon time zone (daemon.timezone), used when a
// request names none and for unknown TZIDs. Call before Init.
func (c *Integration) SetLocation(loc *time.Location) {
	if loc != nil {
		c.loc = loc
	}
}

// Manifest implements integration.Integration.
func (c *Integration) Manifest() integration.Manifest {
	return integration.Manifest{
		Name:            "calendar",
		Version:         "0.1.0",
		Scope:           integration.ScopePerUser,
		Capabilities:    []integration.Capability{capRead, capWrite, capDelete},
		CredentialKinds: []string{KindGoogle, KindPassword},
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
			`"backend":{"type":"string","enum":["caldav"],"description":"calendar protocol (only caldav)"}}}`),
	}
}

// Init implements integration.Integration.
func (c *Integration) Init(_ context.Context, cfg integration.Config, creds integration.CredentialSource) error {
	var s Settings
	if len(cfg.Settings) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("calendar: decode config: %w", err)
		}
	}
	if s.Backend != "" && s.Backend != "caldav" {
		return fmt.Errorf("calendar: unsupported backend %q (only caldav)", s.Backend)
	}
	if creds == nil {
		return errors.New("calendar: no credential source")
	}
	c.creds = creds
	return nil
}

// Close implements integration.Integration.
func (c *Integration) Close() error { return nil }

// Tools implements integration.Integration.
func (c *Integration) Tools() []integration.Tool {
	const tzProp = `"timezone":{"type":"string","description":"IANA time zone for interpreting and showing times, e.g. America/New_York (default: the household zone)"}`
	const calProp = `"calendar":{"type":"string","description":"calendar name or href (default: all calendars for reads, the default calendar for creates)"}`
	const uidProp = `"uid":{"type":"string","minLength":1,"description":"event UID from calendar.events"}`
	const etagProp = `"etag":{"type":"string","description":"ETag from calendar.get; if given, the call fails when the event changed since"}`
	timeDesc := "RFC 3339, a local date-time like 2026-10-13 15:00 (in timezone), a date 2026-10-13, or today/tomorrow/yesterday/this week/next week/this month"
	editProps := `"summary":{"type":"string","description":"title"},` +
		`"start":{"type":"string","description":"start: ` + timeDesc + `"},` +
		`"end":{"type":"string","description":"end (exclusive) for timed events; the last day (inclusive) for all-day events"},` +
		`"duration":{"type":"string","description":"length instead of end, e.g. 1h30m or PT45M"},` +
		`"all_day":{"type":"boolean","description":"all-day event (start/end are dates)"},` +
		`"location":{"type":"string"},` +
		`"description":{"type":"string"},` + tzProp
	return []integration.Tool{
		{
			Name:        "calendar.calendars",
			Description: "List the user's calendars (name, href, color, read-only flag).",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
			Handler:     c.calendars,
		},
		{
			Name: "calendar.events",
			Description: "List events in a time window (recurrences expanded), sorted by start, in local time. " +
				"from/to accept " + timeDesc + ". Default window: today. Example: {\"from\":\"today\"} or {\"from\":\"this week\"}.",
			Capability: capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
				`"from":{"type":"string","description":"window start (a range shorthand like today covers the whole range when to is omitted)"},` +
				`"to":{"type":"string","description":"window end (a shorthand means the end of that range)"},` +
				calProp + `,` + tzProp + `}}`),
			Handler: c.events,
		},
		{
			Name:        "calendar.get",
			Description: "Get one event in full: description, attendees, organizer, recurrence rule, ETag.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["uid"],"properties":{` +
				uidProp + `,` + calProp + `,` + tzProp + `}}`),
			Handler: c.get,
		},
		{
			Name:        "calendar.create",
			Description: "Create an event. Give end or duration (default 1 hour; all-day events default to one day).",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["summary","start"],"properties":{` +
				calProp + `,` + editProps + `}}`),
			Handler: c.create,
		},
		{
			Name: "calendar.update",
			Description: "Change fields of an existing event (only the fields given). For a recurring event this changes the whole series. " +
				"Fails with a conflict if the event changed on the server meanwhile.",
			Capability: capWrite, Risk: integration.RiskWrite,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["uid"],"properties":{` +
				uidProp + `,` + calProp + `,` + etagProp + `,` + editProps + `}}`),
			Handler: c.update,
		},
		{
			Name:        "calendar.delete",
			Description: "Delete an event permanently (for a recurring event, the whole series).",
			Capability:  capDelete, Risk: integration.RiskDestructive,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["uid"],"properties":{` +
				uidProp + `,` + calProp + `,` + etagProp + `}}`),
			Handler: c.delete,
		},
		{
			Name:        "calendar.freebusy",
			Description: "Busy intervals (merged) across the user's writable calendars in a window; free/transparent and cancelled events are ignored. from/to as in calendar.events.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
				`"from":{"type":"string"},"to":{"type":"string"},` + tzProp + `}}`),
			Handler: c.freebusy,
		},
	}
}

// --- accounts ---

func noAccountError() error {
	return errors.New("calendar: no calendar account is set up for you. Either connect Google with `bosun connect google --scopes caldav`, " +
		"or add a CalDAV account with `bosun credentials set calendar password --field url=https://caldav.icloud.com --field user=you@icloud.com` " +
		"(the secret is an app-specific password for iCloud; use your server's CalDAV URL for Fastmail, Nextcloud, Radicale or Baïkal)")
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// account resolves the calling user's account: oauth2:google, then password.
func (c *Integration) account(ctx context.Context) (*account, error) {
	if c.creds == nil {
		return nil, errors.New("calendar: not initialised")
	}
	if uid, ok := integration.UserIDFromContext(ctx); !ok || uid <= 0 {
		return nil, errors.New("calendar: no user in context")
	}
	cred, err := c.creds.Get(ctx, KindGoogle)
	if err == nil {
		email := strings.TrimSpace(cred.Fields["email"])
		if email == "" || strings.ContainsAny(email, "/?#") {
			return nil, errors.New("calendar: the Google credential has no valid email field; run `bosun connect google --scopes caldav` again")
		}
		if !hasCalendarScope(cred.Fields["scopes"]) {
			return nil, errors.New("calendar: the linked Google account was not granted calendar access (CalDAV needs the full " +
				GoogleCalendarScope + " scope); run `bosun connect google --scopes caldav` (combinable, e.g. --scopes imap,caldav)")
		}
		if cred.Secret == "" {
			return nil, errors.New("calendar: the Google credential has no access token; run `bosun connect google --scopes caldav` again")
		}
		start, err := url.Parse(c.googleURL)
		if err != nil {
			return nil, fmt.Errorf("calendar: google url: %w", err)
		}
		if err := checkScheme(start); err != nil {
			return nil, err
		}
		esc := url.PathEscape(email)
		basePath := strings.TrimSuffix(start.Path, "/")
		return &account{
			kind: KindGoogle, start: start, label: email,
			principal: basePath + "/" + esc + "/user",
			fallback:  basePath + "/" + esc + "/events/",
			auth:      "Bearer " + cred.Secret,
			defCal:    cred.Fields["calendar"],
			key:       fingerprint(KindGoogle, email, start.String()),
		}, nil
	}
	if !errors.Is(err, integration.ErrNoCredential) {
		return nil, fmt.Errorf("calendar: read Google credential: %w", err)
	}
	cred, err = c.creds.Get(ctx, KindPassword)
	if errors.Is(err, integration.ErrNoCredential) {
		return nil, noAccountError()
	}
	if err != nil {
		return nil, fmt.Errorf("calendar: read password credential: %w", err)
	}
	raw := strings.TrimSpace(cred.Fields["url"])
	user := strings.TrimSpace(cred.Fields["user"])
	if user == "" {
		user = strings.TrimSpace(cred.Fields["username"])
	}
	if raw == "" || user == "" {
		return nil, errors.New("calendar: the password credential needs --field url=<CalDAV URL> and --field user=<login>; " +
			"e.g. `bosun credentials set calendar password --field url=https://caldav.icloud.com --field user=you@icloud.com`")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	start, err := url.Parse(raw)
	if err != nil || start.Host == "" {
		return nil, fmt.Errorf("calendar: invalid url field %q", raw)
	}
	if start.User != nil {
		return nil, errors.New("calendar: put the login in the user field, not in the URL")
	}
	if err := checkScheme(start); err != nil {
		return nil, err
	}
	return &account{
		kind: KindPassword, start: start, label: user,
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+cred.Secret)),
		defCal: cred.Fields["calendar"],
		key:    fingerprint(KindPassword, start.String(), user, fingerprint(cred.Secret)),
	}, nil
}

// session is a resolved account plus its calendars for one tool call.
type session struct {
	cl   *client
	cals []Calendar
	loc  *time.Location
}

func (c *Integration) open(ctx context.Context, tz string, refresh bool) (*session, error) {
	loc := c.loc
	if tz = strings.TrimSpace(tz); tz != "" {
		l, ok := loadLocation(tz)
		if !ok {
			return nil, fmt.Errorf("calendar: unknown time zone %q (use an IANA name like Europe/Madrid)", tz)
		}
		loc = l
	}
	acct, err := c.account(ctx)
	if err != nil {
		return nil, err
	}
	uid, _ := integration.UserIDFromContext(ctx)
	cl := &client{hc: c.hc, acct: acct}
	c.mu.Lock()
	ent, ok := c.cache[uid]
	c.mu.Unlock()
	if ok && !refresh && ent.key == acct.key && c.now().Sub(ent.at) < cacheTTL {
		return &session{cl: cl, cals: ent.cals, loc: loc}, nil
	}
	cals, err := cl.discover(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cals, func(i, j int) bool { return strings.ToLower(cals[i].Name) < strings.ToLower(cals[j].Name) })
	c.mu.Lock()
	c.cache[uid] = cacheEntry{key: acct.key, cals: cals, at: c.now()}
	c.mu.Unlock()
	return &session{cl: cl, cals: cals, loc: loc}, nil
}

// find returns the calendar named (case-insensitively) or with the href.
func (s *session) find(name string) (*Calendar, error) {
	name = strings.TrimSpace(name)
	for i := range s.cals {
		if strings.EqualFold(s.cals[i].Name, name) || samePath(s.cals[i].Href, name) || s.cals[i].url.String() == name {
			return &s.cals[i], nil
		}
	}
	names := make([]string, 0, len(s.cals))
	for _, cal := range s.cals {
		names = append(names, cal.Name)
	}
	return nil, fmt.Errorf("calendar: no calendar named %q (available: %s)", name, strings.Join(names, ", "))
}

// pick returns the named calendars, or all of them.
func (s *session) pick(name string) ([]*Calendar, error) {
	if strings.TrimSpace(name) != "" {
		cal, err := s.find(name)
		if err != nil {
			return nil, err
		}
		return []*Calendar{cal}, nil
	}
	out := make([]*Calendar, 0, len(s.cals))
	for i := range s.cals {
		out = append(out, &s.cals[i])
	}
	if len(out) == 0 {
		return nil, errors.New("calendar: the account has no calendars")
	}
	return out, nil
}

// defaultCalendar is the named one, else the credential's default, else
// the first writable calendar.
func (s *session) defaultCalendar(name string) (*Calendar, error) {
	if name == "" {
		name = s.cl.acct.defCal
	}
	if name != "" {
		cal, err := s.find(name)
		if err != nil {
			return nil, err
		}
		if cal.ReadOnly {
			return nil, fmt.Errorf("calendar: %q is read-only", cal.Name)
		}
		return cal, nil
	}
	for i := range s.cals {
		if !s.cals[i].ReadOnly {
			return &s.cals[i], nil
		}
	}
	return nil, errors.New("calendar: the account has no writable calendar")
}

func decodeArgs(in json.RawMessage, v any) error {
	if len(in) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func result(content string, data any) integration.Result {
	return integration.Result{Content: content, Untrusted: true, Data: data}
}

// --- calendars ---

func (c *Integration) calendars(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct{}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	s, err := c.open(ctx, "", true)
	if err != nil {
		return integration.Result{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d calendar(s):\n", len(s.cals))
	for _, cal := range s.cals {
		fmt.Fprintf(&sb, "- %s", oneLine(cal.Name, 100))
		if cal.ReadOnly {
			sb.WriteString(" (read-only)")
		}
		if cal.Color != "" {
			fmt.Fprintf(&sb, " color=%s", oneLine(cal.Color, 20))
		}
		fmt.Fprintf(&sb, " href=%s\n", cal.Href)
	}
	return result(sb.String(), s.cals), nil
}

// --- events ---

// EventSummary is one instance in calendar.events output.
type EventSummary struct {
	UID         string `json:"uid"`
	Calendar    string `json:"calendar"`
	Summary     string `json:"summary"`
	Location    string `json:"location,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"` // exclusive; for all-day events the last day (inclusive)
	AllDay      bool   `json:"all_day"`
	Recurring   bool   `json:"recurring"`
	Status      string `json:"status,omitempty"`
	Unsupported string `json:"recurrence_unsupported,omitempty"`

	start, end time.Time
}

type located struct {
	inst Instance
	cal  *Calendar
	res  resource
}

// window fetches and expands instances in [from, to) across cals.
func (c *Integration) window(ctx context.Context, s *session, cals []*Calendar, from, to time.Time) ([]located, int, error) {
	var out []located
	skipped := 0
	for _, cal := range cals {
		rs, err := s.cl.query(ctx, cal, from, to, "")
		if err != nil {
			return nil, 0, fmt.Errorf("calendar %q: %w", cal.Name, err)
		}
		for _, r := range rs {
			cal0, evs, err := parseEvents(r.Data, s.loc, c.loc)
			if err != nil {
				skipped++
				continue
			}
			tz := newTZCtx(cal0, s.loc, c.loc)
			for _, inst := range expand(evs, from, to, tz, maxEvents+1) {
				out = append(out, located{inst: inst, cal: cal, res: r})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].inst, out[j].inst
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Event.AllDay != b.Event.AllDay {
			return a.Event.AllDay
		}
		return a.Event.Summary < b.Event.Summary
	})
	return out, skipped, nil
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, max)
}

// when formats an instance compactly in loc.
func when(start, end time.Time, allDay bool, loc *time.Location) string {
	if allDay {
		last := addDays(time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC), -1)
		if !last.After(time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)) {
			return start.Format("Mon 2006-01-02") + " all day"
		}
		return start.Format("Mon 2006-01-02") + " – " + last.Format("Mon 2006-01-02") + " all day"
	}
	s, e := start.In(loc), end.In(loc)
	if e.Equal(s) {
		return s.Format("Mon 2006-01-02 15:04")
	}
	sy, sm, sd := s.Date()
	ey, em, ed := e.Date()
	if sy == ey && sm == em && sd == ed {
		return s.Format("Mon 2006-01-02 15:04") + "–" + e.Format("15:04")
	}
	return s.Format("Mon 2006-01-02 15:04") + " – " + e.Format("Mon 2006-01-02 15:04")
}

// allDayRange formats an all-day span as first and last (inclusive) date.
func allDayRange(start, end time.Time) (string, string) {
	last := addDays(end, -1)
	if last.Before(start) {
		last = start
	}
	return start.Format(time.DateOnly), last.Format(time.DateOnly)
}

func summarize(l located, loc *time.Location) EventSummary {
	e := l.inst.Event
	es := EventSummary{
		UID: e.UID, Calendar: l.cal.Name, Summary: e.Summary, Location: e.Location,
		AllDay: e.AllDay, Recurring: l.inst.Recurring, Status: e.Status, Unsupported: l.inst.Unsupported,
		start: l.inst.Start, end: l.inst.End,
	}
	if e.AllDay {
		es.Start, es.End = allDayRange(l.inst.Start, l.inst.End)
	} else {
		es.Start = l.inst.Start.In(loc).Format(time.RFC3339)
		es.End = l.inst.End.In(loc).Format(time.RFC3339)
	}
	return es
}

func (c *Integration) events(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		From, To, Calendar, Timezone string
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	s, err := c.open(ctx, a.Timezone, false)
	if err != nil {
		return integration.Result{}, err
	}
	from, to, err := resolveRange(a.From, a.To, c.now(), s.loc)
	if err != nil {
		return integration.Result{}, err
	}
	cals, err := s.pick(a.Calendar)
	if err != nil {
		return integration.Result{}, err
	}
	found, skipped, err := c.window(ctx, s, cals, from, to)
	if err != nil {
		return integration.Result{}, err
	}
	truncated := len(found) > maxEvents
	if truncated {
		found = found[:maxEvents]
	}
	multiCal := len(cals) > 1
	var sb strings.Builder
	fmt.Fprintf(&sb, "Events %s to %s (%s): %d\n", from.Format("Mon 2006-01-02 15:04"), to.Format("Mon 2006-01-02 15:04"), s.loc, len(found))
	out := make([]EventSummary, 0, len(found))
	for _, l := range found {
		es := summarize(l, s.loc)
		out = append(out, es)
		fmt.Fprintf(&sb, "- %s  %s", when(l.inst.Start, l.inst.End, es.AllDay, s.loc), oneLine(orDefault(es.Summary, "(no title)"), 120))
		if es.Location != "" {
			fmt.Fprintf(&sb, " @ %s", oneLine(es.Location, 80))
		}
		if multiCal {
			fmt.Fprintf(&sb, " [%s]", oneLine(es.Calendar, 40))
		}
		var flags []string
		if es.Recurring {
			flags = append(flags, "recurring")
		}
		if es.Status == "CANCELLED" || es.Status == "TENTATIVE" {
			flags = append(flags, strings.ToLower(es.Status))
		}
		if es.Unsupported != "" {
			flags = append(flags, "recurrence not fully expanded: "+es.Unsupported)
		}
		if len(flags) > 0 {
			fmt.Fprintf(&sb, " (%s)", strings.Join(flags, ", "))
		}
		fmt.Fprintf(&sb, " uid=%s\n", oneLine(es.UID, 200))
	}
	if truncated {
		fmt.Fprintf(&sb, "(truncated at %d events; narrow the window)\n", maxEvents)
	}
	if skipped > 0 {
		fmt.Fprintf(&sb, "(%d calendar object(s) could not be parsed and were skipped)\n", skipped)
	}
	return result(sb.String(), out), nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// --- get ---

// found is an event resource located by UID.
type found struct {
	cal    *Calendar
	res    resource
	ical   *Component
	master *Event // the VEVENT without RECURRENCE-ID (or the first)
	events []*Event
}

// findUID locates the resource holding uid in calName (or every calendar).
func (c *Integration) findUID(ctx context.Context, s *session, uid, calName string) (*found, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, errors.New("calendar: uid is required")
	}
	cals, err := s.pick(calName)
	if err != nil {
		return nil, err
	}
	for _, cal := range cals {
		var rs []resource
		rs, qerr := s.cl.query(ctx, cal, time.Time{}, time.Time{}, uid)
		if qerr != nil && isStatus(qerr, http.StatusUnauthorized) {
			return nil, qerr
		}
		if len(rs) == 0 {
			// Fall back to the conventional <uid>.ics name (used for
			// events Bosun creates).
			u, err := s.cl.resolve(cal.url, url.PathEscape(uid)+".ics")
			if err == nil {
				if r, err := s.cl.get(ctx, u); err == nil && r != nil {
					rs = []resource{*r}
				}
			}
		}
		for _, r := range rs {
			ical, evs, err := parseEvents(r.Data, s.loc, c.loc)
			if err != nil {
				continue
			}
			var mine []*Event
			for _, e := range evs {
				if e.UID == uid {
					mine = append(mine, e)
				}
			}
			if len(mine) == 0 {
				continue
			}
			f := &found{cal: cal, res: r, ical: ical, events: mine, master: mine[0]}
			for _, e := range mine {
				if e.RecurrenceID.IsZero() {
					f.master = e
					break
				}
			}
			return f, nil
		}
	}
	return nil, fmt.Errorf("calendar: no event with uid %q", uid)
}

// EventDetail is calendar.get's structured output.
type EventDetail struct {
	UID         string   `json:"uid"`
	Calendar    string   `json:"calendar"`
	Href        string   `json:"href"`
	ETag        string   `json:"etag,omitempty"`
	Summary     string   `json:"summary"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	AllDay      bool     `json:"all_day"`
	Location    string   `json:"location,omitempty"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status,omitempty"`
	RRule       string   `json:"rrule,omitempty"`
	Organizer   *Person  `json:"organizer,omitempty"`
	Attendees   []Person `json:"attendees,omitempty"`
	Overrides   int      `json:"overridden_instances,omitempty"`
}

func (c *Integration) get(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct{ UID, Calendar, Timezone string }
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	s, err := c.open(ctx, a.Timezone, false)
	if err != nil {
		return integration.Result{}, err
	}
	f, err := c.findUID(ctx, s, a.UID, a.Calendar)
	if err != nil {
		return integration.Result{}, err
	}
	e := f.master
	d := EventDetail{
		UID: e.UID, Calendar: f.cal.Name, Href: f.res.URL.EscapedPath(), ETag: f.res.ETag,
		Summary: e.Summary, AllDay: e.AllDay, Location: e.Location, Description: e.Description,
		Status: e.Status, RRule: e.RRule, Organizer: e.Organizer, Attendees: e.Attendees,
		Overrides: len(f.events) - 1,
	}
	if e.AllDay {
		d.Start, d.End = allDayRange(e.Start, e.End)
	} else {
		d.Start, d.End = e.Start.In(s.loc).Format(time.RFC3339), e.End.In(s.loc).Format(time.RFC3339)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", oneLine(orDefault(e.Summary, "(no title)"), 300))
	fmt.Fprintf(&sb, "When: %s (%s)\n", when(e.Start, e.End, e.AllDay, s.loc), s.loc)
	if e.RRule != "" {
		fmt.Fprintf(&sb, "Repeats: %s\n", oneLine(e.RRule, 200))
		if d.Overrides > 0 {
			fmt.Fprintf(&sb, "Modified instances: %d\n", d.Overrides)
		}
	}
	if e.Location != "" {
		fmt.Fprintf(&sb, "Location: %s\n", oneLine(e.Location, 300))
	}
	if e.Status != "" {
		fmt.Fprintf(&sb, "Status: %s\n", strings.ToLower(e.Status))
	}
	if e.Organizer != nil {
		fmt.Fprintf(&sb, "Organizer: %s\n", personString(*e.Organizer))
	}
	if len(e.Attendees) > 0 {
		sb.WriteString("Attendees:\n")
		for i, p := range e.Attendees {
			if i == 100 {
				fmt.Fprintf(&sb, "  … and %d more\n", len(e.Attendees)-100)
				break
			}
			fmt.Fprintf(&sb, "  - %s\n", personString(p))
		}
	}
	if e.Description != "" {
		desc := e.Description
		if len(desc) > maxDescOutput {
			desc = truncate(desc, maxDescOutput)
		}
		fmt.Fprintf(&sb, "Description:\n%s\n", desc)
	}
	fmt.Fprintf(&sb, "Calendar: %s\nUID: %s\nETag: %s\n", f.cal.Name, e.UID, f.res.ETag)
	return result(sb.String(), d), nil
}

func personString(p Person) string {
	s := oneLine(p.Email, 200)
	if p.Name != "" {
		s = oneLine(p.Name, 100) + " <" + s + ">"
	}
	var extra []string
	if p.PartStat != "" {
		extra = append(extra, strings.ToLower(p.PartStat))
	}
	if p.Role != "" && p.Role != "REQ-PARTICIPANT" {
		extra = append(extra, strings.ToLower(p.Role))
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}

// --- create / update ---

type editArgs struct {
	Summary     *string `json:"summary"`
	Start       *string `json:"start"`
	End         *string `json:"end"`
	Duration    *string `json:"duration"`
	AllDay      *bool   `json:"all_day"`
	Location    *string `json:"location"`
	Description *string `json:"description"`
	Timezone    string  `json:"timezone"`
}

func checkText(name string, p *string) error {
	if p != nil && len(*p) > maxTextField {
		return fmt.Errorf("calendar: %s is too long (max %d bytes)", name, maxTextField)
	}
	return nil
}

func (a *editArgs) validate() error {
	for _, x := range []struct {
		n string
		p *string
	}{{"summary", a.Summary}, {"location", a.Location}, {"description", a.Description}} {
		if err := checkText(x.n, x.p); err != nil {
			return err
		}
	}
	if a.End != nil && a.Duration != nil && *a.End != "" && *a.Duration != "" {
		return errors.New("calendar: give end or duration, not both")
	}
	return nil
}

// applyTimes computes the event's new start/end. cur* are the current
// values (zero for create).
func (c *Integration) applyTimes(a *editArgs, curStart, curEnd time.Time, curAllDay bool, loc *time.Location) (time.Time, time.Time, bool, error) {
	now := c.now()
	allDay := curAllDay
	if a.AllDay != nil {
		allDay = *a.AllDay
	}
	start := curStart
	if a.Start != nil && *a.Start != "" {
		var err error
		if allDay {
			start, err = parseDate(*a.Start, now, loc)
		} else {
			start, err = parseInstant(*a.Start, now, loc)
		}
		if err != nil {
			return start, start, false, fmt.Errorf("start: %w", err)
		}
	} else if allDay && !curAllDay {
		start = startOfDay(curStart, loc)
	}
	if start.IsZero() {
		return start, start, false, errors.New("calendar: start is required")
	}
	if allDay {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	}
	var end time.Time
	switch {
	case a.End != nil && *a.End != "":
		if allDay {
			last, err := parseDate(*a.End, now, loc)
			if err != nil {
				return start, start, false, fmt.Errorf("end: %w", err)
			}
			end = addDays(last, 1) // inclusive last day -> exclusive DTEND
		} else {
			e, err := parseInstant(*a.End, now, loc)
			if err != nil {
				return start, start, false, fmt.Errorf("end: %w", err)
			}
			end = e
		}
	case a.Duration != nil && *a.Duration != "":
		d, err := parseUserDuration(*a.Duration)
		if err != nil {
			return start, start, false, err
		}
		if allDay {
			days := int((d + 24*time.Hour - 1) / (24 * time.Hour))
			end = addDays(start, days)
		} else {
			end = start.Add(d)
		}
	case !curStart.IsZero() && allDay == curAllDay:
		// keep the current length
		if allDay {
			days := int((curEnd.Sub(curStart) + 12*time.Hour) / (24 * time.Hour))
			if days < 1 {
				days = 1
			}
			end = addDays(start, days)
		} else {
			end = start.Add(curEnd.Sub(curStart))
		}
	case allDay:
		end = addDays(start, 1)
	default:
		end = start.Add(time.Hour)
	}
	if !end.After(start) {
		return start, end, false, errors.New("calendar: the event must end after it starts")
	}
	return start, end, allDay, nil
}

func newUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) + "@bosun"
}

func setTimes(ev *Component, start, end time.Time, allDay bool) {
	ev.Remove("DURATION")
	ev.Set(dateTimeProp("DTSTART", start, allDay))
	ev.Set(dateTimeProp("DTEND", end, allDay))
}

func (c *Integration) create(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		editArgs
		Calendar string `json:"calendar"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if err := a.validate(); err != nil {
		return integration.Result{}, err
	}
	if a.Summary == nil || strings.TrimSpace(*a.Summary) == "" {
		return integration.Result{}, errors.New("calendar: summary is required")
	}
	if a.Start == nil || *a.Start == "" {
		return integration.Result{}, errors.New("calendar: start is required")
	}
	s, err := c.open(ctx, a.Timezone, false)
	if err != nil {
		return integration.Result{}, err
	}
	cal, err := s.defaultCalendar(a.Calendar)
	if err != nil {
		return integration.Result{}, err
	}
	start, end, allDay, err := c.applyTimes(&a.editArgs, time.Time{}, time.Time{}, false, s.loc)
	if err != nil {
		return integration.Result{}, err
	}
	uid := newUID()
	now := c.now()
	ev := &Component{Name: "VEVENT"}
	ev.Set(&Property{Name: "UID", Value: uid})
	ev.Set(&Property{Name: "DTSTAMP", Value: utcStamp(now)})
	ev.Set(&Property{Name: "CREATED", Value: utcStamp(now)})
	ev.Set(&Property{Name: "LAST-MODIFIED", Value: utcStamp(now)})
	setTimes(ev, start, end, allDay)
	ev.SetText("SUMMARY", strings.TrimSpace(*a.Summary))
	if a.Location != nil {
		ev.SetText("LOCATION", *a.Location)
	}
	if a.Description != nil {
		ev.SetText("DESCRIPTION", *a.Description)
	}
	if allDay {
		ev.Set(&Property{Name: "TRANSP", Value: "TRANSPARENT"})
	}
	vcal := &Component{Name: "VCALENDAR", Props: []*Property{
		{Name: "VERSION", Value: "2.0"},
		{Name: "PRODID", Value: "-//Bosun//Calendar 0.1//EN"},
		{Name: "CALSCALE", Value: "GREGORIAN"},
	}, Children: []*Component{ev}}
	u, err := s.cl.resolve(cal.url, strings.TrimSuffix(uid, "@bosun")+".ics")
	if err != nil {
		return integration.Result{}, err
	}
	etag, err := s.cl.put(ctx, u, vcal.Encode(), "", true)
	if err != nil {
		return integration.Result{}, err
	}
	w := when(start, end, allDay, s.loc)
	data := map[string]any{"uid": uid, "href": u.EscapedPath(), "calendar": cal.Name, "etag": etag, "when": w}
	return result(fmt.Sprintf("Created %q in %s: %s\nuid=%s href=%s\n", oneLine(*a.Summary, 200), cal.Name, w, uid, u.EscapedPath()), data), nil
}

func (c *Integration) update(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		editArgs
		UID      string `json:"uid"`
		Calendar string `json:"calendar"`
		ETag     string `json:"etag"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if err := a.validate(); err != nil {
		return integration.Result{}, err
	}
	if a.Summary != nil && strings.TrimSpace(*a.Summary) == "" {
		return integration.Result{}, errors.New("calendar: summary cannot be empty")
	}
	s, err := c.open(ctx, a.Timezone, false)
	if err != nil {
		return integration.Result{}, err
	}
	f, err := c.findUID(ctx, s, a.UID, a.Calendar)
	if err != nil {
		return integration.Result{}, err
	}
	if f.cal.ReadOnly {
		return integration.Result{}, fmt.Errorf("calendar: %q is read-only", f.cal.Name)
	}
	if a.ETag != "" && f.res.ETag != "" && a.ETag != f.res.ETag {
		return integration.Result{}, ErrConflict
	}
	m := f.master
	if !m.RecurrenceID.IsZero() {
		return integration.Result{}, errors.New("calendar: only a single modified instance of this recurring event is stored here; edit it in your calendar app")
	}
	ev := m.comp
	changed := []string{}
	if a.Start != nil || a.End != nil || a.Duration != nil || a.AllDay != nil {
		start, end, allDay, err := c.applyTimes(&a.editArgs, m.Start, m.End, m.AllDay, s.loc)
		if err != nil {
			return integration.Result{}, err
		}
		if m.RRule != "" && allDay != m.AllDay {
			return integration.Result{}, errors.New("calendar: cannot switch a recurring event between all-day and timed")
		}
		setTimes(ev, start, end, allDay)
		changed = append(changed, "time")
	}
	if a.Summary != nil {
		ev.SetText("SUMMARY", strings.TrimSpace(*a.Summary))
		changed = append(changed, "summary")
	}
	if a.Location != nil {
		ev.SetText("LOCATION", *a.Location)
		changed = append(changed, "location")
	}
	if a.Description != nil {
		ev.SetText("DESCRIPTION", *a.Description)
		changed = append(changed, "description")
	}
	if len(changed) == 0 {
		return integration.Result{}, errors.New("calendar: nothing to change")
	}
	now := c.now()
	ev.Set(&Property{Name: "DTSTAMP", Value: utcStamp(now)})
	ev.Set(&Property{Name: "LAST-MODIFIED", Value: utcStamp(now)})
	ev.Set(&Property{Name: "SEQUENCE", Value: fmt.Sprint(m.Sequence + 1)})
	etag, err := s.cl.put(ctx, f.res.URL, f.ical.Encode(), f.res.ETag, false)
	if err != nil {
		return integration.Result{}, err
	}
	data := map[string]any{"uid": m.UID, "href": f.res.URL.EscapedPath(), "calendar": f.cal.Name, "etag": etag, "changed": changed}
	return result(fmt.Sprintf("Updated %s of uid=%s in %s.\n", strings.Join(changed, ", "), m.UID, f.cal.Name), data), nil
}

func (c *Integration) delete(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct{ UID, Calendar, ETag string }
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	s, err := c.open(ctx, "", false)
	if err != nil {
		return integration.Result{}, err
	}
	f, err := c.findUID(ctx, s, a.UID, a.Calendar)
	if err != nil {
		return integration.Result{}, err
	}
	if f.cal.ReadOnly {
		return integration.Result{}, fmt.Errorf("calendar: %q is read-only", f.cal.Name)
	}
	if a.ETag != "" && f.res.ETag != "" && a.ETag != f.res.ETag {
		return integration.Result{}, ErrConflict
	}
	if err := s.cl.del(ctx, f.res.URL, f.res.ETag); err != nil {
		return integration.Result{}, err
	}
	data := map[string]any{"uid": f.master.UID, "href": f.res.URL.EscapedPath(), "calendar": f.cal.Name}
	return result(fmt.Sprintf("Deleted %q (uid=%s) from %s.\n", oneLine(f.master.Summary, 200), f.master.UID, f.cal.Name), data), nil
}

// --- freebusy ---

// Interval is one busy period.
type Interval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func (c *Integration) freebusy(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct{ From, To, Timezone string }
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	s, err := c.open(ctx, a.Timezone, false)
	if err != nil {
		return integration.Result{}, err
	}
	from, to, err := resolveRange(a.From, a.To, c.now(), s.loc)
	if err != nil {
		return integration.Result{}, err
	}
	var cals []*Calendar
	for i := range s.cals {
		if !s.cals[i].ReadOnly {
			cals = append(cals, &s.cals[i])
		}
	}
	found, _, err := c.window(ctx, s, cals, from, to)
	if err != nil {
		return integration.Result{}, err
	}
	type span struct{ s, e time.Time }
	var spans []span
	for _, l := range found {
		e := l.inst.Event
		if e.Transp == "TRANSPARENT" || e.Status == "CANCELLED" {
			continue
		}
		st, en := l.inst.Start, l.inst.End
		if e.AllDay { // dates are in the request zone already
			st = time.Date(st.Year(), st.Month(), st.Day(), 0, 0, 0, 0, s.loc)
			en = time.Date(en.Year(), en.Month(), en.Day(), 0, 0, 0, 0, s.loc)
		}
		if st.Before(from) {
			st = from
		}
		if en.After(to) {
			en = to
		}
		if en.After(st) {
			spans = append(spans, span{st, en})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s.Before(spans[j].s) })
	var merged []span
	for _, sp := range spans {
		if n := len(merged); n > 0 && !sp.s.After(merged[n-1].e) {
			if sp.e.After(merged[n-1].e) {
				merged[n-1].e = sp.e
			}
			continue
		}
		merged = append(merged, sp)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Busy %s to %s (%s): %d interval(s)\n", from.Format("Mon 2006-01-02 15:04"), to.Format("Mon 2006-01-02 15:04"), s.loc, len(merged))
	out := make([]Interval, 0, len(merged))
	for _, m := range merged {
		out = append(out, Interval{Start: m.s.In(s.loc).Format(time.RFC3339), End: m.e.In(s.loc).Format(time.RFC3339)})
		fmt.Fprintf(&sb, "- %s\n", when(m.s, m.e, false, s.loc))
	}
	return result(sb.String(), out), nil
}
