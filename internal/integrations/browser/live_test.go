package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/perishabledave/bosun/internal/integration"
)

// The live tests drive a real headless Chrome or Chromium against a local
// fixture site. They are skipped when no browser is installed (CI's
// ubuntu-latest has /usr/bin/google-chrome, and macOS runners have Google
// Chrome, so they normally run there).

const fixtureHome = `<!doctype html><html><head><title>Fixture Bistro</title></head><body>
<h1>Fixture Bistro</h1>
<p>Welcome to our   test restaurant.</p>
<p style="display:none">hidden text should not appear</p>
<a id="menu" href="/menu">See the menu</a>
<a id="newtab" href="/menu" target="_blank">Menu in a new tab</a>
<a href="/login">Sign in</a>
<form method="get" action="/search"><input name="q" placeholder="Search dishes"></form>
<form method="post" action="/reserve">
  <label>Name <input name="name"></label>
  <label>Party size <select name="party"><option value="2">Two people</option><option value="4">Four people</option></select></label>
  <input type="hidden" name="csrf" value="tok">
  <button type="submit">Reserve a table</button>
</form>
<button type="button" onclick="document.getElementById('out').textContent='details opened'">Show details</button>
<div id="out"></div>
<button type="button" onclick="document.getElementById('out').textContent='deleted!'">Delete account</button>
<button type="button" onclick="alert('hello from the page')">Say hello</button>
</body></html>`

const fixtureLogin = `<!doctype html><html><head><title>Sign in</title></head><body>
<form method="post" action="/session">
<label>Email <input type="email" name="email" autocomplete="username"></label>
<label>Password <input type="password" name="pw"></label>
<button type="submit">Sign in</button>
</form></body></html>`

