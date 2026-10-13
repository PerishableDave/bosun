package calendar

// A minimal CalDAV (RFC 4791) client over WebDAV (RFC 4918): discovery
// (RFC 6764 well-known URI, current-user-principal, calendar-home-set),
// calendar-query with a time-range, calendar-multiget, and GET/PUT/DELETE
// with ETag preconditions. Hand-written XML; stdlib only.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsApple  = "http://apple.com/ns/ical/"

	maxResponseBytes = 32 << 20
	maxRedirects     = 5
)

// ErrConflict is returned when an If-Match precondition fails: the event
// changed on the server since it was read.
var ErrConflict = errors.New("calendar: the event was changed on the server since it was read (ETag mismatch); fetch it again and retry")

// httpError is a non-success HTTP status from the server.
type httpError struct {
	Method, URL string
	Status      int
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("calendar: %s %s: HTTP %d", e.Method, e.URL, e.Status)
	switch e.Status {
	case http.StatusUnauthorized:
		msg += " (authentication failed: check the stored credential; iCloud requires an app-specific password, and a Google token may need `bosun connect google --scopes caldav` again)"
	case http.StatusForbidden:
		msg += " (forbidden: the account may not have access to this calendar or it is read-only)"
	}
	return msg
}

func isStatus(err error, code int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == code
}

// account is one user's resolved CalDAV account.
type account struct {
	kind      string    // credential kind used
	start     *url.URL  // configured URL (server root, principal, home or calendar)
	principal string    // known principal URL (Google)
	fallback  string    // calendar to use if discovery finds no home set (Google primary)
	auth      string    // Authorization header value
	label     string    // user / email, for messages
	defCal    string    // preferred default calendar name (credential field "calendar")
	key       string    // cache key fingerprint (no secrets in clear)
	feeds     []feedRef // kind "ics": the read-only feeds
}

type client struct {
	hc   *http.Client
	acct *account
}

// isLoopback reports whether host (no port) is a loopback name/address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkScheme allows https, and http only to loopback hosts.
func checkScheme(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("calendar: refusing plain-http URL %s: credentials would be sent unencrypted (use https://; http is only allowed to localhost)", u.Redacted())
	default:
		return fmt.Errorf("calendar: unsupported URL scheme in %q", u.Redacted())
	}
}

// site returns the registrable-ish part of a host: the last two labels (or
// the whole host for IPs and single-label names).
func site(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if net.ParseIP(host) != nil {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// checkTarget allows a URL the server pointed us to (redirect, principal,
// home set) only if it keeps the security level and stays on the same site
// (iCloud moves accounts to pNN-caldav.icloud.com), so credentials are never
// sent to an unrelated host.
func (c *client) checkTarget(u *url.URL) error {
	if err := checkScheme(u); err != nil {
		return err
	}
	if c.acct.start.Scheme == "https" && u.Scheme != "https" {
		return fmt.Errorf("calendar: refusing to follow %s from an https server to plain http", u.Redacted())
	}
	if site(u.Hostname()) != site(c.acct.start.Hostname()) {
		return fmt.Errorf("calendar: server pointed to %s, which is not on the configured host %s; refusing to send credentials there", u.Host, c.acct.start.Host)
	}
	return nil
}

func (c *client) resolve(base *url.URL, href string) (*url.URL, error) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return nil, fmt.Errorf("calendar: bad href %q", href)
	}
	u := base.ResolveReference(ref)
	u.Fragment = ""
	if err := c.checkTarget(u); err != nil {
		return nil, err
	}
	return u, nil
}

// do sends a request, following redirects itself (net/http would turn
// PROPFIND/REPORT into GET). It returns the final URL, response, and body.
func (c *client) do(ctx context.Context, method string, u *url.URL, hdr map[string]string, body []byte) (*url.URL, *http.Response, []byte, error) {
	for i := 0; ; i++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
		if err != nil {
			return u, nil, nil, err
		}
		if c.acct.auth != "" {
			req.Header.Set("Authorization", c.acct.auth)
		}
		req.Header.Set("User-Agent", "bosun-calendar/0.1")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return u, nil, nil, fmt.Errorf("calendar: %s %s: %w", method, u.Redacted(), err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			return u, nil, nil, fmt.Errorf("calendar: %s %s: read: %w", method, u.Redacted(), err)
		}
		if len(data) > maxResponseBytes {
			return u, nil, nil, fmt.Errorf("calendar: %s %s: response too large", method, u.Redacted())
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			if loc == "" || i >= maxRedirects {
				return u, resp, data, &httpError{Method: method, URL: u.Redacted(), Status: resp.StatusCode}
			}
			next, err := c.resolve(u, loc)
			if err != nil {
				return u, nil, nil, err
			}
			u = next
			continue
		}
		return u, resp, data, nil
	}
}

