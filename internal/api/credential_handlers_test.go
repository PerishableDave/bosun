package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

type stubInteg struct{ m integration.Manifest }

func (s stubInteg) Manifest() integration.Manifest { return s.m }
func (s stubInteg) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (s stubInteg) Tools() []integration.Tool { return nil }
func (s stubInteg) Close() error              { return nil }

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

type credEnv struct {
	*env
	v                 *vault.Vault
	audit             *memAudit
	admin, alice, bob string
	adminID, aliceID  int64
}

func setupCredentials(t *testing.T) *credEnv {
	t.Helper()
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
	ce := &credEnv{audit: &memAudit{}}
	tok := func(name string, role auth.Role) (string, int64) {
		u, err := a.CreateUser(ctx, name, "longenough", role)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.LoginWithLabel(ctx, name, "longenough", "test")
		if err != nil {
			t.Fatal(err)
		}
		return s.Token, u.ID
	}
	ce.admin, ce.adminID = tok("dave", auth.RoleAdmin)
	ce.alice, ce.aliceID = tok("alice", auth.RoleMember)
	ce.bob, _ = tok("bob", auth.RoleMember)
	ce.v = vault.New(st, vault.StaticKeySource(bytes.Repeat([]byte{7}, vault.KeySize)))
	integs := []integration.Integration{
		stubInteg{integration.Manifest{Name: "email", Scope: integration.ScopePerUser, CredentialKinds: []string{"apikey", "oauth2:google"}}},
		stubInteg{integration.Manifest{Name: "mcp", Scope: integration.ScopeShared, CredentialKinds: []string{"bearer"}}},
		stubInteg{integration.Manifest{Name: "websearch", Scope: integration.ScopeShared}},
	}
	s, err := New(Options{Auth: a, Bus: events.NewBus(), Version: "test", Audit: ce.audit, Credentials: ce.v,
		Integrations: func() []integration.Integration { return integs }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	ce.env = &env{srv: srv, auth: a}
	return ce
}

func TestKindAllowed(t *testing.T) {
	for _, c := range []struct {
		declared []string
		kind     string
		want     bool
	}{
		{[]string{"bearer"}, "bearer", true},
		{[]string{"bearer"}, "bearer:github", true},
		{[]string{"bearer"}, "bearer:", false},
		{[]string{"bearer"}, "bearerx", false},
		{[]string{"bearer:gh"}, "bearer:gh", true},
		{[]string{"bearer:gh"}, "bearer:other", false},
		{[]string{"bearer:gh"}, "bearer", false},
		{nil, "apikey", false},
	} {
		if got := KindAllowed(c.declared, c.kind); got != c.want {
			t.Errorf("KindAllowed(%v, %q) = %v", c.declared, c.kind, got)
		}
	}
}

func TestCredentialEndpoints(t *testing.T) {
	ctx := context.Background()
	e := setupCredentials(t)
	status := func(method, path, tok, body string) int {
		t.Helper()
		return e.do(t, method, path, tok, body).StatusCode
	}

	// Per-user credential for alice.
	resp := e.do(t, "PUT", "/v1/credentials/email/oauth2:google", e.alice,
		`{"secret":"tok-alice","fields":{"refresh_token":"r1"},"expiry":"2999-01-01T00:00:00Z"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(body), "tok-alice") || strings.Contains(string(body), "r1") {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	var cr CredentialResponse
	json.Unmarshal(body, &cr)
	if cr.Integration != "email" || cr.Kind != "oauth2:google" || cr.Scope != "user" || cr.Expiry == nil || cr.UpdatedAt.IsZero() {
		t.Errorf("put response %+v", cr)
	}
	got, err := e.v.Get(ctx, &e.aliceID, "email", "oauth2:google")
	if err != nil || got.Secret != "tok-alice" || got.Fields["refresh_token"] != "r1" {
		t.Fatalf("vault = %+v, %v", got, err)
	}

	// Validation.
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/credentials/nope/apikey", `{"secret":"x"}`, 404},
		{"/v1/credentials/email/password", `{"secret":"x"}`, 400},
		{"/v1/credentials/email/APIKEY", `{"secret":"x"}`, 400},
		{"/v1/credentials/email/apikey", `{"secret":""}`, 400},
		{"/v1/credentials/email/apikey", `{"secret":"x","expiry":"2001-01-01T00:00:00Z"}`, 400},
		{"/v1/credentials/email/apikey", `{"secret":"x","fields":{"Bad Name":"v"}}`, 400},
		{"/v1/credentials/email/apikey", `{"secret":"x","extra":1}`, 400},
		{"/v1/credentials/email/apikey?scope=everyone", `{"secret":"x"}`, 400},
		{"/v1/credentials/email/apikey?scope=shared", `{"secret":"x"}`, 400}, // per-user integration never reads shared
		{"/v1/credentials/mcp/bearer", `{"secret":"x"}`, 403},                // shared integration, member
		{"/v1/credentials/mcp/bearer?scope=user", `{"secret":"x"}`, 400},     // per-user write to shared integration
		{"/v1/credentials/websearch/apikey", `{"secret":"x"}`, 400},          // declares no credential kinds
	} {
		if got := status("PUT", c.path, e.alice, c.body); got != c.want {
			t.Errorf("PUT %s %s = %d, want %d", c.path, c.body, got, c.want)
		}
	}
	if got := status("PUT", "/v1/credentials/email/apikey?scope=shared", e.admin, `{"secret":"x"}`); got != 400 {
		t.Errorf("admin shared write to per-user integration = %d, want 400", got)
	}
	if got := status("PUT", "/v1/credentials/websearch/apikey", e.admin, `{"secret":"x"}`); got != 400 {
		t.Errorf("undeclared kind = %d, want 400", got)
	}

	// Shared credential: admin, defaulting to shared for a ScopeShared
	// integration, and the bearer:<suffix> form.
	resp = e.do(t, "PUT", "/v1/credentials/mcp/bearer:github", e.admin, `{"secret":"ghp_secret"}`)
	json.NewDecoder(resp.Body).Decode(&cr)
	if resp.StatusCode != 200 || cr.Scope != "shared" {
		t.Fatalf("admin put mcp = %d %+v", resp.StatusCode, cr)
	}
	if c, err := e.v.Get(ctx, nil, "mcp", "bearer:github"); err != nil || c.Secret != "ghp_secret" {
		t.Fatalf("shared vault = %+v, %v", c, err)
	}

	// Listing: alice sees her own and the shared metadata; bob only shared.
	list := func(tok string) []CredentialResponse {
		t.Helper()
		resp := e.do(t, "GET", "/v1/credentials", tok, "")
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || strings.Contains(string(b), "secret") {
			t.Fatalf("list = %d %s", resp.StatusCode, b)
		}
		var l CredentialListResponse
		json.Unmarshal(b, &l)
		return l.Credentials
	}
	if l := list(e.alice); len(l) != 2 || l[0].Integration != "email" || l[0].Scope != "user" || l[1].Scope != "shared" {
		t.Errorf("alice list = %+v", l)
	}
	if l := list(e.bob); len(l) != 1 || l[0].Integration != "mcp" {
		t.Errorf("bob list = %+v", l)
	}

	// Delete.
	if got := status("DELETE", "/v1/credentials/email/oauth2:google", e.bob, ""); got != 404 {
		t.Errorf("bob deleting alice's = %d", got)
	}
	if got := status("DELETE", "/v1/credentials/mcp/bearer:github", e.alice, ""); got != 403 {
		t.Errorf("member deleting shared = %d", got)
	}
	if got := status("DELETE", "/v1/credentials/email/oauth2:google", e.alice, ""); got != 204 {
		t.Errorf("delete = %d", got)
	}
	if got := status("DELETE", "/v1/credentials/email/oauth2:google", e.alice, ""); got != 404 {
		t.Errorf("delete again = %d", got)
	}
	if got := status("DELETE", "/v1/credentials/mcp/bearer:github", e.admin, ""); got != 204 {
		t.Errorf("admin delete shared = %d", got)
	}
	// A credential for an integration that is no longer enabled can still
	// be removed.
	if err := e.v.Put(ctx, &e.aliceID, "calendar", "apikey", integration.Credential{Secret: "old"}); err != nil {
		t.Fatal(err)
	}
	if got := status("DELETE", "/v1/credentials/calendar/apikey", e.alice, ""); got != 204 {
		t.Errorf("delete stale = %d", got)
	}

	// Audit: every write/delete recorded as config, never with the secret.
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	if len(e.audit.entries) != 5 {
		t.Errorf("audit entries = %d, want 5", len(e.audit.entries))
	}
	for _, en := range e.audit.entries {
		raw, _ := json.Marshal(en)
		if en.Kind != audit.KindConfig || bytes.Contains(raw, []byte("tok-alice")) || bytes.Contains(raw, []byte("ghp_secret")) ||
			bytes.Contains(raw, []byte(`"r1"`)) {
			t.Errorf("audit entry %s", raw)
		}
	}
	if d := string(e.audit.entries[0].Detail); !strings.Contains(d, `"refresh_token"`) || !strings.Contains(d, `"scope":"user"`) {
		t.Errorf("audit detail %s", d)
	}
}
