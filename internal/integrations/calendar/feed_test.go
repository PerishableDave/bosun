package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// feedServer serves iCalendar feeds over TLS and counts fetches.
type feedServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	feeds map[string]string // path -> body
	hits  map[string]int
}

func newFeedServer(t *testing.T) *feedServer {
	f := &feedServer{feeds: map[string]string{}, hits: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		body, ok := f.feeds[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *feedServer) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds[path] = body
}

func (f *feedServer) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *feedServer) url(path string) string { return f.srv.URL + path }

// webcal returns the feed URL with the webcal:// scheme.
func (f *feedServer) webcal(path string) string {
	return "webcal://" + strings.TrimPrefix(f.srv.URL, "https://") + path
}

// newFeedCal returns a calendar integration that trusts the feed server's
// certificate and may fetch from loopback (the SSRF guard is tested
// separately).
func newFeedCal(t *testing.T, creds integration.CredentialSource, fs *feedServer) *Integration {
	t.Helper()
	c := newTestCal(t, creds, "")
	c.feedAllowPrivate = true
	c.feedClient = newFeedClient(true, fs.srv.Client().Transport.(*http.Transport))
	return c
}

func vcal(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
}

const familyFeed = "BEGIN:VEVENT\r\nUID:soccer\r\nDTSTART;TZID=America/New_York:20260915T170000\r\nDTEND;TZID=America/New_York:20260915T180000\r\n" +
	"RRULE:FREQ=WEEKLY;BYDAY=TU\r\nSUMMARY:Soccer practice\r\nLOCATION:Field 3\r\nEND:VEVENT\r\n" +
	// This week's practice moved to 18:00.
	"BEGIN:VEVENT\r\nUID:soccer\r\nRECURRENCE-ID;TZID=America/New_York:20261013T170000\r\n" +
	"DTSTART;TZID=America/New_York:20261013T180000\r\nDTEND;TZID=America/New_York:20261013T190000\r\nSUMMARY:Soccer practice (late)\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:dentist\r\nDTSTART:20261013T190000Z\r\nDTEND:20261013T193000Z\r\nSUMMARY:Dentist\r\n" +
	"DESCRIPTION:Bring the forms\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:bad\r\nSUMMARY:no start\r\nEND:VEVENT\r\n"

const workFeed = "BEGIN:VEVENT\r\nUID:review\r\nDTSTART:20261013T150000Z\r\nDTEND:20261013T160000Z\r\nSUMMARY:Design review\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:offsite\r\nDTSTART;VALUE=DATE:20261013\r\nDTEND;VALUE=DATE:20261015\r\nSUMMARY:Offsite\r\nTRANSP:TRANSPARENT\r\nEND:VEVENT\r\n"

func icsCred(kind, url string) integration.Credential {
	return integration.Credential{Kind: kind, Secret: url}
}