// --- multistatus ---

type hrefProp struct {
	Href string `xml:"DAV: href"`
}

type xmlAny struct {
	XMLName xml.Name
}

type prop struct {
	ResourceType *struct {
		Items []xmlAny `xml:",any"`
	} `xml:"DAV: resourcetype"`
	DisplayName          string    `xml:"DAV: displayname"`
	CurrentUserPrincipal *hrefProp `xml:"DAV: current-user-principal"`
	CalendarHomeSet      *struct {
		Hrefs []string `xml:"DAV: href"`
	} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	Color        string `xml:"http://apple.com/ns/ical/ calendar-color"`
	Description  string `xml:"urn:ietf:params:xml:ns:caldav calendar-description"`
	PrivilegeSet *struct {
		Privileges []struct {
			Items []xmlAny `xml:",any"`
		} `xml:"DAV: privilege"`
	} `xml:"DAV: current-user-privilege-set"`
	CompSet *struct {
		Comps []struct {
			Name string `xml:"name,attr"`
		} `xml:"urn:ietf:params:xml:ns:caldav comp"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	ETag         string `xml:"DAV: getetag"`
	CalendarData string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

type propstat struct {
	Prop   prop   `xml:"DAV: prop"`
	Status string `xml:"DAV: status"`
}

type msResponse struct {
	Hrefs     []string   `xml:"DAV: href"`
	Propstats []propstat `xml:"DAV: propstat"`
	Status    string     `xml:"DAV: status"`
}

type multistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"DAV: response"`
}

// ok200 merges the successful propstats of a response.
func (r *msResponse) props() prop {
	var p prop
	for _, ps := range r.Propstats {
		if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
			continue
		}
		q := ps.Prop
		if q.ResourceType != nil {
			p.ResourceType = q.ResourceType
		}
		if q.DisplayName != "" {
			p.DisplayName = q.DisplayName
		}
		if q.CurrentUserPrincipal != nil && q.CurrentUserPrincipal.Href != "" {
			p.CurrentUserPrincipal = q.CurrentUserPrincipal
		}
		if q.CalendarHomeSet != nil && len(q.CalendarHomeSet.Hrefs) > 0 {
			p.CalendarHomeSet = q.CalendarHomeSet
		}
		if q.Color != "" {
			p.Color = q.Color
		}
		if q.Description != "" {
			p.Description = q.Description
		}
		if q.PrivilegeSet != nil {
			p.PrivilegeSet = q.PrivilegeSet
		}
		if q.CompSet != nil {
			p.CompSet = q.CompSet
		}
		if q.ETag != "" {
			p.ETag = q.ETag
		}
		if q.CalendarData != "" {
			p.CalendarData = q.CalendarData
		}
	}
	return p
}

func (p prop) hasType(space, local string) bool {
	if p.ResourceType == nil {
		return false
	}
	for _, it := range p.ResourceType.Items {
		if it.XMLName.Space == space && it.XMLName.Local == local {
			return true
		}
	}
	return false
}

// writable reports whether the privilege set grants writing; an absent set
// is assumed writable.
func (p prop) writable() bool {
	if p.PrivilegeSet == nil {
		return true
	}
	for _, pr := range p.PrivilegeSet.Privileges {
		for _, it := range pr.Items {
			switch it.XMLName.Local {
			case "all", "write", "write-content", "bind":
				return true
			}
		}
	}
	return false
}

func (p prop) supportsEvents() bool {
	if p.CompSet == nil || len(p.CompSet.Comps) == 0 {
		return true
	}
	for _, c := range p.CompSet.Comps {
		if strings.EqualFold(c.Name, "VEVENT") {
			return true
		}
	}
	return false
}

func (c *client) multistatus(ctx context.Context, method string, u *url.URL, depth string, body string) (*url.URL, []msResponse, error) {
	hdr := map[string]string{"Content-Type": `application/xml; charset="utf-8"`, "Depth": depth}
	final, resp, data, err := c.do(ctx, method, u, hdr, []byte(body))
	if err != nil {
		return final, nil, err
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return final, nil, &httpError{Method: method, URL: final.Redacted(), Status: resp.StatusCode}
	}
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return final, nil, fmt.Errorf("calendar: %s %s: bad multistatus: %w", method, final.Redacted(), err)
	}
	return final, ms.Responses, nil
}

const propfindPrincipal = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:current-user-principal/><c:calendar-home-set/><d:resourcetype/></d:prop></d:propfind>`

const propfindHome = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/></d:prop></d:propfind>`

const propfindCalendars = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/">` +
	`<d:prop><d:resourcetype/><d:displayname/><a:calendar-color/><c:calendar-description/>` +
	`<d:current-user-privilege-set/><c:supported-calendar-component-set/></d:prop></d:propfind>`

