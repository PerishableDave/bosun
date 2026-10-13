package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/integrations/mcp/mcptest"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/fake"
)

// TestMCPBearerFromCredentialsAPI: an MCP server behind a bearer token is
// unusable until an admin stores the token through /v1/credentials; after a
// restart the bridge connects with it.
func TestMCPBearerFromCredentialsAPI(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	mcpSrv := httptest.NewServer(mcptest.Handler(mcptest.HTTPOptions{Token: "tok-123", SeenAuth: func(a string) {
		mu.Lock()
		seen = append(seen, a)
		mu.Unlock()
	}}))
	defer mcpSrv.Close()

	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm: { default: fake/m, providers: { fake: {} } }
memory: { enabled: false }
integrations:
  mcp:
    servers:
      - { name: remote, url: %q, credential: "bearer:remote" }
`, filepath.Join(dir, "data"), mcpSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	reg := llm.NewRegistry()
	if err := reg.Register(fake.New("fake")); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	start := func() (*harness, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		d, err := newDaemon(ctx, cfg, log, deps{LLM: reg, Argon2: cheapArgon()})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- d.Serve(ctx, ln) }()
		h := &harness{t: t, d: d, base: "http://" + ln.Addr().String()}
		return h, func() {
			cancel()
			select {
			case <-served:
			case <-time.After(15 * time.Second):
				t.Error("server did not shut down")
			}
			d.Close()
		}
	}

	h, stop := start()
	if _, err := h.d.auth.CreateUser(context.Background(), "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.auth.CreateUser(context.Background(), "bob", "bob-password", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	alice, bob := h.login("alice", "alice-password"), h.login("bob", "bob-password")
	var il api.IntegrationListResponse
	if c := h.do("GET", "/v1/integrations", alice, nil, &il); c != 200 || len(il.Integrations) != 1 || il.Integrations[0].Status == "ok" {
		t.Fatalf("before credential: %d %+v", c, il)
	}
	if c := h.do("PUT", "/v1/credentials/mcp/bearer:remote", bob, api.PutCredentialRequest{Secret: "tok-123"}, nil); c != 403 {
		t.Errorf("member PUT shared = %d", c)
	}
	if c := h.do("PUT", "/v1/credentials/mcp/bearer:other", alice, api.PutCredentialRequest{Secret: "x"}, nil); c != 400 {
		t.Errorf("undeclared kind = %d", c)
	}
	var cr api.CredentialResponse
	if c := h.do("PUT", "/v1/credentials/mcp/bearer:remote", alice, api.PutCredentialRequest{Secret: "tok-123"}, &cr); c != 200 || cr.Scope != "shared" {
		t.Fatalf("PUT = %d %+v", c, cr)
	}
	stop()

	h, stop = start()
	defer stop()
	alice = h.login("alice", "alice-password")
	var tl api.ToolListResponse
	if c := h.do("GET", "/v1/tools", alice, nil, &tl); c != 200 {
		t.Fatalf("tools = %d", c)
	}
	found := false
	for _, tool := range tl.Tools {
		found = found || strings.HasPrefix(tool.Name, "mcp.remote.")
	}
	if !found {
		t.Errorf("no mcp.remote tools after storing the credential: %+v", tl.Tools)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[len(seen)-1] != "Bearer tok-123" {
		t.Errorf("MCP server saw Authorization %q", seen)
	}
}