func TestICSFeedsMergedReadOnly(t *testing.T) {
	fs := newFeedServer(t)
	fs.set("/family/private-abc123/basic.ics", vcal(familyFeed))
	fs.set("/work.ics", vcal(workFeed))
	creds := newFakeCreds()
	creds.set(1, icsCred("ics:family", fs.webcal("/family/private-abc123/basic.ics"))) // webcal:// is rewritten to https
	creds.set(1, icsCred("ics:work", fs.url("/work.ics")))
	c := newFeedCal(t, creds, fs)
	ctx := userCtx(1)

	res := mustCall(t, c, ctx, "calendar.calendars", map[string]any{})
	if !strings.Contains(res.Content, "2 calendar(s)") || !strings.Contains(res.Content, "- family (read-only)") ||
		!strings.Contains(res.Content, "- work (read-only)") || strings.Contains(res.Content, "abc123") {
		t.Errorf("calendars:\n%s", res.Content)
	}

	res = mustCall(t, c, ctx, "calendar.events", map[string]any{"from": "today"})
	for _, want := range []string{
		"18:00–19:00  Soccer practice (late) [family] (recurring)", // the override replaces the 17:00 instance
		"15:00–15:30  Dentist [family]", "11:00–12:00  Design review [work]", "all day  Offsite [work]",
		"(1 calendar object(s) could not be parsed and were skipped)",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("events missing %q:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "17:00–18:00  Soccer") {
		t.Errorf("overridden instance still listed:\n%s", res.Content)
	}
	evs := res.Data.([]EventSummary)
	if len(evs) != 4 {
		t.Errorf("got %d events: %+v", len(evs), evs)
	}

	// Recurrence expansion over a longer window.
	res = mustCall(t, c, ctx, "calendar.events", map[string]any{"from": "2026-10-01", "to": "2026-11-01", "calendar": "family"})
	if n := strings.Count(res.Content, "Soccer practice"); n != 4 { // Oct 6, 13 (moved), 20, 27
		t.Errorf("soccer instances = %d:\n%s", n, res.Content)
	}

	res = mustCall(t, c, ctx, "calendar.get", map[string]any{"uid": "dentist"})
	if !strings.Contains(res.Content, "Bring the forms") || !strings.Contains(res.Content, "Calendar: family") || strings.Contains(res.Content, "abc123") {
		t.Errorf("get:\n%s", res.Content)
	}
	if d := res.Data.(EventDetail); d.Href != "ics:family" {
		t.Errorf("href = %q", d.Href)
	}

	res = mustCall(t, c, ctx, "calendar.freebusy", map[string]any{"from": "today"})
	if !strings.Contains(res.Content, "3 interval(s)") { // review, dentist, late soccer; the offsite is transparent
		t.Errorf("freebusy:\n%s", res.Content)
	}

	// Writes are refused before anything is fetched.
	before := fs.count("/work.ics")
	for tool, args := range map[string]map[string]any{
		"calendar.create": {"summary": "x", "start": "tomorrow"},
		"calendar.update": {"uid": "dentist", "summary": "y"},
		"calendar.delete": {"uid": "dentist"},
	} {
		_, err := call(t, c, ctx, tool, args)
		if err == nil || !strings.Contains(err.Error(), "read-only (ICS)") || !strings.Contains(err.Error(), "CalDAV") {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if fs.count("/work.ics") != before {
		t.Error("write tools fetched the feed")
	}
}

func TestICSFeedCache(t *testing.T) {
	fs := newFeedServer(t)
	fs.set("/f.ics", vcal(workFeed))
	creds := newFakeCreds()
	creds.set(1, icsCred(KindICS, fs.url("/f.ics")))
	creds.set(2, icsCred(KindICS, fs.url("/f.ics")))
	c := newFeedCal(t, creds, fs)
	now := fixedNow
	c.now = func() time.Time { return now }

	res := mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "today"})
	if !strings.Contains(res.Content, "Design review") || strings.Contains(res.Content, "[calendar]") {
		t.Errorf("events:\n%s", res.Content)
	}
	mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "today", "timezone": "Europe/Madrid"})
	mustCall(t, c, userCtx(1), "calendar.calendars", map[string]any{})
	if n := fs.count("/f.ics"); n != 1 {
		t.Errorf("fetches = %d, want 1 (cached)", n)
	}
	// The cache is per user.
	mustCall(t, c, userCtx(2), "calendar.events", map[string]any{"from": "today"})
	if n := fs.count("/f.ics"); n != 2 {
		t.Errorf("fetches = %d, want 2", n)
	}
	// Expired after five minutes.
	fs.set("/f.ics", vcal("BEGIN:VEVENT\r\nUID:n\r\nDTSTART:20261013T200000Z\r\nSUMMARY:New thing\r\nEND:VEVENT\r\n"))
	now = now.Add(4 * time.Minute)
	res = mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "today"})
	if strings.Contains(res.Content, "New thing") {
		t.Error("cache expired early")
	}
	now = now.Add(2 * time.Minute)
	res = mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "today"})
	if !strings.Contains(res.Content, "New thing") || fs.count("/f.ics") != 3 {
		t.Errorf("after expiry (%d fetches):\n%s", fs.count("/f.ics"), res.Content)
	}
}

