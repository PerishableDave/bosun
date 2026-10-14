package oauth_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/oauth"
	"github.com/perishabledave/bosun/internal/oauth/oauthtest"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Record(_ context.Context, e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

type fixture struct {
	idp   *oauthtest.IdP
	v     *vault.Vault
	m     *oauth.Manager
	clock *clock
	audit *memAudit
	alice int64
	bob   int64
}

const redirect = "http://127.0.0.1:7777/oauth/callback"

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, n := range []string{"alice", "bob"} {
		u, err := st.CreateUser(ctx, n, "x", "member")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u.ID)
	}
	idp := oauthtest.New()
	t.Cleanup(idp.Close)
	v := vault.New(st, vault.StaticKeySource(bytes.Repeat([]byte{3}, vault.KeySize)))
	f := &fixture{idp: idp, v: v, clock: &clock{t: time.Now()}, audit: &memAudit{}, alice: ids[0], bob: ids[1]}
	f.m, err = oauth.New(oauth.Options{Vault: v, Clients: map[string]oauth.Client{"google": idp.Client("google")},
		Audit: f.audit, Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	f.m.Register(v)
	return f
}

// authorize plays the browser: follows the auth URL to the fake IdP, which
// redirects with code and state; returns that callback query.
func (f *fixture) authorize(t *testing.T, authURL string) url.Values {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: HTTP %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != redirect {
		t.Fatalf("redirected to %s", got)
	}
	return loc.Query()
}

// link runs a full flow for userID.
func (f *fixture) link(t *testing.T, userID int64, bundles ...string) *oauth.StartResult {
	t.Helper()
	ctx := context.Background()
	res, err := f.m.Start(ctx, userID, "google", bundles, redirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Callback(ctx, f.authorize(t, res.AuthURL)); err != nil {
		t.Fatal(err)
	}
	return res
}

func asUser(id int64) context.Context { return integration.WithUserID(context.Background(), id) }

func TestScopeBundles(t *testing.T) {
	p, _ := oauth.Builtin("google")
	got, err := p.ExpandScopes([]string{"email", "calendar", "email"})
	if err != nil {
		t.Fatal(err)
	}
	g := "https://www.googleapis.com/auth/"
	want := []string{"openid", "email", g + "gmail.readonly", g + "gmail.send", g + "gmail.modify", g + "calendar.events", g + "calendar.readonly"}
	if !slices.Equal(got, want) {
		t.Errorf("email,calendar = %v", got)
	}
	def, _ := p.ExpandScopes(nil)
	if !slices.Equal(def, want) {
		t.Errorf("default = %v", def)
	}
	got, _ = p.ExpandScopes([]string{"tasks", "drive", "https://example.com/x"})
	if !slices.Equal(got, []string{"openid", "email", g + "tasks", g + "drive.readonly", "https://example.com/x"}) {
		t.Errorf("tasks,drive = %v", got)
	}
	if _, err := p.ExpandScopes([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "calendar") {
		t.Errorf("unknown bundle err = %v", err)
	}
	ms, _ := oauth.Builtin("microsoft")
	got, _ = ms.ExpandScopes(nil)
	if !slices.Contains(got, "offline_access") || !slices.Contains(got, "https://graph.microsoft.com/Mail.Send") {
		t.Errorf("microsoft default = %v", got)
	}
	if !strings.Contains(ms.AuthURL, "/common/oauth2/v2.0/") {
		t.Errorf("microsoft auth url %s", ms.AuthURL)
	}
}

func TestLinkFlowAndCredentialContract(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.m.Start(ctx, f.alice, "google", []string{"email"}, redirect)
	if err != nil {
		t.Fatal(err)
	}
	au, _ := url.Parse(res.AuthURL)
	q := au.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") != redirect ||
		q.Get("access_type") != "offline" || !strings.Contains(q.Get("scope"), "gmail.modify") || q.Get("state") != res.State {
		t.Fatalf("auth url params %v", q)
	}
	if st, err := f.m.Status(f.alice, "google", res.State); err != nil || st.Status != oauth.StatusPending {
		t.Fatalf("status before callback = %+v, %v", st, err)
	}
	if _, err := f.m.Status(f.bob, "google", res.State); !errors.Is(err, oauth.ErrBadState) {
		t.Errorf("bob sees alice's state: %v", err)
	}

	cbq := f.authorize(t, res.AuthURL)
	cb, err := f.m.Callback(ctx, cbq)
	if err != nil {
		t.Fatal(err)
	}
	if cb.UserID != f.alice || cb.Account.Email != "alice@example.com" {
		t.Errorf("callback = %+v", cb)
	}
	if v := f.idp.Verifiers(); len(v) != 1 || len(v[0]) < 43 {
		t.Errorf("PKCE verifiers sent = %v", v)
	}
	st, err := f.m.Status(f.alice, "google", res.State)
	if err != nil || st.Status != oauth.StatusComplete || st.Account == nil || st.Account.Email != "alice@example.com" {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// Replay: the state is single use.
	if _, err := f.m.Callback(ctx, cbq); !errors.Is(err, oauth.ErrBadState) {
		t.Errorf("replayed callback err = %v", err)
	}

	// Stored under the pseudo-integration "oauth" for alice.
	raw, err := f.v.Get(ctx, &f.alice, "oauth", "oauth2:google")
	if err != nil || raw.Fields["email"] != "alice@example.com" || raw.Fields["refresh_token"] == "" || raw.Secret == "" {
		t.Fatalf("stored = %+v, %v", raw, err)
	}

	// Any integration's source resolves the kind, with exactly the contract.
	src := f.v.SourceFor("email", integration.ScopePerUser)
	c, err := src.Get(asUser(f.alice), "oauth2:google")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "oauth2:google" || c.Secret != raw.Secret || c.Expiry.IsZero() || len(c.Fields) != 3 ||
		c.Fields["email"] != "alice@example.com" || c.Fields["provider"] != "google" || !strings.Contains(c.Fields["scopes"], "gmail.send") {
		t.Errorf("credential = %+v", c)
	}
	if _, ok := c.Fields["refresh_token"]; ok {
		t.Error("refresh token handed to an integration")
	}
	if c2, err := f.v.SourceFor("calendar", integration.ScopePerUser).Get(asUser(f.alice), "oauth2:google"); err != nil || c2.Secret != c.Secret {
		t.Errorf("other integration: %+v, %v", c2, err)
	}

	accts, err := f.m.Accounts(ctx, f.alice)
	if err != nil || len(accts) != 1 || accts[0].Email != "alice@example.com" || accts[0].Provider != "google" || accts[0].LinkedAt.IsZero() {
		t.Errorf("accounts = %+v, %v", accts, err)
	}

	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	if len(f.audit.entries) != 1 || f.audit.entries[0].Kind != audit.KindConfig || f.audit.entries[0].UserID != f.alice ||
		!strings.Contains(string(f.audit.entries[0].Detail), "alice@example.com") ||
		strings.Contains(string(f.audit.entries[0].Detail), raw.Secret) || strings.Contains(string(f.audit.entries[0].Detail), raw.Fields["refresh_token"]) {
		t.Errorf("audit = %+v", f.audit.entries)
	}
}

func TestStateExpiryAndErrors(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.m.Start(ctx, f.alice, "google", nil, redirect)
	if err != nil {
		t.Fatal(err)
	}
	cbq := f.authorize(t, res.AuthURL)
	f.clock.Add(oauth.StateTTL + time.Second)
	if _, err := f.m.Callback(ctx, cbq); !errors.Is(err, oauth.ErrBadState) {
		t.Errorf("expired callback err = %v", err)
	}
	if st, _ := f.m.Status(f.alice, "google", res.State); st.Status != oauth.StatusExpired {
		t.Errorf("status = %+v", st)
	}
	if _, err := f.m.Callback(ctx, url.Values{"state": {"made-up"}, "code": {"x"}}); !errors.Is(err, oauth.ErrBadState) {
		t.Errorf("unknown state err = %v", err)
	}

	// The user denies consent.
	res, _ = f.m.Start(ctx, f.alice, "google", nil, redirect)
	if _, err := f.m.Callback(ctx, url.Values{"state": {res.State}, "error": {"access_denied"}}); err == nil {
		t.Error("denied callback succeeded")
	}
	if st, _ := f.m.Status(f.alice, "google", res.State); st.Status != oauth.StatusError || !strings.Contains(st.Error, "access_denied") {
		t.Errorf("status = %+v", st)
	}

	if _, err := f.m.Start(ctx, f.alice, "google", []string{"bogus"}, redirect); !errors.Is(err, oauth.ErrInvalid) {
		t.Errorf("bogus bundle err = %v", err)
	}
	if _, err := f.m.Start(ctx, f.alice, "microsoft", nil, redirect); !errors.Is(err, oauth.ErrNotConfigured) {
		t.Errorf("unconfigured err = %v", err)
	}
	if _, err := f.m.Start(ctx, f.alice, "acme", nil, redirect); !errors.Is(err, oauth.ErrUnknownProvider) {
		t.Errorf("unknown provider err = %v", err)
	}
}

func TestAutoRefresh(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.idp.SetExpiry(3600, 3600)
	f.idp.SetRotate(true)
	f.link(t, f.alice)
	before, _ := f.v.Get(ctx, &f.alice, "oauth", "oauth2:google")

	// Not near expiry: no refresh.
	if c, err := f.m.Get(ctx, f.alice, "google"); err != nil || c.Secret != before.Secret || f.idp.RefreshCount() != 0 {
		t.Fatalf("get = %+v, %v, refreshes %d", c, err, f.idp.RefreshCount())
	}

	// Within 2 minutes of expiry: many concurrent callers, one refresh.
	f.clock.Add(time.Hour - oauth.RefreshSkew + time.Second)
	f.idp.SetRefreshDelay(100 * time.Millisecond)
	src := f.v.SourceFor("email", integration.ScopePerUser)
	var wg sync.WaitGroup
	secrets := make([]string, 8)
	errs := make([]error, 8)
	for i := range secrets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := src.Get(asUser(f.alice), "oauth2:google")
			secrets[i], errs[i] = c.Secret, err
		}()
	}
	wg.Wait()
	for i := range secrets {
		if errs[i] != nil || secrets[i] == before.Secret || secrets[i] != secrets[0] {
			t.Fatalf("caller %d: %q, %v (all: %v)", i, secrets[i], errs[i], secrets)
		}
	}
	if n := f.idp.RefreshCount(); n != 1 {
		t.Errorf("refreshes = %d, want 1 (single flight)", n)
	}
	after, _ := f.v.Get(ctx, &f.alice, "oauth", "oauth2:google")
	if after.Secret != secrets[0] || !after.Expiry.After(before.Expiry) || after.Fields["refresh_token"] == before.Fields["refresh_token"] ||
		after.Fields["email"] != "alice@example.com" {
		t.Errorf("persisted after refresh = %+v (before %+v)", after, before)
	}

	// The rotated refresh token is the one used next time.
	f.idp.SetRefreshDelay(0)
	f.clock.Add(time.Hour)
	c, err := f.m.Get(ctx, f.alice, "google")
	if err != nil || c.Secret == after.Secret || f.idp.RefreshCount() != 2 {
		t.Errorf("second refresh = %+v, %v (count %d)", c, err, f.idp.RefreshCount())
	}
}

func TestRevokedRefreshToken(t *testing.T) {
	f := setup(t)
	f.idp.SetExpiry(60, 60) // already within the refresh window
	f.link(t, f.alice)
	f.idp.RevokeAll()
	_, err := f.v.SourceFor("email", integration.ScopePerUser).Get(asUser(f.alice), "oauth2:google")
	if !errors.Is(err, integration.ErrNoCredential) || !strings.Contains(err.Error(), "bosun connect google") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "rt-") || strings.Contains(err.Error(), "at-") {
		t.Errorf("error leaks a token: %v", err)
	}
}

func TestUserIsolation(t *testing.T) {
	f := setup(t)
	f.link(t, f.alice)
	src := f.v.SourceFor("email", integration.ScopePerUser)
	if _, err := src.Get(asUser(f.bob), "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) ||
		!strings.Contains(err.Error(), "bosun connect google") {
		t.Errorf("bob read: %v", err)
	}
	if _, err := src.Get(context.Background(), "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("no user: %v", err)
	}
	if _, err := f.v.SourceFor("mcp", integration.ScopeShared).Get(asUser(f.alice), "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("shared source: %v", err)
	}

	// Bob links his own account; each sees only theirs.
	f.idp.SetEmail("bob@example.com")
	f.link(t, f.bob)
	a, _ := src.Get(asUser(f.alice), "oauth2:google")
	b, _ := src.Get(asUser(f.bob), "oauth2:google")
	if a.Fields["email"] != "alice@example.com" || b.Fields["email"] != "bob@example.com" || a.Secret == b.Secret {
		t.Errorf("alice %+v bob %+v", a, b)
	}
}

func TestUnlink(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.link(t, f.alice)
	raw, _ := f.v.Get(ctx, &f.alice, "oauth", "oauth2:google")
	if _, err := f.m.Unlink(ctx, f.bob, "google"); !errors.Is(err, oauth.ErrNotLinked) {
		t.Errorf("bob unlink: %v", err)
	}
	a, err := f.m.Unlink(ctx, f.alice, "google")
	if err != nil || a.Email != "alice@example.com" {
		t.Fatalf("unlink = %+v, %v", a, err)
	}
	if r := f.idp.Revokes(); len(r) != 1 || r[0] != raw.Fields["refresh_token"] {
		t.Errorf("revoked %v", r)
	}
	if _, err := f.v.Get(ctx, &f.alice, "oauth", "oauth2:google"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("still stored: %v", err)
	}
	if _, err := f.m.Get(ctx, f.alice, "google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("get after unlink: %v", err)
	}
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	if n := len(f.audit.entries); n != 2 || f.audit.entries[1].Notes[0] != "oauth account unlinked" {
		t.Errorf("audit = %+v", f.audit.entries)
	}
}