type fixture struct {
	mu       sync.Mutex
	reserved []string
	sessions []string
	server   *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		}
	}
	mux.HandleFunc("GET /{$}", page(fixtureHome))
	mux.HandleFunc("GET /menu", page(`<!doctype html><title>Menu</title><h1>Menu</h1><p>Soup of the day</p>`))
	mux.HandleFunc("GET /login", page(fixtureLogin))
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Results</title><p>Results for %s</p>`, r.URL.Query().Get("q"))
	})
	mux.HandleFunc("POST /reserve", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.reserved = append(f.reserved, r.PostForm.Get("name")+"/"+r.PostForm.Get("party"))
		f.mu.Unlock()
		fmt.Fprint(w, `<!doctype html><title>Reserved</title><p>Your table is reserved.</p>`)
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.sessions = append(f.sessions, r.PostForm.Get("email")+":"+r.PostForm.Get("pw"))
		f.mu.Unlock()
		fmt.Fprint(w, `<!doctype html><title>Welcome</title><p>Signed in.</p>`)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type mapCreds map[string]integration.Credential

func (m mapCreds) Get(_ context.Context, kind string) (integration.Credential, error) {
	if c, ok := m[kind]; ok {
		return c, nil
	}
	return integration.Credential{}, integration.ErrNoCredential
}

// liveBrowser returns an initialised integration with allow_private (the
// fixture is on loopback), or skips the test when no Chrome is installed.
func liveBrowser(t *testing.T, extra string, creds integration.CredentialSource) *Integration {
	t.Helper()
	if testing.Short() {
		t.Skip("live browser test skipped in -short mode")
	}
	path, err := FindChrome("")
	if err != nil {
		t.Skipf("no Chrome/Chromium on this machine: %v", err)
	}
	settings := `{"allow_private":true`
	if runtime.GOOS == "linux" {
		// Ubuntu 23.10+ restricts the unprivileged user namespaces that
		// Chrome's sandbox needs (CI runners included).
		settings += `,"no_sandbox":true`
	}
	settings += extra + `}`
	in := New()
	in.lookPath = func() (string, error) { return path, nil }
	if err := in.Init(context.Background(), integration.Config{Name: "browser", DataDir: t.TempDir(),
		Settings: json.RawMessage(settings)}, creds); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close() })
	return in
}

func call(t *testing.T, ctx context.Context, h integration.Handler, args string) (integration.Result, error) {
	t.Helper()
	return h(ctx, json.RawMessage(args))
}

func mustCall(t *testing.T, ctx context.Context, h integration.Handler, args string) string {
	t.Helper()
	res, err := call(t, ctx, h, args)
	if err != nil {
		t.Fatalf("call %s: %v", args, err)
	}
	if !res.Untrusted {
		t.Errorf("result for %s not marked untrusted", args)
	}
	return res.Content
}

// refOf finds the ref of the line containing label in a read/find output.
func refOf(t *testing.T, text, label string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, label) && strings.HasPrefix(l, "[ref_") {
			return l[1:strings.Index(l, "]")]
		}
	}
	t.Fatalf("no element %q in:\n%s", label, text)
	return ""
}

func TestLiveBrowserFlow(t *testing.T) {
	f := newFixture(t)
	creds := mapCreds{"login:127.0.0.1": {Kind: "login:127.0.0.1", Secret: "s3cret-pw", Fields: map[string]string{"username": "ada@example.com"}}}
	in := liveBrowser(t, "", creds)
	ctx := integration.WithConversationID(integration.WithUserID(context.Background(), 1), "conv_live")
	site := `"site":"127.0.0.1"`

	// open + read: refs, headings, collapsed text, no hidden text.
	text := mustCall(t, ctx, in.handleOpen, `{"url":"`+f.server.URL+`/"}`)
	for _, want := range []string{"Page: Fixture Bistro", "Site: 127.0.0.1", "# Fixture Bistro", "Welcome to our test restaurant.",
		`link "See the menu" -> /menu`, `button "Reserve a table"`, "(submits a form)", `textbox "Search dishes"`,
		`combobox "Party size" value="Two people" options=["Two people","Four people"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("open output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hidden text") || strings.Contains(text, "csrf") {
		t.Errorf("hidden content leaked:\n%s", text)
	}
	again := mustCall(t, ctx, in.handleRead, `{}`)
	if refOf(t, again, "See the menu") != refOf(t, text, "See the menu") {
		t.Error("refs are not stable across reads")
	}

	// find
	found := mustCall(t, ctx, in.handleFind, `{"query":"reserve table"}`)
	if !strings.Contains(found, "Reserve a table") || strings.Contains(found, "See the menu") {
		t.Errorf("find = %q", found)
	}

	// The submit gate.
	for _, label := range []string{"Reserve a table", "Delete account"} {
		_, err := call(t, ctx, in.handleClick, `{`+site+`,"ref":"`+refOf(t, text, label)+`"}`)
		if err == nil || !strings.Contains(err.Error(), "browser.submit") {
			t.Errorf("click %q: err = %v", label, err)
		}
	}
	// Site must match the page.
	if _, err := call(t, ctx, in.handleClick, `{"site":"example.com","ref":"`+refOf(t, text, "Show details")+`"}`); err == nil ||
		!strings.Contains(err.Error(), `pass site="127.0.0.1"`) {
		t.Errorf("site mismatch: %v", err)
	}
	// A plain button works; alerts are dismissed and reported.
	mustCall(t, ctx, in.handleClick, `{`+site+`,"ref":"`+refOf(t, text, "Show details")+`"}`)
	if r := mustCall(t, ctx, in.handleRead, `{}`); !strings.Contains(r, "details opened") {
		t.Errorf("click had no effect:\n%s", r)
	}
	if r := mustCall(t, ctx, in.handleClick, `{`+site+`,"ref":"`+refOf(t, text, "Say hello")+`"}`); !strings.Contains(r, "hello from the page") {
		t.Errorf("dialog not reported: %s", r)
	}

	// type + select, then press Enter in a POST form is refused.
	nameRef := refOf(t, text, `textbox "Name"`)
	mustCall(t, ctx, in.handleType, `{`+site+`,"ref":"`+nameRef+`","text":"Ada"}`)
	if _, err := call(t, ctx, in.handleType, `{`+site+`,"ref":"`+nameRef+`","text":"Ada","press_enter":true}`); err == nil ||
		!strings.Contains(err.Error(), "Enter") {
		t.Errorf("press_enter in a POST form: %v", err)
	}
	mustCall(t, ctx, in.handleSelect, `{`+site+`,"ref":"`+refOf(t, text, "Party size")+`","value":"four"}`)
	r := mustCall(t, ctx, in.handleRead, `{}`)
	if !strings.Contains(r, `value="Ada"`) || !strings.Contains(r, `value="Four people"`) {
		t.Errorf("form state not shown:\n%s", r)
	}

	// Approval preview + details for the submit; then the submit itself.
	submit := `{` + site + `,"ref":"` + refOf(t, text, "Reserve a table") + `","irreversible":true,"summary":"reserve for Ada"}`
	png, mime, err := in.Preview(ctx, "browser.submit", json.RawMessage(submit))
	if err != nil || mime != "image/png" || len(png) < 1000 || string(png[1:4]) != "PNG" {
		t.Fatalf("Preview = %d bytes %q, %v", len(png), mime, err)
	}
	det, err := in.ApprovalDetails(ctx, "browser.submit", json.RawMessage(submit))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"method":"post"`, `"name":"name"`, `"value":"Ada"`, `"value":"Four people"`, `"hidden_fields":1`, `Reserve a table`} {
		if !strings.Contains(string(det), want) {
			t.Errorf("details lack %s: %s", want, det)
		}
	}
	if f.reservedCount() != 0 {
		t.Fatal("preview submitted the form")
	}
	if r := mustCall(t, ctx, in.handleSubmit, submit); !strings.Contains(r, "Reserved") {
		t.Errorf("submit result = %s", r)
	}
	if got := f.reservedList(); len(got) != 1 || got[0] != "Ada/4" {
		t.Errorf("reservations = %v", got)
	}

	// back, then a link with target=_blank stays in this tab.
	back := mustCall(t, ctx, in.handleBack, `{`+site+`}`)
	if !strings.Contains(back, "Fixture Bistro") {
		t.Errorf("back = %s", back)
	}
	mustCall(t, ctx, in.handleClick, `{`+site+`,"ref":"`+refOf(t, back, "Menu in a new tab")+`"}`)
	if r := mustCall(t, ctx, in.handleRead, `{}`); !strings.Contains(r, "Soup of the day") {
		t.Errorf("target=_blank left the tab:\n%s", r)
	}

	// Search box: Enter in a GET form is fine.
	home := mustCall(t, ctx, in.handleOpen, `{"url":"`+f.server.URL+`/"}`)
	mustCall(t, ctx, in.handleType, `{`+site+`,"ref":"`+refOf(t, home, "Search dishes")+`","text":"soup","press_enter":true}`)
	if r := mustCall(t, ctx, in.handleRead, `{}`); !strings.Contains(r, "Results for soup") {
		t.Errorf("search:\n%s", r)
	}

	// Login: fills without returning the secret, never submits, and the
	// password never appears in a read.
	lp := mustCall(t, ctx, in.handleOpen, `{"url":"`+f.server.URL+`/login"}`)
	if _, err := call(t, ctx, in.handleType, `{`+site+`,"ref":"`+refOf(t, lp, `textbox "Password"`)+`","text":"x"}`); err == nil {
		t.Error("typing into a password field was allowed")
	}
	res, err := call(t, ctx, in.handleLogin, `{`+site+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "s3cret") || strings.Contains(res.Content, "ada@") {
		t.Errorf("login result leaks the credential: %s", res.Content)
	}
	r = mustCall(t, ctx, in.handleRead, `{}`)
	if strings.Contains(r, "s3cret") || !strings.Contains(r, "value=(hidden)") || !strings.Contains(r, `value="ada@example.com"`) {
		t.Errorf("read after login:\n%s", r)
	}
	if len(f.sessionList()) != 0 {
		t.Error("login submitted the form")
	}
	mustCall(t, ctx, in.handleSubmit, `{`+site+`,"ref":"`+refOf(t, lp, `button "Sign in"`)+`","irreversible":true,"summary":"sign in"}`)
	if got := f.sessionList(); len(got) != 1 || got[0] != "ada@example.com:s3cret-pw" {
		t.Errorf("sessions = %v", got)
	}
	// No credential for another site.
	if _, _, err := in.loginFor(ctx, "example.com"); err == nil || !strings.Contains(err.Error(), "bosun credentials set browser login:example.com") {
		t.Errorf("missing login: %v", err)
	}

	// Screenshot, status, step screenshots on disk.
	sres, err := call(t, ctx, in.handleScreenshot, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := sres.Data.(StepData); !ok || len(d.PNG) < 1000 {
		t.Errorf("screenshot data = %T", sres.Data)
	}
	st := in.UserStatus(1)
	if !st.Running || !st.Active || st.Steps < 10 || !st.HasScreenshot || !strings.Contains(st.URL, "/session") {
		t.Errorf("status = %+v", st)
	}
	if fs := stepFiles(filepath.Join(in.dataDir, "steps", "u1", "conv_live")); len(fs) < 10 {
		t.Errorf("step screenshots = %d", len(fs))
	}
	if len(sres.AuditDetail) == 0 || !strings.Contains(string(sres.AuditDetail), "screenshot") {
		t.Errorf("audit detail = %s", sres.AuditDetail)
	}
	if b, err := in.LatestScreenshot(context.Background(), 1); err != nil || len(b) < 1000 {
		t.Errorf("LatestScreenshot = %d, %v", len(b), err)
	}

	// Stop kills the browser (its whole process group); the next read has
	// no page.
	sess := in.existing(1)
	sess.cmdMu.Lock()
	pid := sess.cmd.Process.Pid
	sess.cmdMu.Unlock()
	in.StopUser(1)
	if processAlive(pid) {
		t.Errorf("Chromium (pid %d) still running after stop", pid)
	}
	if in.UserStatus(1).Running {
		t.Error("still running after stop")
	}
	if _, err := call(t, ctx, in.handleRead, `{}`); err == nil || !strings.Contains(err.Error(), "browser.open") {
		t.Errorf("read after stop: %v", err)
	}
	if _, err := in.LatestScreenshot(context.Background(), 1); !errors.Is(err, ErrNoScreenshot) {
		t.Errorf("screenshot after stop: %v", err)
	}
}

func TestLiveGuardAndSchemes(t *testing.T) {
	f := newFixture(t)
	ctx := integration.WithUserID(context.Background(), 2)
	// Without allow_private, loopback is refused up front ...
	in := liveBrowser(t, `,"allow_private":false`, nil)
	if _, err := call(t, ctx, in.handleOpen, `{"url":"`+f.server.URL+`/"}`); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("open loopback: %v", err)
	}
	for _, u := range []string{"file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,hi"} {
		if _, err := call(t, ctx, in.handleOpen, `{"url":"`+u+`"}`); err == nil {
			t.Errorf("open %s succeeded", u)
		}
	}
	// ... and the proxy blocks it even when the early check is skipped
	// (a page redirecting or linking to the network).
	px, err := newGuardProxy(false)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	pu, _ := url.Parse(px.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := client.Get(f.server.URL + "/menu")
	if err == nil {
		if resp.StatusCode < 400 {
			t.Errorf("proxied loopback request: HTTP %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	// In the real browser, the guard proxy refuses loopback even when a
	// navigation skips the early check.
	s, _, err := in.session(3)
	if err != nil {
		t.Fatal(err)
	}
	actx, cancel := s.actx(context.Background(), 30*time.Second)
	defer cancel()
	err = chromedp.Do(actx, chromedp.Navigate(f.server.URL+"/menu"))
	var le *chromedp.LoadError
	if !errors.As(err, &le) {
		st, _ := s.live(actx)
		t.Errorf("navigation to loopback through the proxy: err = %v, page = %+v", err, st)
	}
	if err := s.checkScheme(actx); err == nil || !strings.Contains(err.Error(), "could not be loaded") {
		t.Errorf("checkScheme after a blocked load: %v", err)
	}
	// The browser's own profile is per user and not shared.
	if p := in.profileDir(2); !strings.HasSuffix(p, filepath.Join("profiles", "u2")) {
		t.Errorf("profile dir = %s", p)
	}
	if _, err := os.Stat(in.profileDir(2)); err == nil {
		// Never launched for user 2 (the open was refused before launch).
		t.Errorf("profile created for a refused open")
	}
}

func (f *fixture) reservedCount() int { return len(f.reservedList()) }

func (f *fixture) reservedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reserved...)
}

func (f *fixture) sessionList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sessions...)
}

func processAlive(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return false
		}
		// A killed child stays a zombie until reaped; chromedp reaps it.
		time.Sleep(100 * time.Millisecond)
	}
	return true
}
