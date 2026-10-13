package webui

import (
	"net/http"
	"net/http/httptest"
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

func TestStaticContentTypes(t *testing.T) {
	for path, want := range map[string]string{
		"/static/app.js":    "text/javascript; charset=utf-8",
		"/static/style.css": "text/css; charset=utf-8",
	} {
		rec := get(t, Handler(), "GET", path)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != want {
			t.Errorf("%s: status %d, type %q, want %q", path, rec.Code, rec.Header().Get("Content-Type"), want)
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
