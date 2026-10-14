package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
	"github.com/perishabledave/bosun/internal/oauth"
	"github.com/perishabledave/bosun/internal/oauth/oauthtest"
)

// credStub is a per-user integration that keeps the CredentialSource the
// daemon hands it, like the email and calendar integrations do.
type credStub struct{ creds integration.CredentialSource }

func (s *credStub) Manifest() integration.Manifest {
	return integration.Manifest{Name: "mailstub", Version: "test", Scope: integration.ScopePerUser,
		CredentialKinds: []string{"oauth2:google"}}
}
func (s *credStub) Init(_ context.Context, _ integration.Config, c integration.CredentialSource) error {
	s.creds = c
	return nil
}
func (s *credStub) Tools() []integration.Tool { return nil }
func (s *credStub) Close() error              { return nil }

// TestOAuthConnectE2E: the admin stores the Google client secret in the
// vault, a member links their account through the daemon's endpoints and
// callback, and an integration's CredentialSource then yields the token.
func TestOAuthConnectE2E(t *testing.T) {
	idp := oauthtest.New()
	defer idp.Close()
	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm: { default: fake/m, providers: { fake: {} } }
memory: { enabled: false }
oauth:
  google:
    client_id: %q
    client_secret_env: BOSUN_TEST_UNSET_SECRET_ENV
    auth_url: %q
    token_url: %q
    userinfo_url: %q
    revoke_url: %q
`, filepath.Join(dir, "data"), idp.ClientID, idp.Server.URL+"/authorize", idp.Server.URL+"/token",
		idp.Server.URL+"/userinfo", idp.Server.URL+"/revoke"))
	if err != nil {
		t.Fatal(err)
	}
	reg := llm.NewRegistry()
	if err := reg.Register(fake.New("fake")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &credStub{}
	d, err := newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		deps{LLM: reg, Argon2: cheapArgon(), Integrations: []integration.Integration{stub}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		d.Close()
	}()
	h := &harness{t: t, d: d, base: "http://" + ln.Addr().String()}

	if _, err := d.auth.CreateUser(ctx, "admin", "admin-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	bobU, err := d.auth.CreateUser(ctx, "bob", "bob-password", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	admin, bob := h.login("admin", "admin-password"), h.login("bob", "bob-password")

	// Without a client secret, Google cannot be started.
	if c := h.do("POST", "/v1/oauth/google/start", bob, api.OAuthStartRequest{}, nil); c != 400 {
		t.Errorf("start without secret = %d", c)
	}
	// The admin stores it through the "oauth" pseudo-integration.
	if c := h.do("PUT", "/v1/credentials/oauth/client_secret:google", bob, api.PutCredentialRequest{Secret: idp.ClientSecret}, nil); c != 403 {
		t.Errorf("member stores client secret = %d", c)
	}
	if c := h.do("PUT", "/v1/credentials/oauth/client_secret:google", admin, api.PutCredentialRequest{Secret: idp.ClientSecret}, nil); c != 200 {
		t.Fatalf("admin stores client secret = %d", c)
	}

	var start oauth.StartResult
	if c := h.do("POST", "/v1/oauth/google/start", bob, api.OAuthStartRequest{Scopes: []string{"email"}}, &start); c != 200 {
		t.Fatalf("start = %d", c)
	}
	if start.RedirectURL != h.base+"/oauth/callback" {
		t.Errorf("redirect = %s", start.RedirectURL)
	}
	resp, err := http.Get(start.AuthURL) // the browser: IdP → 302 → bosund /oauth/callback
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "close this tab") {
		t.Fatalf("callback = %d %s", resp.StatusCode, body)
	}
	var fs oauth.FlowStatus
	if c := h.do("GET", "/v1/oauth/google/status?state="+start.State, bob, nil, &fs); c != 200 || fs.Status != oauth.StatusComplete {
		t.Fatalf("status = %d %+v", c, fs)
	}

	cred, err := stub.creds.Get(integration.WithUserID(ctx, bobU.ID), "oauth2:google")
	if err != nil || cred.Secret == "" || cred.Fields["email"] != "alice@example.com" || cred.Kind != "oauth2:google" {
		t.Fatalf("integration credential = %+v, %v", cred, err)
	}
	if _, err := stub.creds.Get(integration.WithUserID(ctx, 1), "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("admin (not linked) = %v", err)
	}

	var list api.OAuthListResponse
	if c := h.do("GET", "/v1/oauth", bob, nil, &list); c != 200 || len(list.Accounts) != 1 {
		t.Errorf("list = %d %+v", c, list)
	}
	if c := h.do("DELETE", "/v1/oauth/google", bob, nil, nil); c != 204 {
		t.Errorf("delete = %d", c)
	}
	if _, err := stub.creds.Get(integration.WithUserID(ctx, bobU.ID), "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("after unlink = %v", err)
	}
}
