package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestIndexNoStore(t *testing.T) {
	rec := get(t, Handler(), "GET", "/")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "/static/app.js") {
		t.Error("index does not reference app.js")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing CSP")
	}
}

func TestIndexReferencesAssets(t *testing.T) {
	body := get(t, Handler(), "GET", "/").Body.String()
	for _, ref := range []string{`src="/static/theme.js"`, `href="/static/style.css"`, `type="module" src="/static/app.js"`} {
		if !strings.Contains(body, ref) {
			t.Errorf("index.html lacks %s", ref)
		}
	}
}

// TestStaticContentTypes serves every embedded asset and checks its type,
// caching and that it is not empty.
func TestStaticContentTypes(t *testing.T) {
	want := map[string]string{
		".js":  "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8",
	}
	files, err := fs.Glob(embedded, "web/static/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("only %d static files embedded", len(files))
	}
	h := Handler()
	for _, f := range files {
		p := "/" + strings.TrimPrefix(f, "web/")
		rec := get(t, h, "GET", p)
		ct := rec.Header().Get("Content-Type")
		if rec.Code != 200 || ct != want[path.Ext(f)] {
			t.Errorf("%s: status %d, type %q, want %q", p, rec.Code, ct, want[path.Ext(f)])
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s is empty", p)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", p, rec.Header())
		}
	}
}

// TestModuleImportsResolve checks that every relative ES-module import in
// the static JavaScript names an embedded file, so a renamed module cannot
// break the page without a failing test.
func TestModuleImportsResolve(t *testing.T) {
	re := regexp.MustCompile(`(?m)^\s*(?:import|export)\s[^;]*?from\s+"\./([^"]+)"`)
	files, _ := fs.Glob(embedded, "web/static/*.js")
	for _, f := range files {
		b, err := fs.ReadFile(embedded, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			if _, err := fs.Stat(embedded, "web/static/"+string(m[1])); err != nil {
				t.Errorf("%s imports missing ./%s", f, m[1])
			}
		}
	}
}

// TestNoRawHTMLSinks guards the rule that model output reaches innerHTML
// only through md.render: the only innerHTML writers are dom.js's "html"
// prop and the markdown re-render in chat.js.
func TestNoRawHTMLSinks(t *testing.T) {
	allowed := map[string]int{"web/static/dom.js": 1, "web/static/chat.js": 1}
	files, _ := fs.Glob(embedded, "web/static/*.js")
	for _, f := range files {
		b, _ := fs.ReadFile(embedded, f)
		n := strings.Count(string(b), ".innerHTML =")
		if n != allowed[f] {
			t.Errorf("%s: %d innerHTML assignments, want %d", f, n, allowed[f])
		}
		for _, bad := range []string{"insertAdjacentHTML", "outerHTML =", "document.write", "eval("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

func TestNotFoundAndMethods(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/nope", "/static/", "/static/missing.js", "/static/../index.html", "/v1/health", "/static/%2e%2e/index.html"} {
		if rec := get(t, h, "GET", p); rec.Code != 404 && rec.Code != 301 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := get(t, h, "POST", "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d", rec.Code)
	}
}
