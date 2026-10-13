package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/memory"
	"github.com/dperedo/bosun/internal/store"
)

func setupMemory(t *testing.T) (e *env, aliceTok, bobTok string) {
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
	tok := func(name string) string {
		if _, err := a.CreateUser(ctx, name, "longenough", auth.RoleMember); err != nil {
			t.Fatal(err)
		}
		s, err := a.LoginWithLabel(ctx, name, "longenough", "test")
		if err != nil {
			t.Fatal(err)
		}
		return s.Token
	}
	aliceTok, bobTok = tok("alice"), tok("bob")
	s, err := New(Options{Auth: a, Bus: events.NewBus(), Version: "test", Memories: memory.NewStore(st)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, auth: a}, aliceTok, bobTok
}

func TestMemoryEndpoints(t *testing.T) {
	e, alice, bob := setupMemory(t)

	resp := e.do(t, "POST", "/v1/memory", alice, `{"kind":"preference","content":"  Prefers metric units. "}`)
	var m MemoryResponse
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 201 || m.ID == 0 || m.Kind != "preference" || m.Content != "Prefers metric units." || m.Source != "user" || m.CreatedAt.IsZero() {
		t.Fatalf("create = %d %+v", resp.StatusCode, m)
	}
	resp = e.do(t, "POST", "/v1/memory", alice, `{"kind":"note","content":"Gate code is 4512."}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create note = %d", resp.StatusCode)
	}
	for _, body := range []string{`{"kind":"secret","content":"x"}`, `{"kind":"note","content":"  "}`, `{"kind":"note"}`, `{"kind":"note","content":"x","extra":1}`} {
		if resp := e.do(t, "POST", "/v1/memory", alice, body); resp.StatusCode != 400 {
			t.Errorf("create %s = %d", body, resp.StatusCode)
		}
	}

	list := func(tok, query string) (int, []MemoryResponse) {
		t.Helper()
		resp := e.do(t, "GET", "/v1/memory"+query, tok, "")
		var l MemoryListResponse
		if resp.StatusCode == 200 {
			if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
				t.Fatal(err)
			}
			if l.Memories == nil {
				t.Errorf("memories is null, want []")
			}
		}
		return resp.StatusCode, l.Memories
	}
	if c, l := list(alice, ""); c != 200 || len(l) != 2 || l[0].Kind != "note" {
		t.Errorf("list = %d %+v", c, l)
	}
	if c, l := list(alice, "?kind=preference"); c != 200 || len(l) != 1 || l[0].ID != m.ID {
		t.Errorf("list kind = %d %+v", c, l)
	}
	if c, l := list(alice, "?q=metric"); c != 200 || len(l) != 1 || l[0].ID != m.ID {
		t.Errorf("search = %d %+v", c, l)
	}
	if c, l := list(alice, "?q=metric&kind=note"); c != 200 || len(l) != 0 {
		t.Errorf("search+kind = %d %+v", c, l)
	}
	if c, l := list(alice, `?q=%22gate%20AND%20(`); c != 200 || len(l) != 0 {
		t.Errorf("search with FTS syntax = %d %+v", c, l)
	}
	if c, l := list(alice, "?limit=1&offset=1"); c != 200 || len(l) != 1 || l[0].ID != m.ID {
		t.Errorf("paging = %d %+v", c, l)
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?limit=x", "?offset=-1", "?kind=secret"} {
		if c, _ := list(alice, q); c != 400 {
			t.Errorf("list %s = %d", q, c)
		}
	}

	// Bob sees and touches nothing of alice's.
	if c, l := list(bob, ""); c != 200 || len(l) != 0 {
		t.Errorf("bob list = %d %+v", c, l)
	}
	if c, l := list(bob, "?q=metric"); c != 200 || len(l) != 0 {
		t.Errorf("bob search = %d %+v", c, l)
	}
	path := fmt.Sprintf("/v1/memory/%d", m.ID)
	if resp := e.do(t, "PUT", path, bob, `{"content":"pwned"}`); resp.StatusCode != 404 || errCode(t, resp) != "not_found" {
		t.Errorf("bob update = %d", resp.StatusCode)
	}
	if resp := e.do(t, "DELETE", path, bob, ""); resp.StatusCode != 404 {
		t.Errorf("bob delete = %d", resp.StatusCode)
	}

	resp = e.do(t, "PUT", path, alice, `{"content":"Prefers imperial units."}`)
	var up MemoryResponse
	json.NewDecoder(resp.Body).Decode(&up)
	if resp.StatusCode != 200 || up.Content != "Prefers imperial units." || up.Kind != "preference" || up.UpdatedAt.Before(m.UpdatedAt) {
		t.Errorf("update = %d %+v", resp.StatusCode, up)
	}
	if resp := e.do(t, "PUT", path, alice, `{"content":""}`); resp.StatusCode != 400 {
		t.Errorf("empty update = %d", resp.StatusCode)
	}
	for _, p := range []string{"/v1/memory/abc", "/v1/memory/0", "/v1/memory/99999"} {
		if resp := e.do(t, "DELETE", p, alice, ""); resp.StatusCode != 404 {
			t.Errorf("delete %s = %d", p, resp.StatusCode)
		}
	}
	if resp := e.do(t, "DELETE", path, alice, ""); resp.StatusCode != 204 {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp := e.do(t, "DELETE", path, alice, ""); resp.StatusCode != 404 {
		t.Errorf("second delete = %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/v1/memory", "", ""); resp.StatusCode != 401 {
		t.Errorf("unauthenticated = %d", resp.StatusCode)
	}
}

func TestMemoryNotConfigured(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	if resp := e.do(t, "GET", "/v1/memory", tok, ""); resp.StatusCode != 501 {
		t.Errorf("GET without memory = %d", resp.StatusCode)
	}
}
