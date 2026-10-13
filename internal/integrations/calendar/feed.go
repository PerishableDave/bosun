package calendar

// Read-only ICS (iCalendar feed) backend. A credential of kind "ics" or
// "ics:<name>" holds a private feed URL as its Secret (Google's "Secret
// address in iCal format", an Outlook.com published calendar, an iCloud
// public calendar, ...). The feed is fetched over https only, through a
// dialer that refuses private and loopback addresses (the URL is user
// supplied), capped at 5 MB, parsed once and cached per user for five
// minutes. Each feed appears as one read-only calendar named after the
// credential kind's suffix.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/providers"
)

// KindICS is the credential kind for a read-only iCalendar feed; suffixed
// kinds ("ics:family") add more feeds.
const KindICS = "ics"

const (
	maxFeedBytes    = maxICSBytes // 5 MB
	maxFeeds        = 20
	maxFeedRedirect = 5
	feedTimeout     = 30 * time.Second
	defaultFeedName = "calendar"
)

// errReadOnlyICS is returned by the write tools for ICS-only accounts.
var errReadOnlyICS = errors.New("calendar: this calendar is read-only (ICS); link a CalDAV account " +
	"(`bosun connect email` for iCloud or Fastmail, or `bosun credentials set calendar password ...`) or Google OAuth " +
	"(`bosun connect google --scopes caldav`) for edits")

var errBlockedAddr = errors.New("address is private, loopback, or otherwise not publicly routable")

// feedRef is one configured feed.
type feedRef struct {
	kind string // "ics" or "ics:<name>"
	name string // calendar name shown to the model
	u    *url.URL
}

// parsedFeed is a feed parsed for one floating time zone.
type parsedFeed struct {
	tz      *tzCtx
	groups  [][]*Event // events grouped by UID, in feed order
	byUID   map[string][]*Event
	skipped int // VEVENTs that could not be parsed
}

type feedEntry struct {
	at     time.Time
	data   string
	parsed map[string]*parsedFeed // by floating zone name
}

// isICSKind reports whether kind is "ics" or "ics:<name>".
func isICSKind(kind string) bool {
	return kind == KindICS || (strings.HasPrefix(kind, KindICS+":") && len(kind) > len(KindICS)+1)
}

// feedName is the calendar name for a feed credential.
func feedName(kind string, fields map[string]string) string {
	if _, suffix, ok := strings.Cut(kind, ":"); ok && suffix != "" {
		return suffix
	}
	if n := strings.TrimSpace(fields["name"]); n != "" && len(n) <= 64 && !strings.ContainsAny(n, "\r\n") {
		return n
	}
	return defaultFeedName
}

// feedFromCredential validates an ics credential.
func feedFromCredential(cred integration.Credential) (feedRef, error) {
	u, err := providers.NormalizeICSURL(cred.Secret)
	if err != nil {
		return feedRef{}, fmt.Errorf("calendar: the %s credential: %v", cred.Kind, err)
	}
	return feedRef{kind: cred.Kind, name: feedName(cred.Kind, cred.Fields), u: u}, nil
}

// icsFeeds returns the calling user's feeds: "ics", plus every "ics:<name>"
// the credential source can list.
func (c *Integration) icsFeeds(ctx context.Context) ([]feedRef, error) {
	kinds := []string{KindICS}
	if l, ok := c.creds.(integration.CredentialKindLister); ok {
		ks, err := l.Kinds(ctx)
		if err != nil && !errors.Is(err, integration.ErrNoCredential) {
			return nil, fmt.Errorf("calendar: list feeds: %w", err)
		}
		kinds = kinds[:0]
		for _, k := range ks {
			if isICSKind(k) {
				kinds = append(kinds, k)
			}
		}
		sort.Strings(kinds)
	}
	var out []feedRef
	seen := map[string]bool{}
	for _, k := range kinds {
		cred, err := c.creds.Get(ctx, k)
		if errors.Is(err, integration.ErrNoCredential) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("calendar: read %s credential: %w", k, err)
		}
		if cred.Kind == "" {
			cred.Kind = k
		}
		f, err := feedFromCredential(cred)
		if err != nil {
			return nil, err
		}
		for seen[strings.ToLower(f.name)] {
			f.name += "-" + strings.TrimPrefix(k, KindICS+":")
		}
		seen[strings.ToLower(f.name)] = true
		out = append(out, f)
		if len(out) == maxFeeds {
			break
		}
	}
	return out, nil
}

// --- fetching ---

// blockedIP reports whether ip must not be contacted for a feed.
func blockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		if b[0] == 0 || (b[0] == 100 && b[1]&0xc0 == 64) { // 0.0.0.0/8, CGNAT 100.64/10
			return true
		}
	}
	return false
}

// newFeedClient returns the client for feed fetches. Unless allowPrivate
// (tests), its dialer checks the address actually connected to, which
// defeats DNS rebinding and redirects to internal hosts. Redirects must stay
// on https. Proxy environment variables are ignored.
func newFeedClient(allowPrivate bool, tr *http.Transport) *http.Client {
	d := &net.Dialer{
		Timeout: feedTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return errBlockedAddr
			}
			return nil
		},
	}
	if tr == nil {
		tr = &http.Transport{}
	} else {
		tr = tr.Clone()
	}
	tr.Proxy = nil
	tr.DialContext = d.DialContext
	tr.TLSHandshakeTimeout = feedTimeout
	tr.ResponseHeaderTimeout = feedTimeout
	tr.DisableKeepAlives = true
	return &http.Client{
		Timeout:   feedTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxFeedRedirect {
				return fmt.Errorf("stopped after %d redirects", maxFeedRedirect)
			}
			if req.URL.Scheme != "https" {
				return errors.New("the feed redirected to a non-https URL")
			}
			return nil
		},
	}
}

