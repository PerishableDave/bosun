package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/store"
)

type env struct {
	srv  *httptest.Server
	auth *auth.Service
	bus  *events.Bus
	user *auth.User
}

func setup(t *testing.T) *env {
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
	u, err := a.CreateUser(ctx, "dave", "longenough", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	s, err := New(Options{Auth: a, Bus: bus, Version: "test", Heartbeat: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, auth: a, bus: bus, user: u}
}

func (e *env) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) login(t *testing.T) string {
	t.Helper()
	resp := e.do(t, "POST", "/v1/login", "", `{"username":"dave","password":"longenough","label":"test"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	var lr LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	if lr.Token == "" || lr.User == nil || lr.User.Username != "dave" || lr.ExpiresAt.IsZero() {
		t.Fatalf("login response %+v", lr)
	}
	return lr.Token
}

func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var eb ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return eb.Error.Code
}

func TestHealth(t *testing.T) {
	e := setup(t)
	resp := e.do(t, "GET", "/v1/health", "", "")
	var h HealthResponse
	json.NewDecoder(resp.Body).Decode(&h)
	if resp.StatusCode != 200 || h.Status != "ok" || h.Version != "test" {
		t.Errorf("health = %d %+v", resp.StatusCode, h)
	}
}

func TestLoginAndMe(t *testing.T) {
	e := setup(t)
	resp := e.do(t, "POST", "/v1/login", "", `{"username":"dave","password":"nope-nope"}`)
	if resp.StatusCode != 401 || errCode(t, resp) != "invalid_credentials" {
		t.Errorf("bad login = %d", resp.StatusCode)
	}
	resp = e.do(t, "POST", "/v1/login", "", `{"username":"dave"`)
	if resp.StatusCode != 400 {
		t.Errorf("malformed login = %d", resp.StatusCode)
	}
	resp = e.do(t, "POST", "/v1/login", "", `{"username":"dave","password":"x","extra":1}`)
	if resp.StatusCode != 400 {
		t.Errorf("unknown field = %d", resp.StatusCode)
	}
	tok := e.login(t)

	resp = e.do(t, "GET", "/v1/me", "", "")
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("me without token = %d", resp.StatusCode)
	}
	resp = e.do(t, "GET", "/v1/me", "bsn_wrong", "")
	if resp.StatusCode != 401 {
		t.Errorf("me with bad token = %d", resp.StatusCode)
	}
	resp = e.do(t, "GET", "/v1/me", tok, "")
	var me auth.User
	json.NewDecoder(resp.Body).Decode(&me)
	if resp.StatusCode != 200 || me.ID != e.user.ID || me.Role != auth.RoleAdmin {
		t.Errorf("me = %d %+v", resp.StatusCode, me)
	}

	resp = e.do(t, "POST", "/v1/logout", tok, "")
	if resp.StatusCode != 204 {
		t.Errorf("logout = %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/v1/me", tok, ""); resp.StatusCode != 401 {
		t.Errorf("me after logout = %d", resp.StatusCode)
	}
}

func TestStubs(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/conversations", `{}`, 501},
		{"POST", "/v1/conversations/conv_1/messages", `{"content":"hi"}`, 501},
		{"POST", "/v1/conversations/conv_1/messages", `{}`, 400},
		{"GET", "/v1/approvals", "", 501},
		{"POST", "/v1/approvals/appr_1", `{"decision":"approve"}`, 501},
		{"POST", "/v1/approvals/appr_1", `{"decision":"maybe"}`, 400},
		{"GET", "/v1/nope", "", 404},
	}
	for _, c := range cases {
		if resp := e.do(t, c.method, c.path, tok, c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	// Stubs still require auth.
	if resp := e.do(t, "GET", "/v1/approvals", "", ""); resp.StatusCode != 401 {
		t.Errorf("unauthenticated stub = %d", resp.StatusCode)
	}
}

func TestEventsSSE(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	// Wait for the subscription to be registered (the ": connected" comment).
	if line, _ := br.ReadString('\n'); line != ": connected\n" {
		t.Fatalf("first line %q", line)
	}
	e.bus.Publish(e.user.ID+1, events.Event{Type: events.TypeText, Data: events.TextData{Delta: "not yours"}})
	e.bus.Publish(e.user.ID, events.Event{Type: events.TypeApprovalRequired, Data: events.ApprovalRequiredData{
		ApprovalID: "appr_1", Tool: "email.send", Args: json.RawMessage(`{"to":"a@b"}`), Risk: "write"}})

	var got bytes.Buffer
	deadline := time.After(2 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			line, err := br.ReadString('\n')
			got.WriteString(line)
			if err != nil || strings.HasPrefix(line, "data: ") {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatal("timed out waiting for event")
	}
	out := got.String()
	if !strings.Contains(out, "event: approval_required\n") || !strings.Contains(out, `"approval_id":"appr_1"`) {
		t.Errorf("stream = %q", out)
	}
	if strings.Contains(out, "not yours") {
		t.Error("received another user's event")
	}
	cancel()
	io.Copy(io.Discard, br)
}
