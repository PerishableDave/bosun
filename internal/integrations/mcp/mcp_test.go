package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/integrations/mcp/mcptest"
)

// TestMain lets the test binary double as the fake stdio MCP server.
func TestMain(m *testing.M) {
	if os.Getenv(mcptest.EnvServe) == "1" {
		mcptest.Main()
		return
	}
	os.Exit(m.Run())
}

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// fakeEnv is the daemon environment the integration sees: real variables
// plus overrides.
func fakeEnv(over map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := over[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stdioServer returns a config entry that runs the fake server. extra env
// mappings are child var -> daemon var.
func stdioServer(t *testing.T, name string, extra map[string]string) map[string]any {
	env := map[string]any{mcptest.EnvServe: "T_SERVE"}
	for k, v := range extra {
		env[k] = v
	}
	return map[string]any{"name": name, "command": []string{testBinary(t)}, "env": env}
}

func newBridge(t *testing.T, env map[string]string, creds integration.CredentialSource, servers ...map[string]any) *Integration {
	t.Helper()
	i := New(WithLogger(testLogger(t)), WithClientVersion("test"))
	base := map[string]string{"T_SERVE": "1", "T_ONE": "1"}
	for k, v := range env {
		base[k] = v
	}
	i.getenv = fakeEnv(base)
	i.killGrace = 300 * time.Millisecond
	i.connectTimeout = 10 * time.Second
	settings, _ := json.Marshal(map[string]any{"servers": servers})
	if err := i.Init(context.Background(), integration.Config{Name: "mcp", Settings: settings}, creds); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { i.Close() })
	return i
}

func toolByName(t *testing.T, i *Integration, name string) integration.Tool {
	t.Helper()
	for _, tl := range i.Tools() {
		if tl.Name == name {
			return tl
		}
	}
	var names []string
	for _, tl := range i.Tools() {
		names = append(names, tl.Name)
	}
	t.Fatalf("tool %s not found in %v", name, names)
	return integration.Tool{}
}

func call(t *testing.T, i *Integration, name, args string) (integration.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return toolByName(t, i, name).Handler(ctx, json.RawMessage(args))
}

func TestManifestBeforeInit(t *testing.T) {
	m := New().Manifest()
	if m.Name != "mcp" || m.Version != "0.1.0" || m.Scope != integration.ScopeShared || len(m.Capabilities) != 0 {
		t.Errorf("manifest = %+v", m)
	}
	if !json.Valid(m.ConfigSchema) {
		t.Error("config schema is not valid JSON")
	}
	if len(New().Tools()) != 0 {
		t.Error("tools before Init")
	}
}

func TestStdioBridge(t *testing.T) {
	i := newBridge(t, map[string]string{"SECRET_API_KEY": "sk-leak", "GH_TOKEN_DAEMON": "ghp_ok"}, nil,
		stdioServer(t, "fake", map[string]string{"GITHUB_TOKEN": "GH_TOKEN_DAEMON"}))

	t.Run("handshake and status", func(t *testing.T) {
		st := i.Servers()
		if len(st) != 1 || st[0].State != StateReady || st[0].Transport != "stdio" || st[0].ServerName != "mcptest" ||
			st[0].ProtocolVersion != mcptest.ProtocolVersion || st[0].Tools != 8 {
			t.Fatalf("status = %+v", st)
		}
		if s := i.Status(); s.State != integration.StatusOK {
			t.Errorf("Status = %+v", s)
		}
	})

	t.Run("tools listed across pages, sanitized, prefixed, risk mapped", func(t *testing.T) {
		want := map[string]integration.Risk{
			"mcp.fake.echo": integration.RiskRead, "mcp.fake.create_issue": integration.RiskWrite,
			"mcp.fake.delete_all": integration.RiskDestructive, "mcp.fake.fail": integration.RiskRead,
			"mcp.fake.env": integration.RiskRead, "mcp.fake.mixed": integration.RiskRead,
			"mcp.fake.sleep": integration.RiskRead, "mcp.fake.crash": integration.RiskRead,
		}
		tools := i.Tools()
		if len(tools) != len(want) {
			t.Fatalf("got %d tools, want %d", len(tools), len(want))
		}
		m := i.Manifest()
		for _, tl := range tools {
			if r, ok := want[tl.Name]; !ok || r != tl.Risk {
				t.Errorf("tool %s risk %s, want %v", tl.Name, tl.Risk, r)
			}
			if string(tl.Capability) != tl.Name || !m.HasCapability(tl.Capability) {
				t.Errorf("tool %s capability %s not declared", tl.Name, tl.Capability)
			}
			if !json.Valid(tl.InputSchema) {
				t.Errorf("tool %s schema invalid", tl.Name)
			}
		}
		if s := string(toolByName(t, i, "mcp.fake.delete_all").InputSchema); s != `{"type":"object"}` {
			t.Errorf("default schema = %s", s)
		}
		if d := toolByName(t, i, "mcp.fake.create_issue").Description; !strings.Contains(d, `"Create Issue!"`) || !strings.Contains(d, "third-party") {
			t.Errorf("description = %q", d)
		}
		if err := integration.NewRegistry().Register(i); err != nil {
			t.Errorf("Register: %v", err)
		}
	})

	t.Run("call returns untrusted text", func(t *testing.T) {
		r, err := call(t, i, "mcp.fake.echo", `{"message":"hi there"}`)
		if err != nil || r.Content != "hi there" || !r.Untrusted {
			t.Errorf("echo = %+v, %v", r, err)
		}
		// The original (unsanitized) tool name is what reaches the server.
		r, err = call(t, i, "mcp.fake.create_issue", `{"title":"bug"}`)
		if err != nil || r.Content != "created bug" || !r.Untrusted {
			t.Errorf("create_issue = %+v, %v", r, err)
		}
		if _, err := call(t, i, "mcp.fake.echo", `[1]`); err == nil {
			t.Error("non-object arguments accepted")
		}
	})

	t.Run("isError becomes an error", func(t *testing.T) {
		_, err := call(t, i, "mcp.fake.fail", `{}`)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("non-text content summarized", func(t *testing.T) {
		r, err := call(t, i, "mcp.fake.mixed", ``)
		if err != nil || !r.Untrusted {
			t.Fatalf("mixed = %+v, %v", r, err)
		}
		for _, want := range []string{"hello", "[image image/png]", "[resource file:///a.txt text/plain]\ninside",
			"[resource link file:///b.bin b]", "[future_kind content]"} {
			if !strings.Contains(r.Content, want) {
				t.Errorf("content %q missing %q", r.Content, want)
			}
		}
		if strings.Contains(r.Content, "aGk=") {
			t.Error("image data leaked into text")
		}
	})

	t.Run("call timeout from ctx", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := toolByName(t, i, "mcp.fake.sleep").Handler(ctx, nil)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Errorf("err = %v after %v", err, time.Since(start))
		}
		if r, err := call(t, i, "mcp.fake.echo", `{"message":"still alive"}`); err != nil || r.Content != "still alive" {
			t.Errorf("after timeout: %+v %v", r, err)
		}
	})

	t.Run("child environment is minimal", func(t *testing.T) {
		r, err := call(t, i, "mcp.fake.env", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, mcptest.EnvServe: true, "GITHUB_TOKEN": true,
			"__CF_USER_TEXT_ENCODING": true} // added by macOS libc in some environments
		got := map[string]string{}
		for _, line := range strings.Split(r.Content, "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
			if !allowed[k] {
				t.Errorf("unexpected child env var %q", k)
			}
		}
		if got["GITHUB_TOKEN"] != "ghp_ok" || got[mcptest.EnvServe] != "1" {
			t.Errorf("mapped vars missing: %v", got)
		}
		if strings.Contains(r.Content, "sk-leak") {
			t.Error("daemon secret leaked into child env")
		}
		if os.Getenv("PATH") != "" && got["PATH"] != os.Getenv("PATH") {
			t.Errorf("PATH = %q", got["PATH"])
		}
	})

	t.Run("crash: clear error and status", func(t *testing.T) {
		if _, err := call(t, i, "mcp.fake.crash", `{}`); err == nil || !strings.Contains(err.Error(), "mcp server fake is not running") {
			t.Errorf("crash call err = %v", err)
		}
		_, err := call(t, i, "mcp.fake.echo", `{"message":"x"}`)
		if err == nil || !strings.Contains(err.Error(), "mcp server fake is not running") || !errors.Is(err, errNotRunning) {
			t.Errorf("after crash err = %v", err)
		}
		st := i.Servers()[0]
		if st.State != StateExited || !strings.Contains(st.Error, "exit status 3") {
			t.Errorf("status = %+v", st)
		}
		if s := i.Status(); s.State != integration.StatusDown || !strings.Contains(s.Message, "fake exited") {
			t.Errorf("Status = %+v", s)
		}
	})
}

func TestFailedServersAreSkipped(t *testing.T) {
	i := newBridge(t, map[string]string{"T_FAIL": "1"}, nil,
		map[string]any{"name": "missing", "command": []string{"/nonexistent/mcp-server"}},
		stdioServer(t, "badinit", map[string]string{mcptest.EnvFailInit: "T_FAIL"}),
		stdioServer(t, "good", nil),
	)
	st := i.Servers()
	if len(st) != 3 {
		t.Fatalf("status = %+v", st)
	}
	if st[0].State != StateFailed || st[0].Error == "" || st[1].State != StateFailed || !strings.Contains(st[1].Error, "init refused") {
		t.Errorf("failed servers = %+v / %+v", st[0], st[1])
	}
	if st[2].State != StateReady || st[2].Tools != 8 {
		t.Errorf("good server = %+v", st[2])
	}
	for _, tl := range i.Tools() {
		if !strings.HasPrefix(tl.Name, "mcp.good.") {
			t.Errorf("tool from failed server: %s", tl.Name)
		}
	}
	if s := i.Status(); s.State != integration.StatusDegraded || !strings.Contains(s.Message, "missing failed") {
		t.Errorf("Status = %+v", s)
	}
	if err := integration.Validate(i); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestNoServers(t *testing.T) {
	i := newBridge(t, nil, nil)
	if len(i.Tools()) != 0 || len(i.Manifest().Capabilities) != 0 || i.Status().State != integration.StatusOK {
		t.Errorf("tools %v status %+v", i.Tools(), i.Status())
	}
}

func stdioOf(t *testing.T, i *Integration) *stdioTransport {
	t.Helper()
	i.mu.RLock()
	defer i.mu.RUnlock()
	st, ok := i.servers[0].t.(*stdioTransport)
	if !ok {
		t.Fatal("not a stdio transport")
	}
	return st
}

func processGone(t *testing.T, st *stdioTransport) {
	t.Helper()
	select {
	case <-st.exited:
	default:
		t.Fatal("child has not exited")
	}
	if err := syscall.Kill(st.cmd.Process.Pid, 0); err == nil {
		t.Errorf("pid %d still exists", st.cmd.Process.Pid)
	}
}

func TestCloseTerminatesChild(t *testing.T) {
	i := newBridge(t, nil, nil, stdioServer(t, "fake", nil))
	st := stdioOf(t, i)
	start := time.Now()
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	processGone(t, st)
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Errorf("graceful close took %v (should not need SIGKILL)", d)
	}
	if s := i.Servers()[0]; s.State != StateClosed {
		t.Errorf("state after close = %+v", s)
	}
	if _, err := call(t, i, "mcp.fake.echo", `{"message":"x"}`); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("call after close = %v", err)
	}
	if err := i.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCloseKillsStubbornChild(t *testing.T) {
	i := newBridge(t, map[string]string{"T_STUB": "1"}, nil,
		stdioServer(t, "stubborn", map[string]string{mcptest.EnvStubborn: "T_STUB"}))
	st := stdioOf(t, i)
	start := time.Now()
	i.Close()
	processGone(t, st)
	if d := time.Since(start); d < i.killGrace {
		t.Errorf("closed after %v; expected to wait the %v grace period before SIGKILL", d, i.killGrace)
	}
	if ws, ok := st.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("process state = %v", st.cmd.ProcessState)
	}
}

