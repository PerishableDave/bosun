package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/webui"
)

// With a web UI mounted, / serves it while /v1 behaves exactly as before.
func TestWebUIMount(t *testing.T) {
	s, err := New(Options{Auth: setup(t).auth, Bus: events.NewBus(), WebUI: webui.Handler()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(p string) *http.Response {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	if r := get("/"); r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("/ = %d %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
	if r := get("/v1/health"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("/v1/health = %d %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get("/v1/me"); r.StatusCode != 401 {
		t.Errorf("/v1/me unauthenticated = %d, want 401", r.StatusCode)
	}
	if r := get("/v1/nope"); r.StatusCode != 404 || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("/v1/nope = %d %q, want JSON 404", r.StatusCode, r.Header.Get("Content-Type"))
	}
}