func TestICSFeedErrors(t *testing.T) {
	fs := newFeedServer(t)
	fs.set("/ok.ics", vcal(workFeed))
	fs.set("/html", "<html><body>Sign in</body></html>")
	fs.set("/big.ics", "BEGIN:VCALENDAR\r\n"+strings.Repeat("X-PAD:"+strings.Repeat("a", 1000)+"\r\n", 5300)+"END:VCALENDAR\r\n")
	creds := newFakeCreds()
	c := newFeedCal(t, creds, fs)
	ctx := userCtx(1)
	test := func(kind, u string) error { return c.TestCredential(ctx, icsCred(kind, u)) }

	if err := test("ics:work", fs.url("/ok.ics")); err != nil {
		t.Fatalf("ok feed: %v", err)
	}
	if err := test("ics", fs.webcal("/ok.ics")); err != nil {
		t.Fatalf("webcal feed: %v", err)
	}
	err := test("ics:x", fs.url("/secret-token-404.ics"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("404: %v", err)
	}
	if err := test("ics", fs.url("/html")); err == nil || !strings.Contains(err.Error(), "did not return an iCalendar feed") {
		t.Errorf("html: %v", err)
	}
	if err := test("ics", fs.url("/big.ics")); err == nil || !strings.Contains(err.Error(), "larger than 5 MB") {
		t.Errorf("size cap: %v", err)
	}
	if err := test("ics", "http://example.com/a.ics"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("plain http: %v", err)
	}

	// Without the test override, private and loopback addresses are refused
	// before any request is made.
	guarded := newTestCal(t, creds, "")
	for _, u := range []string{fs.url("/ok.ics"), "https://10.0.0.1/a.ics", "https://localhost:1/a.ics"} {
		err := guarded.TestCredential(ctx, icsCred("ics", u))
		if err == nil || !strings.Contains(err.Error(), "not publicly routable") {
			t.Errorf("SSRF %s: %v", u, err)
		}
	}
	// Even when the early check is bypassed, the dialer refuses.
	cl := newFeedClient(false, fs.srv.Client().Transport.(*http.Transport))
	if _, err := cl.Get(fs.url("/ok.ics")); err == nil || !strings.Contains(err.Error(), "not publicly routable") {
		t.Errorf("dialer guard: %v", err)
	}
	if n := fs.count("/ok.ics"); n != 2 {
		t.Errorf("guarded fetches reached the server: %d", n-2)
	}

	// One broken feed is a warning when another works; all broken is an error.
	creds.set(1, icsCred("ics:good", fs.url("/ok.ics")))
	creds.set(1, icsCred("ics:gone", fs.url("/gone.ics")))
	res := mustCall(t, c, ctx, "calendar.events", map[string]any{"from": "today"})
	if !strings.Contains(res.Content, "Design review") || !strings.Contains(res.Content, `warning: calendar: feed "gone"`) {
		t.Errorf("partial:\n%s", res.Content)
	}
	creds.set(2, icsCred("ics:gone", fs.url("/gone.ics")))
	if _, err := call(t, c, userCtx(2), "calendar.events", map[string]any{}); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("all broken: %v", err)
	}
	// A malformed stored URL is reported without the URL.
	creds.set(3, icsCred("ics", "ftp://example.com/secret"))
	if _, err := call(t, c, userCtx(3), "calendar.events", map[string]any{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("bad stored url: %v", err)
	}
}

func TestICSPrecedence(t *testing.T) {
	fs := newFeedServer(t)
	fs.set("/f.ics", vcal(workFeed))
	fb := newFakeDAV(t).basic("alice", "pw")
	seedStandard(fb)
	creds := newFakeCreds()
	creds.set(1, passwordCred(fb, "alice", "pw"))
	creds.set(1, icsCred("ics:work", fs.url("/f.ics")))
	c := newFeedCal(t, creds, fs)
	res := mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "today"})
	if !strings.Contains(res.Content, "Standup") || strings.Contains(res.Content, "Design review") || fs.count("/f.ics") != 0 {
		t.Errorf("CalDAV must win over ICS:\n%s", res.Content)
	}
}

func TestCalendarTestCredentialCalDAV(t *testing.T) {
	fb := newFakeDAV(t).basic("alice", "pw")
	seedStandard(fb)
	c := newTestCal(t, newFakeCreds(), "")
	ctx := context.Background()
	if err := c.TestCredential(ctx, passwordCred(fb, "alice", "pw")); err != nil {
		t.Fatalf("good: %v", err)
	}
	err := c.TestCredential(ctx, passwordCred(fb, "alice", "wrong"))
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad password: %v", err)
	}
	if err := c.TestCredential(ctx, integration.Credential{Kind: KindGoogle, Secret: "x"}); err == nil {
		t.Error("google kind accepted")
	}
}

// bigFeed builds n events: a third weekly-recurring since 2024, the rest
// single events spread over two years.
func bigFeed(n int) string {
	var sb strings.Builder
	sb.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	base := time.Date(2025, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "BEGIN:VEVENT\r\nUID:e%d@test\r\nDTSTAMP:20250101T000000Z\r\n", i)
		if i%3 == 0 {
			fmt.Fprintf(&sb, "DTSTART;TZID=America/New_York:20240101T%02d0000\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;BYDAY=%s\r\n",
				8+i%10, []string{"MO", "TU", "WE", "TH", "FR"}[i%5])
		} else {
			st := base.Add(time.Duration(i) * 5 * time.Hour)
			fmt.Fprintf(&sb, "DTSTART:%s\r\nDTEND:%s\r\n", utcStamp(st), utcStamp(st.Add(time.Hour)))
		}
		fmt.Fprintf(&sb, "SUMMARY:Event number %d with a reasonably long title\r\nDESCRIPTION:Some description text that is\r\n  folded across lines %d\r\nLOCATION:Room %d\r\nEND:VEVENT\r\n", i, i, i)
	}
	sb.WriteString("END:VCALENDAR\r\n")
	return sb.String()
}

func TestICSLargeFeedFast(t *testing.T) {
	fs := newFeedServer(t)
	fs.set("/big.ics", bigFeed(3000))
	creds := newFakeCreds()
	creds.set(1, icsCred(KindICS, fs.url("/big.ics")))
	c := newFeedCal(t, creds, fs)
	start := time.Now()
	res := mustCall(t, c, userCtx(1), "calendar.events", map[string]any{"from": "this week"})
	elapsed := time.Since(start)
	if !strings.Contains(res.Content, "Event number") {
		t.Fatalf("events:\n%.500s", res.Content)
	}
	limit := time.Second
	if raceEnabled {
		limit = 10 * time.Second
	}
	t.Logf("3000-event feed: fetch+parse+expand in %v", elapsed)
	if elapsed > limit {
		t.Errorf("took %v, want < %v", elapsed, limit)
	}
}
