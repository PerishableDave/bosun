package calendar

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// fakeDAV is an in-memory CalDAV server: PROPFIND discovery (well-known
// redirect, principal, home set, calendars), REPORT calendar-query (with
// time-range or UID filtering) and calendar-multiget, and GET/PUT/DELETE
// with ETag preconditions.
type fakeDAV struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	user      string // Basic auth user ("" = no Basic)
	pass      string
	token     string // Bearer token ("" = no Bearer)
	principal string
	home      string
	cals      map[string]*fakeCal // path -> calendar
	etagN     int
	log       []string
	authSeen  []string

	// Knobs.
	principalHref string // override the current-user-principal href
	noInlineData  bool   // calendar-query omits calendar-data (forces multiget)
	beforePut     func(path string)
	noWellKnown   bool
}

type fakeCal struct {
	name, color string
	readOnly    bool
	objs        map[string]*fakeObj // name -> object
}

type fakeObj struct{ data, etag string }

func newFakeDAV(t *testing.T) *fakeDAV {
	f := &fakeDAV{t: t, cals: map[string]*fakeCal{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// basic configures Basic auth and standard paths for user.
func (f *fakeDAV) basic(user, pass string) *fakeDAV {
	f.user, f.pass = user, pass
	f.principal = "/principals/" + user + "/"
	f.home = "/calendars/" + user + "/"
	return f
}

// google configures Bearer auth and Google-style paths.
func (f *fakeDAV) google(email, token string) *fakeDAV {
	f.token = token
	f.principal = "/caldav/v2/" + email + "/user"
	f.home = "/caldav/v2/" + email + "/"
	f.noWellKnown = true
	return f
}

func (f *fakeDAV) addCal(id, name, color string, readOnly bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.home + id + "/"
	f.cals[p] = &fakeCal{name: name, color: color, readOnly: readOnly, objs: map[string]*fakeObj{}}
	return p
}

func (f *fakeDAV) nextETag() string {
	f.etagN++
	return fmt.Sprintf(`"e%d"`, f.etagN)
}

// put stores an object directly (test setup), wrapping a VEVENT body.
func (f *fakeDAV) put(calPath, name, vevents string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" + vevents + "END:VCALENDAR\r\n"
	f.cals[calPath].objs[name] = &fakeObj{data: data, etag: f.nextETag()}
}

// touch changes an object's ETag as if another client edited it.
func (f *fakeDAV) touch(calPath, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.cals[calPath].objs[name]
	o.etag = f.nextETag()
}

func (f *fakeDAV) object(calPath, name string) *fakeObj {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.cals[calPath].objs[name]; ok {
		c := *o
		return &c
	}
	return nil
}

func (f *fakeDAV) objects(calPath string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for n, o := range f.cals[calPath].objs {
		out[n] = o.data
	}
	return out
}

func (f *fakeDAV) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	f.authSeen = append(f.authSeen, h)
	if f.token != "" && h == "Bearer "+f.token {
		return true
	}
	if f.user != "" && h == "Basic "+base64.StdEncoding.EncodeToString([]byte(f.user+":"+f.pass)) {
		return true
	}
	return false
}

const msHead = `<?xml version="1.0" encoding="utf-8"?>` +
	`<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/">`

func okResp(href, props string) string {
	return `<d:response><d:href>` + html.EscapeString(href) + `</d:href><d:propstat><d:prop>` + props +
		`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
}

func writeMS(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, msHead+body+`</d:multistatus>`)
}

var (
	reHref  = regexp.MustCompile(`<d:href>(.*?)</d:href>`)
	reRange = regexp.MustCompile(`time-range start="(\w+)" end="(\w+)"`)
	reUID   = regexp.MustCompile(`<c:text-match[^>]*>(.*?)</c:text-match>`)
)

func (f *fakeDAV) calProps(c *fakeCal) string {
	p := `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>` + html.EscapeString(c.name) + `</d:displayname>` +
		`<c:supported-calendar-component-set><c:comp name="VEVENT"/><c:comp name="VTODO"/></c:supported-calendar-component-set>`
	if c.color != "" {
		p += `<a:calendar-color>` + c.color + `</a:calendar-color>`
	}
	if c.readOnly {
		p += `<d:current-user-privilege-set><d:privilege><d:read/></d:privilege></d:current-user-privilege-set>`
	} else {
		p += `<d:current-user-privilege-set><d:privilege><d:read/></d:privilege><d:privilege><d:write/></d:privilege></d:current-user-privilege-set>`
	}
	return p
}

func (f *fakeDAV) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	if !f.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch r.Method {
	case "PROPFIND":
		switch {
		case path == "/.well-known/caldav" && !f.noWellKnown:
			w.Header().Set("Location", "/")
			w.WriteHeader(http.StatusMovedPermanently)
		case path == "/":
			ph := f.principal
			if f.principalHref != "" {
				ph = f.principalHref
			}
			writeMS(w, okResp("/", `<d:current-user-principal><d:href>`+ph+`</d:href></d:current-user-principal><d:resourcetype><d:collection/></d:resourcetype>`))
		case path == f.principal:
			writeMS(w, okResp(path, `<c:calendar-home-set><d:href>`+f.home+`</d:href></c:calendar-home-set>`))
		case path == f.home:
			out := okResp(path, `<d:resourcetype><d:collection/></d:resourcetype>`)
			if r.Header.Get("Depth") == "1" {
				keys := make([]string, 0, len(f.cals))
				for k := range f.cals {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					out += okResp(k, f.calProps(f.cals[k]))
				}
				// a non-calendar child, which must be ignored
				out += okResp(f.home+"inbox/", `<d:resourcetype><d:collection/><c:schedule-inbox/></d:resourcetype>`)
			}
			writeMS(w, out)
		case f.cals[path] != nil:
			writeMS(w, okResp(path, f.calProps(f.cals[path])))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case "REPORT":
		cal := f.cals[path]
		if cal == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b := string(body)
		var out string
		names := make([]string, 0, len(cal.objs))
		for n := range cal.objs {
			names = append(names, n)
		}
		sort.Strings(names)
		if strings.Contains(b, "calendar-multiget") {
			for _, m := range reHref.FindAllStringSubmatch(b, -1) {
				href := html.UnescapeString(m[1])
				name := strings.TrimPrefix(href, path)
				if o, ok := cal.objs[name]; ok {
					out += okResp(href, `<d:getetag>`+html.EscapeString(o.etag)+`</d:getetag><c:calendar-data>`+html.EscapeString(o.data)+`</c:calendar-data>`)
				}
			}
			writeMS(w, out)
			return
		}
		var from, to time.Time
		uid := ""
		if m := reUID.FindStringSubmatch(b); m != nil {
			uid = html.UnescapeString(m[1])
		} else if m := reRange.FindStringSubmatch(b); m != nil {
			from, _ = time.Parse("20060102T150405Z", m[1])
			to, _ = time.Parse("20060102T150405Z", m[2])
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, n := range names {
			o := cal.objs[n]
			_, evs, err := parseEvents(o.data, time.UTC, time.UTC)
			if err != nil {
				f.t.Errorf("fake server: stored object %s does not parse: %v", n, err)
				continue
			}
			match := false
			for _, e := range evs {
				if uid != "" && e.UID == uid {
					match = true
				}
			}
			if uid == "" {
				// Coarse but correct for tests: a day of slack for floating
				// times, as real servers do.
				match = len(expand(evs, from.Add(-14*time.Hour), to.Add(14*time.Hour), newTZCtx(nil, time.UTC, time.UTC), 1)) > 0
			}
			if !match {
				continue
			}
			props := `<d:getetag>` + html.EscapeString(o.etag) + `</d:getetag>`
			if !f.noInlineData {
				props += `<c:calendar-data>` + html.EscapeString(o.data) + `</c:calendar-data>`
			}
			out += okResp(path+n, props)
		}
		writeMS(w, out)
	case http.MethodGet, http.MethodPut, http.MethodDelete:
		i := strings.LastIndex(path, "/")
		cal := f.cals[path[:i+1]]
		name := path[i+1:]
		if cal == nil || name == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		o := cal.objs[name]
		switch r.Method {
		case http.MethodGet:
			if o == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", o.etag)
			w.Header().Set("Content-Type", "text/calendar")
			io.WriteString(w, o.data)
		case http.MethodPut:
			if f.beforePut != nil {
				hook := f.beforePut
				f.beforePut = nil
				f.mu.Unlock()
				hook(path)
				f.mu.Lock()
				o = cal.objs[name]
			}
			if cal.readOnly {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if im := r.Header.Get("If-Match"); im != "" && (o == nil || o.etag != im) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if r.Header.Get("If-None-Match") == "*" && o != nil {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if _, err := ParseICS(string(body)); err != nil {
				f.t.Errorf("fake server: PUT body does not parse: %v\n%s", err, body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			status := http.StatusNoContent
			if o == nil {
				status = http.StatusCreated
			}
			n := &fakeObj{data: string(body), etag: f.nextETag()}
			cal.objs[name] = n
			w.Header().Set("ETag", n.etag)
			w.WriteHeader(status)
		case http.MethodDelete:
			if o == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if im := r.Header.Get("If-Match"); im != "" && o.etag != im {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			delete(cal.objs, name)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// fakeCreds is a per-user CredentialSource keyed by the user in ctx, like
// vault.SourceFor with ScopePerUser.
type fakeCreds struct {
	mu    sync.Mutex
	creds map[int64]map[string]integration.Credential
	gets  []string
}

func newFakeCreds() *fakeCreds {
	return &fakeCreds{creds: map[int64]map[string]integration.Credential{}}
}

func (f *fakeCreds) set(user int64, c integration.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds[user] == nil {
		f.creds[user] = map[string]integration.Credential{}
	}
	f.creds[user][c.Kind] = c
}

func (f *fakeCreds) Get(ctx context.Context, kind string) (integration.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := integration.UserIDFromContext(ctx)
	f.gets = append(f.gets, fmt.Sprintf("%d:%s", uid, kind))
	if !ok {
		return integration.Credential{}, fmt.Errorf("no user: %w", integration.ErrNoCredential)
	}
	c, ok := f.creds[uid][kind]
	if !ok {
		return integration.Credential{}, fmt.Errorf("calendar/%s: %w", kind, integration.ErrNoCredential)
	}
	return c, nil
}