type fakeCreds struct {
	mu    sync.Mutex
	token string
	kinds []string
}

func (f *fakeCreds) Get(_ context.Context, kind string) (integration.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kinds = append(f.kinds, kind)
	if f.token == "" {
		return integration.Credential{}, integration.ErrNoCredential
	}
	return integration.Credential{Kind: kind, Secret: f.token}, nil
}

func TestHTTPTransport(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			srv := httptest.NewServer(mcptest.Handler(mcptest.HTTPOptions{SSE: sse, Token: "tok-1",
				SeenAuth: func(h string) { mu.Lock(); seen = append(seen, h); mu.Unlock() }}))
			defer srv.Close()
			creds := &fakeCreds{token: "tok-1"}
			i := newBridge(t, nil, creds, map[string]any{"name": "remote", "url": srv.URL + "/mcp", "credential": "bearer"})
			st := i.Servers()
			if len(st) != 1 || st[0].State != StateReady || st[0].Transport != "http" || st[0].Tools != 8 ||
				st[0].ProtocolVersion != mcptest.ProtocolVersion {
				t.Fatalf("status = %+v", st)
			}
			if m := i.Manifest(); !slices.Equal(m.CredentialKinds, []string{"bearer"}) {
				t.Errorf("credential kinds = %v", m.CredentialKinds)
			}
			r, err := call(t, i, "mcp.remote.echo", `{"message":"over http"}`)
			if err != nil || r.Content != "over http" || !r.Untrusted {
				t.Errorf("echo = %+v, %v", r, err)
			}
			if _, err := call(t, i, "mcp.remote.fail", `{}`); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Errorf("fail err = %v", err)
			}
			mu.Lock()
			for _, h := range seen {
				if h != "Bearer tok-1" {
					t.Errorf("Authorization header = %q", h)
				}
			}
			n := len(seen)
			mu.Unlock()
			if n < 6 { // initialize, initialized, 4 list pages, calls
				t.Errorf("only %d requests seen", n)
			}
			creds.mu.Lock()
			if len(creds.kinds) == 0 || creds.kinds[0] != "bearer" {
				t.Errorf("credential lookups = %v", creds.kinds)
			}
			creds.mu.Unlock()
			i.Close()
			if s := i.Servers()[0]; s.State != StateClosed {
				t.Errorf("after close = %+v", s)
			}
		})
	}

	t.Run("no credential: no header", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string
		srv := httptest.NewServer(mcptest.Handler(mcptest.HTTPOptions{SeenAuth: func(h string) { mu.Lock(); seen = append(seen, h); mu.Unlock() }}))
		defer srv.Close()
		i := newBridge(t, nil, nil, map[string]any{"name": "open", "url": srv.URL})
		if st := i.Servers()[0]; st.State != StateReady {
			t.Fatalf("status = %+v", st)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, h := range seen {
			if h != "" {
				t.Errorf("unexpected Authorization %q", h)
			}
		}
	})

	t.Run("wrong or missing token: server fails, not fatal", func(t *testing.T) {
		srv := httptest.NewServer(mcptest.Handler(mcptest.HTTPOptions{Token: "right"}))
		defer srv.Close()
		i := newBridge(t, nil, &fakeCreds{token: "wrong"},
			map[string]any{"name": "a", "url": srv.URL, "credential": "bearer:a"},
			map[string]any{"name": "b", "url": srv.URL})
		st := i.Servers()
		if st[0].State != StateFailed || !strings.Contains(st[0].Error, "401") || st[1].State != StateFailed {
			t.Errorf("status = %+v", st)
		}
		i2 := newBridge(t, nil, &fakeCreds{}, map[string]any{"name": "c", "url": srv.URL, "credential": "bearer"})
		if st := i2.Servers()[0]; st.State != StateFailed || !strings.Contains(st.Error, "no credential") {
			t.Errorf("status = %+v", st)
		}
	})
}

