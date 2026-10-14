package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/integrations/calendar"
	"github.com/perishabledave/bosun/internal/store"
)

// testerInteg is an email-like integration whose TestCredential accepts
// password "right" and echoes the secret back on failure (to check that the
// handler scrubs it).
type testerInteg struct {
	stubInteg
	mu    sync.Mutex
	users []int64
}

func (t *testerInteg) TestCredential(ctx context.Context, c integration.Credential) error {
	uid, _ := integration.UserIDFromContext(ctx)
	t.mu.Lock()
	t.users = append(t.users, uid)
	t.mu.Unlock()
	if c.Secret == "right" && c.Fields["host"] == "imap.example.test" {
		return nil
	}
	return fmt.Errorf("wrong password: imap.example.test rejected %q for %s", c.Secret, c.Fields["user"])
}

// davStub is a CalDAV server whose root is a calendar collection, with
// Basic auth alice:pw.
func davStub(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != "PROPFIND" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<d:response><d:href>/cal/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/><c:calendar/></d:resourcetype>`+
			`<d:displayname>Home</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCredentialTestEndpoint(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(st, auth.Options{Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	login := func(name string) (string, int64) {
		u, err := a.CreateUser(ctx, name, "longenough", auth.RoleMember)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.LoginWithLabel(ctx, name, "longenough", "test")
		if err != nil {
			t.Fatal(err)
		}
		return s.Token, u.ID
	}
	alice, aliceID := login("alice")
	bob, _ := login("bob")

	mail := &testerInteg{stubInteg: stubInteg{integration.Manifest{Name: "email", Scope: integration.ScopePerUser, CredentialKinds: []string{"password", "oauth2:google"}}}}
	cal := calendar.New()
	if err := cal.Init(ctx, integration.Config{Name: "calendar"}, nilSource{}); err != nil {
		t.Fatal(err)
	}
	integs := []integration.Integration{mail, cal,
		stubInteg{integration.Manifest{Name: "notes", Scope: integration.ScopePerUser, CredentialKinds: []string{"apikey"}}}}
	var logs bytes.Buffer
	s, err := New(Options{Auth: a, Bus: events.NewBus(), Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Integrations: func() []integration.Integration { return integs }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{srv: srv, auth: a}

	post := func(tok, name string, body any) (int, TestCredentialResponse, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp := e.do(t, "POST", "/v1/integrations/"+name+"/test", tok, string(b))
		raw, _ := io.ReadAll(resp.Body)
		var out TestCredentialResponse
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out, string(raw)
	}
	fields := map[string]string{"host": "imap.example.test", "user": "alice@example.test"}

	// OK, then a wrong password (HTTP 200, ok=false, secret scrubbed).
	code, out, raw := post(alice, "email", TestCredentialRequest{Kind: "password", Secret: "right", Fields: fields})
	if code != 200 || !out.OK || out.Error != "" {
		t.Fatalf("ok: %d %s", code, raw)
	}
	code, out, raw = post(alice, "email", TestCredentialRequest{Kind: "password", Secret: "hunter2-secret", Fields: fields})
	if code != 200 || out.OK || !strings.Contains(out.Error, "wrong password") || strings.Contains(raw, "hunter2-secret") ||
		!strings.Contains(out.Error, "[redacted]") {
		t.Fatalf("bad password: %d %s", code, raw)
	}
	if mail.users[0] != aliceID {
		t.Errorf("tester ran as user %d", mail.users[0])
	}

	// Real calendar integration: CalDAV ok / 401, and an ICS feed on a
	// private address refused by the SSRF guard.
	dav := davStub(t)
	code, out, raw = post(alice, "calendar", TestCredentialRequest{Kind: "password", Secret: "pw", Fields: map[string]string{"url": dav.URL, "user": "alice"}})
	if code != 200 || !out.OK {
		t.Fatalf("caldav ok: %d %s", code, raw)
	}
	code, out, raw = post(alice, "calendar", TestCredentialRequest{Kind: "password", Secret: "nope", Fields: map[string]string{"url": dav.URL, "user": "alice"}})
	if code != 200 || out.OK || !strings.Contains(out.Error, "401") {
		t.Fatalf("caldav 401: %d %s", code, raw)
	}

	// The fifth test this minute is allowed, the sixth is rate limited, and
	// other users have their own budget.
	code, out, raw = post(alice, "calendar", TestCredentialRequest{Kind: "ics:family", Secret: "https://127.0.0.1:1/private.ics"})
	if code != 200 || out.OK || !strings.Contains(out.Error, "not publicly routable") {
		t.Fatalf("ics ssrf: %d %s", code, raw)
	}
	resp := e.do(t, "POST", "/v1/integrations/email/test", alice, `{"kind":"password","secret":"right","fields":{"host":"imap.example.test","user":"a@b.c"}}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", resp.StatusCode)
	}
	if code, out, _ := post(bob, "email", TestCredentialRequest{Kind: "password", Secret: "right", Fields: fields}); code != 200 || !out.OK {
		t.Errorf("bob limited by alice's budget: %d", code)
	}

	// Request errors (checked before the rate limit).
	for _, c := range []struct {
		tok, name, body string
		want            int
	}{
		{bob, "nope", `{"kind":"password","secret":"x"}`, 404},
		{bob, "notes", `{"kind":"apikey","secret":"x"}`, 400},  // cannot test
		{bob, "email", `{"kind":"apikey","secret":"x"}`, 400},  // undeclared kind
		{bob, "email", `{"kind":"password","secret":""}`, 400}, // no secret
		{bob, "email", `{"kind":"password","secret":"x","bogus":1}`, 400},
		{"", "email", `{"kind":"password","secret":"x"}`, 401},
	} {
		if resp := e.do(t, "POST", "/v1/integrations/"+c.name+"/test", c.tok, c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d", c.name, c.body, resp.StatusCode, c.want)
		}
	}
	if strings.Contains(logs.String(), "hunter2") || strings.Contains(logs.String(), "right") || !strings.Contains(logs.String(), "credential test") {
		t.Errorf("logs:\n%s", logs.String())
	}
}

type nilSource struct{}

func (nilSource) Get(context.Context, string) (integration.Credential, error) {
	return integration.Credential{}, integration.ErrNoCredential
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	if ok, _ := l.allow(1); !ok {
		t.Fatal("1st")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.allow(1); !ok {
		t.Fatal("2nd")
	}
	if ok, wait := l.allow(1); ok || wait != 50*time.Second {
		t.Fatalf("3rd: %v %v", ok, wait)
	}
	if ok, _ := l.allow(2); !ok {
		t.Fatal("other key")
	}
	now = now.Add(51 * time.Second)
	if ok, _ := l.allow(1); !ok {
		t.Fatal("after window")
	}
}