// checkFeedHost is an early, friendlier version of the dialer's check.
func (c *Integration) checkFeedHost(ctx context.Context, host string) error {
	if c.feedAllowPrivate {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return errBlockedAddr
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return errBlockedAddr
		}
	}
	return nil
}

// fetchFeed downloads a feed. Error messages name the feed, never its URL
// (the URL is the secret).
func (c *Integration) fetchFeed(ctx context.Context, f feedRef) (string, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("calendar: feed %q: %s", f.name, fmt.Sprintf(format, args...))
	}
	if err := c.checkFeedHost(ctx, f.u.Hostname()); err != nil {
		return "", fail("refusing to fetch from %s: %v", f.u.Hostname(), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.u.String(), nil)
	if err != nil {
		return "", fail("bad URL")
	}
	req.Header.Set("User-Agent", "bosun-calendar/0.1")
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	resp, err := c.feedHTTP().Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return "", fail("fetch failed: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fail("the server refused the link (HTTP %d); it may have been reset or unpublished, so copy the current link again", resp.StatusCode)
	default:
		return "", fail("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return "", fail("read: %v", err)
	}
	if len(data) > maxFeedBytes {
		return "", fail("the feed is larger than %d MB", maxFeedBytes>>20)
	}
	s := string(data)
	head := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(s[:min(len(s), 256)], "\uFEFF")))
	if !strings.HasPrefix(head, "BEGIN:VCALENDAR") {
		return "", fail("the link did not return an iCalendar feed (expected BEGIN:VCALENDAR; is it the private/secret iCal address?)")
	}
	return s, nil
}

func (c *Integration) feedHTTP() *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.feedClient == nil {
		c.feedClient = newFeedClient(c.feedAllowPrivate, nil)
	}
	return c.feedClient
}

// parseFeed parses a whole feed leniently: malformed lines and VEVENTs are
// skipped rather than failing the calendar.
func parseFeed(data string, floating, fallback *time.Location) (*parsedFeed, error) {
	cal, err := parseICS(data, true)
	if err != nil {
		return nil, err
	}
	if cal.Name != "VCALENDAR" {
		return nil, errors.New("not a VCALENDAR")
	}
	pf := &parsedFeed{tz: newTZCtx(cal, floating, fallback), byUID: map[string][]*Event{}}
	var order []string
	for _, comp := range cal.Children {
		if comp.Name != "VEVENT" {
			continue
		}
		e, err := parseEvent(comp, pf.tz)
		if err != nil {
			pf.skipped++
			continue
		}
		if e.UID == "" {
			e.UID = fmt.Sprintf("feed-event-%d", len(order)+pf.skipped)
		}
		if _, ok := pf.byUID[e.UID]; !ok {
			order = append(order, e.UID)
		}
		pf.byUID[e.UID] = append(pf.byUID[e.UID], e)
	}
	pf.groups = make([][]*Event, 0, len(order))
	for _, uid := range order {
		pf.groups = append(pf.groups, pf.byUID[uid])
	}
	return pf, nil
}

// feed returns feed f parsed for floating zone loc, from the per-user cache
// when it is younger than cacheTTL.
func (c *Integration) feed(ctx context.Context, f feedRef, loc *time.Location) (*parsedFeed, error) {
	uid, _ := integration.UserIDFromContext(ctx)
	key := fmt.Sprintf("%d|%s", uid, fingerprint(f.u.String()))
	now := c.now()
	c.mu.Lock()
	ent, ok := c.feeds[key]
	if ok && now.Sub(ent.at) >= cacheTTL {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		data, err := c.fetchFeed(ctx, f)
		if err != nil {
			return nil, err
		}
		ent = &feedEntry{at: now, data: data, parsed: map[string]*parsedFeed{}}
		c.mu.Lock()
		for k, e := range c.feeds { // drop stale entries
			if now.Sub(e.at) >= cacheTTL {
				delete(c.feeds, k)
			}
		}
		c.feeds[key] = ent
		c.mu.Unlock()
	}
	c.mu.Lock()
	pf := ent.parsed[loc.String()]
	c.mu.Unlock()
	if pf != nil {
		return pf, nil
	}
	pf, err := parseFeed(ent.data, loc, c.loc)
	if err != nil {
		return nil, fmt.Errorf("calendar: feed %q: %v", f.name, err)
	}
	c.mu.Lock()
	ent.parsed[loc.String()] = pf
	c.mu.Unlock()
	return pf, nil
}

// feedInstances expands a feed's events overlapping [from, to).
func feedInstances(pf *parsedFeed, from, to time.Time) []Instance {
	var out []Instance
	for _, g := range pf.groups {
		out = append(out, expand(g, from, to, pf.tz, maxEvents+1)...)
	}
	return out
}

// testFeed fetches and parses a feed credential (TestCredential).
func (c *Integration) testFeed(ctx context.Context, cred integration.Credential) error {
	f, err := feedFromCredential(cred)
	if err != nil {
		return err
	}
	data, err := c.fetchFeed(ctx, f)
	if err != nil {
		return err
	}
	if _, err := parseFeed(data, c.loc, c.loc); err != nil {
		return fmt.Errorf("calendar: feed %q could not be parsed: %v", f.name, err)
	}
	return nil
}
