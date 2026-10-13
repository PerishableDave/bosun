package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/integrations/mcp/mcptest"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/fake"
)

// TestMain lets the test binary double as a fake stdio MCP server, so the
// MCP scenario needs no network and no npm.
func TestMain(m *testing.M) {
	if os.Getenv(mcptest.EnvServe) == "1" {
		mcptest.Main()
		return
	}
	os.Exit(m.Run())
}

// newMCPHarness starts a daemon whose only integration is the MCP bridge
// with two servers: "fake" (this test binary) and "broken" (cannot start).
func newMCPHarness(t *testing.T) *harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The child only sees variables mapped in config; map a differently
	// named daemon variable so this process does not itself turn into the
	// fake server.
	t.Setenv("BOSUN_E2E_MCP_SERVE", "1")
	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm:
  default: fake/test-model
  providers: { fake: {} }
memory: { enabled: false }
integrations:
  mcp:
    servers:
      - name: fake
        command: [%q]
        env: { %s: BOSUN_E2E_MCP_SERVE }
      - name: broken
        command: ["/nonexistent/mcp-server"]
policy:
  defaults: { read: allow, write: ask, destructive: deny }
  approval_timeout: 1m
`, filepath.Join(dir, "data"), exe, mcptest.EnvServe))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, llm: fake.New("fake")}
	reg := llm.NewRegistry()
	if err := reg.Register(h.llm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.d, err = newDaemon(ctx, cfg, log, deps{LLM: reg, Argon2: cheapArgon()})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if h.alice, err = h.d.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if h.bob, err = h.d.auth.CreateUser(ctx, "bob", "bob-password", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.base = "http://" + ln.Addr().String()
	served := make(chan error, 1)
	go func() { served <- h.d.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		h.d.Close()
	})
	return h
}

func TestEndToEndMCP(t *testing.T) {
	h := newMCPHarness(t)
	alice := h.login("alice", "alice-password")
	bob := h.login("bob", "bob-password")

	t.Run("bridged tools listed with risk and policy", func(t *testing.T) {
		var tl api.ToolListResponse
		if c := h.do("GET", "/v1/tools", alice, nil, &tl); c != 200 {
			t.Fatalf("tools = %d", c)
		}
		got := map[string]string{}
		for _, tool := range tl.Tools {
			got[tool.Name] = tool.Integration + "/" + tool.Capability + "/" + tool.Risk + "/" + tool.Decision
		}
		for name, want := range map[string]string{
			"mcp.fake.create_issue": "mcp/mcp.fake.create_issue/write/ask",
			"mcp.fake.echo":         "mcp/mcp.fake.echo/read/allow",
			"mcp.fake.delete_all":   "mcp/mcp.fake.delete_all/destructive/deny",
		} {
			if got[name] != want {
				t.Errorf("%s = %q, want %q", name, got[name], want)
			}
		}
		for name := range got {
			if strings.HasPrefix(name, "mcp.broken.") {
				t.Errorf("tool from failed server listed: %s", name)
			}
		}
	})

	t.Run("integration status", func(t *testing.T) {
		var il api.IntegrationListResponse
		if c := h.do("GET", "/v1/integrations", alice, nil, &il); c != 200 || len(il.Integrations) != 1 {
			t.Fatalf("integrations = %d %+v", c, il)
		}
		in := il.Integrations[0]
		if in.Name != "mcp" || in.Scope != "shared" || in.Status != "degraded" || in.ToolCount != 8 ||
			!strings.Contains(in.Message, "broken failed") || in.Details == nil {
			t.Errorf("mcp integration = %+v", in)
		}
		var bl api.IntegrationListResponse
		if c := h.do("GET", "/v1/integrations", bob, nil, &bl); c != 200 || len(bl.Integrations) != 1 || bl.Integrations[0].Details != nil {
			t.Errorf("member sees details: %d %+v", c, bl)
		}
	})

	t.Run("write tool through the bridge needs approval", func(t *testing.T) {
		conv := h.newConversation(alice)
		h.llm.Push(fake.ToolCall("call_m1", "mcp__fake__create_issue", `{"title":"leaky tap"}`), fake.Text("Filed it."))
		evs := h.turn(alice, conv, "file an issue about the leaky tap", func(e sseEvent) {
			if e.Type == "approval_required" {
				if c := h.do("POST", "/v1/approvals/"+field(e, "approval_id"), alice, api.ResolveApprovalRequest{Decision: "approve"}, nil); c != 200 {
					t.Errorf("approve = %d", c)
				}
			}
		})
		if got := strings.Join(types(evs), ","); got != "tool_call,approval_required,approval_resolved,tool_result,text,done" {
			t.Fatalf("event types = %s", got)
		}
		if field(evs[0], "name") != "mcp.fake.create_issue" {
			t.Errorf("tool_call = %s", evs[0].Data)
		}
		reqs := h.llm.Requests()
		tr := lastToolResult(t, reqs[len(reqs)-1])
		if tr.IsError || !strings.Contains(tr.Content, "<untrusted-data") || !strings.Contains(tr.Content, "created leaky tap") {
			t.Errorf("tool result to model = %+v", tr)
		}
	})
}

// A bridge whose servers all fail must not stop the daemon from starting.
func TestDaemonStartsWhenEveryMCPServerFails(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm: { default: fake/m, providers: { fake: {} } }
memory: { enabled: false }
integrations:
  mcp:
    servers:
      - { name: broken, command: ["/nonexistent/mcp-server"] }
`, filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	reg := llm.NewRegistry()
	if err := reg.Register(fake.New("fake")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), deps{LLM: reg, Argon2: cheapArgon()})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	defer d.Close()
	if len(d.integrations) != 1 || len(d.integrations[0].Tools()) != 0 {
		t.Errorf("integrations = %+v", d.integrations)
	}
}