// Calendar is one calendar collection.
type Calendar struct {
	Name        string `json:"name"`
	Href        string `json:"href"` // path on the server
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"read_only"`

	url  *url.URL
	feed *parsedFeed // ICS feed calendars only
}

func samePath(a, b string) bool {
	norm := func(s string) string {
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		return strings.TrimSuffix(s, "/")
	}
	return norm(a) == norm(b)
}

// discover finds the account's calendars.
func (c *client) discover(ctx context.Context) ([]Calendar, error) {
	start := c.acct.start
	var principal *url.URL
	var home *url.URL

	if c.acct.principal != "" {
		u, err := c.resolve(start, c.acct.principal)
		if err != nil {
			return nil, err
		}
		principal = u
	} else {
		// RFC 6764: try /.well-known/caldav at the server root first.
		var tries []*url.URL
		if start.Path == "" || start.Path == "/" {
			wk, _ := c.resolve(start, "/.well-known/caldav")
			tries = append(tries, wk)
		}
		tries = append(tries, start)
		var lastErr error
		for _, u := range tries {
			final, rs, err := c.multistatus(ctx, "PROPFIND", u, "0", propfindPrincipal)
			if err != nil {
				lastErr = err
				if isStatus(err, http.StatusUnauthorized) {
					return nil, err
				}
				continue
			}
			for _, r := range rs {
				p := r.props()
				if p.hasType(nsCalDAV, "calendar") {
					// The URL is a calendar collection itself.
					return c.listCollections(ctx, final, "0")
				}
				if p.CalendarHomeSet != nil && home == nil {
					if home, err = c.resolve(final, p.CalendarHomeSet.Hrefs[0]); err != nil {
						return nil, err
					}
				}
				if p.CurrentUserPrincipal != nil && principal == nil {
					if principal, err = c.resolve(final, p.CurrentUserPrincipal.Href); err != nil {
						return nil, err
					}
				}
			}
			if principal != nil || home != nil {
				lastErr = nil
				break
			}
		}
		if principal == nil && home == nil {
			if lastErr != nil {
				return nil, fmt.Errorf("calendar: discovery failed at %s: %w", start.Redacted(), lastErr)
			}
			return nil, fmt.Errorf("calendar: %s did not report a CalDAV principal or calendar home; set the credential's url field to your calendar home or principal URL", start.Redacted())
		}
	}
	if home == nil {
		final, rs, err := c.multistatus(ctx, "PROPFIND", principal, "0", propfindHome)
		if err != nil && (c.acct.fallback == "" || isStatus(err, http.StatusUnauthorized) || isStatus(err, http.StatusForbidden)) {
			return nil, fmt.Errorf("calendar: read calendar-home-set: %w", err)
		}
		for _, r := range rs {
			if p := r.props(); p.CalendarHomeSet != nil {
				if home, err = c.resolve(final, p.CalendarHomeSet.Hrefs[0]); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	if home == nil {
		if c.acct.fallback != "" {
			u, err := c.resolve(start, c.acct.fallback)
			if err != nil {
				return nil, err
			}
			return []Calendar{{Name: c.acct.label, Href: u.Path, url: u}}, nil
		}
		return nil, errors.New("calendar: the server reported no calendar-home-set")
	}
	cals, err := c.listCollections(ctx, home, "1")
	if err != nil {
		return nil, err
	}
	if len(cals) == 0 && c.acct.fallback != "" {
		u, err := c.resolve(start, c.acct.fallback)
		if err != nil {
			return nil, err
		}
		cals = []Calendar{{Name: c.acct.label, Href: u.Path, url: u}}
	}
	return cals, nil
}

func (c *client) listCollections(ctx context.Context, u *url.URL, depth string) ([]Calendar, error) {
	final, rs, err := c.multistatus(ctx, "PROPFIND", u, depth, propfindCalendars)
	if err != nil {
		return nil, fmt.Errorf("calendar: list calendars: %w", err)
	}
	var out []Calendar
	for _, r := range rs {
		if len(r.Hrefs) == 0 {
			continue
		}
		p := r.props()
		if !p.hasType(nsCalDAV, "calendar") || !p.supportsEvents() {
			continue
		}
		cu, err := c.resolve(final, r.Hrefs[0])
		if err != nil {
			continue
		}
		if !strings.HasSuffix(cu.Path, "/") {
			cu.Path += "/"
		}
		name := strings.TrimSpace(p.DisplayName)
		if name == "" {
			segs := strings.Split(strings.Trim(cu.Path, "/"), "/")
			name = segs[len(segs)-1]
		}
		out = append(out, Calendar{
			Name: name, Href: cu.EscapedPath(), Color: strings.TrimSpace(p.Color),
			Description: strings.TrimSpace(p.Description), ReadOnly: !p.writable(), url: cu,
		})
	}
	return out, nil
}

// resource is one calendar object resource (an .ics file on the server).
type resource struct {
	URL  *url.URL
	ETag string
	Data string
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// query runs a calendar-query REPORT for VEVENTs overlapping [from, to)
// (or, if uid is non-empty, with that UID).
func (c *client) query(ctx context.Context, cal *Calendar, from, to time.Time, uid string) ([]resource, error) {
	var filter string
	if uid != "" {
		filter = `<c:prop-filter name="UID"><c:text-match collation="i;octet">` + xmlEscape(uid) + `</c:text-match></c:prop-filter>`
	} else {
		filter = fmt.Sprintf(`<c:time-range start="%s" end="%s"/>`, utcStamp(from), utcStamp(to))
	}
	body := `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>` +
		`<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">` + filter +
		`</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	final, rs, err := c.multistatus(ctx, "REPORT", cal.url, "1", body)
	if err != nil {
		return nil, err
	}
	return c.collect(ctx, cal, final, rs)
}

// collect turns REPORT responses into resources, fetching any whose
// calendar-data the server left out with a calendar-multiget.
func (c *client) collect(ctx context.Context, cal *Calendar, base *url.URL, rs []msResponse) ([]resource, error) {
	var out []resource
	var missing []string
	for _, r := range rs {
		if len(r.Hrefs) == 0 {
			continue
		}
		u, err := c.resolve(base, r.Hrefs[0])
		if err != nil || samePath(u.Path, cal.url.Path) {
			continue
		}
		p := r.props()
		if p.CalendarData == "" {
			if r.Status == "" || strings.Contains(r.Status, " 200") {
				missing = append(missing, u.EscapedPath())
			}
			continue
		}
		out = append(out, resource{URL: u, ETag: p.ETag, Data: p.CalendarData})
	}
	if len(missing) > 0 {
		more, err := c.multiget(ctx, cal, missing)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// multiget fetches resources by href with a calendar-multiget REPORT.
func (c *client) multiget(ctx context.Context, cal *Calendar, hrefs []string) ([]resource, error) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`)
	for _, h := range hrefs {
		sb.WriteString("<d:href>" + xmlEscape(h) + "</d:href>")
	}
	sb.WriteString("</c:calendar-multiget>")
	final, rs, err := c.multistatus(ctx, "REPORT", cal.url, "1", sb.String())
	if err != nil {
		return nil, err
	}
	var out []resource
	for _, r := range rs {
		if len(r.Hrefs) == 0 {
			continue
		}
		p := r.props()
		if p.CalendarData == "" {
			continue
		}
		u, err := c.resolve(final, r.Hrefs[0])
		if err != nil {
			continue
		}
		out = append(out, resource{URL: u, ETag: p.ETag, Data: p.CalendarData})
	}
	return out, nil
}

