package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integrations/browser"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

// bistro is a local site for the browser end-to-end test: a home page with a
// plain link (ref_1) and a reservation form (name ref_2, button ref_3).
type bistro struct {
	mu       sync.Mutex
	reserved []string
}

func (b *bistro) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reserved)
}

func (b *bistro) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Bistro</title><h1>Bistro</h1>
<a href="/menu">See the menu</a>
<form method="post" action="/reserve"><label>Name <input name="name" value="Alice"></label>
<button type="submit">Reserve a table</button></form>`)
	})
	mux.HandleFunc("GET /menu", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Menu</title><h1>Menu</h1><p>Soup</p>`)
	})
	mux.HandleFunc("POST /reserve", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		b.mu.Lock()
		b.reserved = append(b.reserved, r.PostForm.Get("name"))
		b.mu.Unlock()
		fmt.Fprint(w, `<!doctype html><title>Reserved</title><p>Table reserved.</p>`)
	})
	return mux
}

// TestEndToEndBrowser drives the browser integration through the daemon
// with the fake LLM and a real headless Chrome: a site-scoped rule lets
// browser.act run without asking, browser.submit asks with a preview
// served by the API, approving submits, and denying leaves the page alone.
func TestEndToEndBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("live browser test skipped in -short mode")
	}
	if _, err := browser.FindChrome(""); err != nil {
		t.Skipf("no Chrome/Chromium on this machine: %v", err)
	}
	site := &bistro{}
	srv := httptest.NewServer(site.handler())
	t.Cleanup(srv.Close)
	host := "127.0.0.1"

	dir := t.TempDir()
	noSandbox := runtime.GOOS == "linux"
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm:
  default: fake/test-model
  providers: { fake: {} }
memory: { enabled: false }
integrations:
  browser: { enabled: true, allow_private: true, no_sandbox: %t }
policy:
  defaults: { read: allow, write: ask, destructive: ask }
  approval_timeout: 1m
