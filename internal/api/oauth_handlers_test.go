package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/oauth"
	"github.com/dperedo/bosun/internal/oauth/oauthtest"
	"github.com/dperedo/bosun/internal/store"
	"github.com/dperedo/bosun/internal/vault"
)

func TestOAuthEndpointsRoundTrip(t *testing.T) {
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
	tok := func(name string) (string, int64) {
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
	alice, aliceID := tok("alice")
	bob, _ := tok("bob")

	idp := oauthtest.New()
	defer idp.Close()
	v := vault.New(st, vault.StaticKeySource(bytes.Repeat([]byte{9}, vault.KeySize)))
	am := &memAudit{}
	m, err := oauth.New(oauth.Options{Vault: v, Clients: map[string]oauth.Client{"google": idp.Client("google")}, Audit: am})
	if err != nil {
		t.Fatal(err)
	}
	m.Register(v)
	webUI := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	s, err := New(Options{Auth: a, Bus: events.NewBus(), Version: "test", Credentials: v, OAuth: m, WebUI: webUI})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e := &env{srv: srv, auth: a}

	// Unauthenticated start is refused.
	if c := e.do(t, "POST", "/v1/oauth/google/start", "", `{}`).StatusCode; c != 401 {
		t.Errorf("unauthenticated start = %d", c)
	}
	if c := e.do(t, "POST", "/v1/oauth/google/start", alice, `{"scopes":["nope"]}`).StatusCode; c != 400 {
		t.Errorf("bad bundle = %d", c)
	}
	if c := e.do(t, "POST", "/v1/oauth/microsoft/start", alice, `{}`).StatusCode; c != 400 {
		t.Errorf("unconfigured provider = %d", c)
	}

	resp := e.do(t, "POST", "/v1/oauth/google/start", alice, `{"scopes":["email","calendar"]}`)
	var start oauth.StartResult
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&start) != nil {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	if start.RedirectURL != srv.URL+"/oauth/callback" || start.State == "" || !strings.HasPrefix(start.AuthURL, idp.Server.URL) {
		t.Fatalf("start = %+v", start)
	}
	statusPath := "/v1/oauth/google/status?state=" + url.QueryEscape(start.State)
	var fs oauth.FlowStatus
	json.NewDecoder(e.do(t, "GET", statusPath, alice, "").Body).Decode(&fs)
	if fs.Status != oauth.StatusPending {
		t.Errorf("status = %+v", fs)
	}
	if c := e.do(t, "GET", statusPath, bob, "").StatusCode; c != 404 {
		t.Errorf("bob polling alice's state = %d", c)
	}

	// The browser: IdP authorize → 302 → daemon /oauth/callback.
	var callbackURL string
	browser := &http.Client{CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		callbackURL = r.URL.String()
		return nil
	}}
	page, err := browser.Get(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || !strings.Contains(string(body), "You can close this tab") || !strings.Contains(string(body), "alice@example.com") ||
		!strings.HasPrefix(page.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("callback page = %d %s", page.StatusCode, body)
	}

	fs = oauth.FlowStatus{}
	json.NewDecoder(e.do(t, "GET", statusPath, alice, "").Body).Decode(&fs)
	if fs.Status != oauth.StatusComplete || fs.Account == nil || fs.Account.Email != "alice@example.com" {
		t.Errorf("status = %+v", fs)
	}

	// Replaying the callback fails.
	replay, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(replay.Body)
	replay.Body.Close()
	if replay.StatusCode != 400 || !strings.Contains(string(rb), "already used") {
		t.Errorf("replay = %d %s", replay.StatusCode, rb)
	}

	// Tokens are usable through any integration's source...
	c, err := v.SourceFor("email", integration.ScopePerUser).Get(integration.WithUserID(ctx, aliceID), "oauth2:google")
	if err != nil || c.Secret == "" || c.Fields["email"] != "alice@example.com" {
		t.Fatalf("resolved = %+v, %v", c, err)
	}
	// ...and never appear in listings.
	resp = e.do(t, "GET", "/v1/oauth", alice, "")
	lb, _ := io.ReadAll(resp.Body)
	var list OAuthListResponse
	json.Unmarshal(lb, &list)
	if resp.StatusCode != 200 || len(list.Accounts) != 1 || list.Accounts[0].Email != "alice@example.com" ||
		strings.Contains(string(lb), c.Secret) || strings.Contains(string(lb), "rt-") || len(list.Providers) < 2 {
		t.Errorf("list = %d %s", resp.StatusCode, lb)
	}
	resp = e.do(t, "GET", "/v1/oauth", bob, "")
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Accounts) != 0 {
		t.Errorf("bob sees %+v", list.Accounts)
	}

	if c := e.do(t, "DELETE", "/v1/oauth/google", bob, "").StatusCode; c != 404 {
		t.Errorf("bob delete = %d", c)
	}
	if c := e.do(t, "DELETE", "/v1/oauth/google", alice, "").StatusCode; c != 204 {
		t.Errorf("alice delete = %d", c)
	}
	if len(idp.Revokes()) != 1 {
		t.Errorf("revokes = %v", idp.Revokes())
	}
	if c := e.do(t, "DELETE", "/v1/oauth/google", alice, "").StatusCode; c != 404 {
		t.Errorf("second delete = %d", c)
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	if len(am.entries) != 2 {
		t.Errorf("audit entries = %d", len(am.entries))
	}
	for _, en := range am.entries {
		if strings.Contains(string(en.Detail), c.Secret) {
			t.Errorf("audit leaks token: %s", en.Detail)
		}
	}
}

func TestOAuthEndpointsWithoutManager(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	if c := e.do(t, "GET", "/v1/oauth", tok, "").StatusCode; c != 501 {
		t.Errorf("list = %d", c)
	}
	if c := e.do(t, "GET", "/oauth/callback?state=x&code=y", "", "").StatusCode; c != 501 {
		t.Errorf("callback = %d", c)
	}
}