// get fetches one resource. A 404 returns (nil, nil).
func (c *client) get(ctx context.Context, u *url.URL) (*resource, error) {
	final, resp, data, err := c.do(ctx, http.MethodGet, u, map[string]string{"Accept": "text/calendar"}, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Method: "GET", URL: final.Redacted(), Status: resp.StatusCode}
	}
	return &resource{URL: final, ETag: resp.Header.Get("ETag"), Data: string(data)}, nil
}

// put stores a resource. ifMatch "" with create=true sends If-None-Match: *.
// It returns the new ETag if the server sent one.
func (c *client) put(ctx context.Context, u *url.URL, data, ifMatch string, create bool) (string, error) {
	hdr := map[string]string{"Content-Type": "text/calendar; charset=utf-8"}
	if ifMatch != "" {
		hdr["If-Match"] = ifMatch
	} else if create {
		hdr["If-None-Match"] = "*"
	}
	final, resp, _, err := c.do(ctx, http.MethodPut, u, hdr, []byte(data))
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return resp.Header.Get("ETag"), nil
	case http.StatusPreconditionFailed:
		if create && ifMatch == "" {
			return "", fmt.Errorf("calendar: an event already exists at %s", final.Redacted())
		}
		return "", ErrConflict
	}
	return "", &httpError{Method: "PUT", URL: final.Redacted(), Status: resp.StatusCode}
}

// del deletes a resource with an optional If-Match.
func (c *client) del(ctx context.Context, u *url.URL, ifMatch string) error {
	hdr := map[string]string{}
	if ifMatch != "" {
		hdr["If-Match"] = ifMatch
	}
	final, resp, _, err := c.do(ctx, http.MethodDelete, u, hdr, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted:
		return nil
	case http.StatusPreconditionFailed:
		return ErrConflict
	}
	return &httpError{Method: "DELETE", URL: final.Redacted(), Status: resp.StatusCode}
}