`, filepath.Join(dir, "data"), noSandbox))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, llm: fake.New("fake")}
	reg := llm.NewRegistry()
	if err := reg.Register(h.llm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.d, err = newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), deps{LLM: reg, Argon2: cheapArgon()})
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
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		h.d.Close()
	})
	alice := h.login("alice", "alice-password")
	bob := h.login("bob", "bob-password")

	// Grant browser.act on the fixture's site only.
	var rule api.PolicyRuleResponse
	if c := h.do("PUT", "/v1/policy/browser.act?scope=WWW."+host, alice, api.SetPolicyRuleRequest{Decision: "allow"}, &rule); c != 200 ||
		rule.Scope != host || rule.Decision != "allow" {
		t.Fatalf("scoped rule = %d %+v", c, rule)
	}
	var pol api.PolicyResponse
	if c := h.do("GET", "/v1/policy", alice, nil, &pol); c != 200 || len(pol.Rules) != 1 || pol.Rules[0].Scope != host {
		t.Fatalf("policy = %d %+v", c, pol)
	}
	if c := h.do("PUT", "/v1/policy/browser.act?scope=not%20a%20site", alice, api.SetPolicyRuleRequest{Decision: "allow"}, nil); c != 400 {
		t.Errorf("bad scope = %d", c)
	}

	siteArg := `"site":"` + host + `"`
	conv := h.newConversation(alice)

	// 1. open, read, click a plain link: no approval (read is allowed by
	// default; the scoped rule covers browser.act).
	h.llm.Push(
		fake.ToolCall("o1", "browser__open", `{"url":"`+srv.URL+`/"}`),
		fake.ToolCall("r1", "browser__read", `{}`),
		fake.ToolCall("c1", "browser__click", `{`+siteArg+`,"ref":"ref_1"}`),
		fake.Text("opened the menu"))
	evs := h.turn(alice, conv, "show me the menu", nil)
	if got := strings.Join(types(evs), ","); strings.Contains(got, "approval_required") || evs[len(evs)-1].Type != "done" {
		t.Fatalf("events = %s", got)
	}
	reqs := h.llm.Requests()
	if tr := lastToolResult(t, reqs[len(reqs)-1]); tr.IsError || !strings.Contains(tr.Content, "Now on: Menu") ||
		!strings.Contains(tr.Content, "untrusted-data") {
		t.Fatalf("click result = %+v", tr)
	}
	if tr := lastToolResult(t, reqs[len(reqs)-2]); !strings.Contains(tr.Content, `[ref_3] button "Reserve a table"`) {
		t.Fatalf("read result = %+v", tr)
	}

	// 2. The gate: clicking the submit button is refused, pointing at
	// browser.submit. Then browser.submit asks, with a preview; approve.
	var previewStatus, bobStatus int
	var previewType string
	var approval struct{ Scope, Risk string }
	h.llm.Push(
		fake.ToolCall("o2", "browser__open", `{"url":"`+srv.URL+`/"}`),
		fake.ToolCall("c2", "browser__click", `{`+siteArg+`,"ref":"ref_3"}`),
		fake.ToolCall("s2", "browser__submit", `{`+siteArg+`,"ref":"ref_3","irreversible":true,"summary":"Reserve a table for Alice"}`),
		fake.Text("reserved"))
	evs = h.turn(alice, conv, "book it", func(e sseEvent) {
		if e.Type != "approval_required" {
			return
		}
		id := field(e, "approval_id")
		approval.Scope, approval.Risk = field(e, "scope"), field(e, "risk")
		resp := h.get("/v1/approvals/"+id+"/preview", alice)
		previewStatus, previewType = resp.StatusCode, resp.Header.Get("Content-Type")
		resp.Body.Close()
		resp = h.get("/v1/approvals/"+id+"/preview", bob)
		bobStatus = resp.StatusCode
		resp.Body.Close()
		if site.count() != 0 {
			t.Error("submitted before approval")
		}
		if c := h.do("POST", "/v1/approvals/"+id, alice, api.ResolveApprovalRequest{Decision: "approve"}, nil); c != 200 {
			t.Errorf("approve = %d", c)
		}
		if resp := h.get("/v1/approvals/"+id+"/preview", alice); resp.StatusCode != 404 {
			t.Errorf("preview after approval = %d", resp.StatusCode)
			resp.Body.Close()
		}
	})
	if previewStatus != 200 || previewType != "image/png" || bobStatus != 404 {
		t.Errorf("preview: alice %d %q, bob %d", previewStatus, previewType, bobStatus)
	}
	if approval.Scope != host || approval.Risk != "destructive" {
		t.Errorf("approval = %+v", approval)
	}
	reqs = h.llm.Requests()
	gate := lastToolResult(t, reqs[len(reqs)-2])
	if !gate.IsError || !strings.Contains(gate.Content, "browser.submit") {
		t.Errorf("click on submit button = %+v", gate)
	}
	if tr := lastToolResult(t, reqs[len(reqs)-1]); tr.IsError || !strings.Contains(tr.Content, "Reserved") {
		t.Errorf("submit result = %+v", tr)
	}
	if site.count() != 1 {
		t.Errorf("reservations = %d", site.count())
	}

	// 3. A denied submit leaves the page unchanged.
	h.llm.Push(
		fake.ToolCall("o3", "browser__open", `{"url":"`+srv.URL+`/"}`),
		fake.ToolCall("s3", "browser__submit", `{`+siteArg+`,"ref":"ref_3","irreversible":true,"summary":"Reserve again"}`),
		fake.Text("not booked"))
	h.turn(alice, conv, "book again", func(e sseEvent) {
		if e.Type == "approval_required" {
			h.do("POST", "/v1/approvals/"+field(e, "approval_id"), alice, api.ResolveApprovalRequest{Decision: "deny"}, nil)
		}
	})
	reqs = h.llm.Requests()
	if tr := lastToolResult(t, reqs[len(reqs)-1]); !tr.IsError || !strings.Contains(tr.Content, "denied") {
		t.Errorf("denied submit = %+v", tr)
	}
	if site.count() != 1 {
		t.Errorf("reservations after deny = %d", site.count())
	}
	var st api.BrowserStatus
	if c := h.do("GET", "/v1/browser", alice, nil, &st); c != 200 || !st.Running || !st.Enabled || st.URL != srv.URL+"/" ||
		st.Steps < 5 || !st.HasScreenshot {
		t.Errorf("status = %d %+v", c, st)
	}
	if resp := h.get("/v1/browser/screenshot", alice); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("screenshot = %d", resp.StatusCode)
	}
	if resp := h.get("/v1/browser/screenshot", bob); resp.StatusCode != 404 {
		t.Errorf("bob screenshot = %d", resp.StatusCode)
	}

	// 4. "Approve and always allow on <site>" for browser.login-like scoped
	// approvals: a scoped browser.act approval on another site.
	if c := h.do("DELETE", "/v1/policy/browser.act?scope="+url.QueryEscape(host), alice, nil, nil); c != 204 {
		t.Fatalf("delete scoped rule = %d", c)
	}
	h.llm.Push(
		fake.ToolCall("o4", "browser__open", `{"url":"`+srv.URL+`/"}`),
		fake.ToolCall("c4", "browser__click", `{`+siteArg+`,"ref":"ref_1"}`),
		fake.Text("ok"))
	h.turn(alice, conv, "menu again", func(e sseEvent) {
		if e.Type == "approval_required" {
			if c := h.do("POST", "/v1/approvals/"+field(e, "approval_id"), alice,
				api.ResolveApprovalRequest{Decision: "approve", Always: true}, nil); c != 200 {
				t.Errorf("approve always = %d", c)
			}
		}
	})
	if c := h.do("GET", "/v1/policy", alice, nil, &pol); c != 200 || len(pol.Rules) != 1 || pol.Rules[0].Scope != host ||
		pol.Rules[0].Capability != "browser.act" || pol.Rules[0].Decision != "allow" {
		t.Errorf("rule from always-allow = %+v", pol)
	}

	// 5. Stop.
	if c := h.do("POST", "/v1/browser/stop", alice, nil, nil); c != 204 {
		t.Errorf("stop = %d", c)
	}
	if c := h.do("GET", "/v1/browser", alice, nil, &st); c != 200 || st.Running {
		t.Errorf("status after stop = %+v", st)
	}
}

func (h *harness) get(path, token string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}