func TestConfigValidation(t *testing.T) {
	bad := []struct {
		settings string
		want     string
	}{
		{`{"servers":[{"name":"x","command":["a"],"bogus":1}]}`, "unknown field"},
		{`{"other":1}`, "unknown field"},
		{`{"servers":[{"name":"Bad","command":["a"]}]}`, "name"},
		{`{"servers":[{"name":"a__b","command":["a"]}]}`, "name"},
		{`{"servers":[{"name":"x","command":["a"]},{"name":"x","command":["b"]}]}`, "duplicate"},
		{`{"servers":[{"name":"x"}]}`, "one of command or url"},
		{`{"servers":[{"name":"x","command":["a"],"url":"http://h"}]}`, "not both"},
		{`{"servers":[{"name":"x","url":"ftp://h"}]}`, "http(s)"},
		{`{"servers":[{"name":"x","url":"http://example.com","credential":"bearer"}]}`, "plain http"},
		{`{"servers":[{"name":"x","url":"https://example.com","credential":"apikey"}]}`, "credential"},
		{`{"servers":[{"name":"x","command":["a"],"credential":"bearer"}]}`, "url servers only"},
		{`{"servers":[{"name":"x","command":["a"],"env":{"A B":"C"}}]}`, "environment variable names"},
		{`{"servers":[{"name":"x","url":"https://h","env":{"A":"B"}}]}`, "command servers only"},
		{`{"servers":[{"name":"x","command":["a"],"timeout":"soon"}]}`, "timeout"},
	}
	for _, c := range bad {
		err := New().Init(context.Background(), integration.Config{Name: "mcp", Settings: json.RawMessage(c.settings)}, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.settings, err, c.want)
		}
	}
	ok := `{"servers":[{"name":"x","url":"http://127.0.0.1:1/mcp","credential":"bearer:x","timeout":"5s"}]}`
	if _, err := parseSettings(integration.Config{Settings: json.RawMessage(ok)}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestSanitizeToolName(t *testing.T) {
	for in, want := range map[string]string{
		"echo":            "echo",
		"Create Issue!":   "create_issue",
		"createIssue":     "create_issue",
		"get-file.v2":     "get-file_v2",
		"HTTPServer":      "httpserver",
		"a__b":            "a_b",
		"__weird__":       "weird",
		"!!!":             "tool",
		"search/repos:*":  "search_repos",
		"Ünïcode tool":    "n_code_tool",
		"list_PRs":        "list_prs",
		"-leading-dash":   "leading-dash",
		"trailing space ": "trailing_space",
	} {
		if got := sanitizeToolName(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolNamerUniqueAndBounded(t *testing.T) {
	n := newToolNamer("server")
	a, b, c := n.name("create issue"), n.name("create-issue!"), n.name("create_issue")
	if a != "create_issue" || b != "create-issue" || c != "create_issue_2" {
		t.Errorf("names = %q %q %q", a, b, c)
	}
	long := strings.Repeat("very_long_tool_name_", 10)
	l1, l2 := n.name(long), n.name(long+"x")
	for _, l := range []string{l1, l2} {
		wire := "mcp__server__" + l
		if len(wire) > maxWireName {
			t.Errorf("wire name %q is %d chars", wire, len(wire))
		}
	}
	if l1 == l2 {
		t.Errorf("long names collide: %q", l1)
	}
	if !validServerName("github") || !validServerName("my-srv_1") || validServerName("a__b") || validServerName("x_") || validServerName("") {
		t.Error("validServerName")
	}
}

func TestRiskFor(t *testing.T) {
	tr, fa := true, false
	mk := func(ro, de *bool) remoteTool {
		var rt remoteTool
		json.Unmarshal([]byte(`{"name":"x","annotations":{}}`), &rt)
		rt.Annotations.ReadOnlyHint, rt.Annotations.DestructiveHint = ro, de
		return rt
	}
	cases := []struct {
		rt   remoteTool
		want integration.Risk
	}{
		{remoteTool{Name: "x"}, integration.RiskWrite},
		{mk(nil, nil), integration.RiskWrite},
		{mk(&tr, nil), integration.RiskRead},
		{mk(&tr, &tr), integration.RiskRead}, // destructiveHint is meaningless when read-only
		{mk(&fa, nil), integration.RiskWrite},
		{mk(nil, &tr), integration.RiskDestructive},
		{mk(&fa, &fa), integration.RiskWrite},
	}
	for n, c := range cases {
		if got := riskFor(c.rt); got != c.want {
			t.Errorf("case %d: risk = %s, want %s", n, got, c.want)
		}
	}
}
