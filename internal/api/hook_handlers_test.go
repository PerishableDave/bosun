package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/llm"
)

func TestListHooks(t *testing.T) {
	e := setup(t)
	admin := e.login(t)

	// Without a registry: 501 for admins.
	if resp := e.do(t, "GET", "/v1/hooks", admin, ""); resp.StatusCode != 501 {
		t.Errorf("no registry = %d", resp.StatusCode)
	}

	reg := hooks.NewRegistry()
	_ = reg.AddPreModelCall("builtin.redact_secrets", func(context.Context, *llm.Request) error { return nil })
	_ = reg.AddPreModelCall("script-1-scrub", func(context.Context, *llm.Request) error { return nil }, hooks.AsType(hooks.TypeScript))
	s, err := New(Options{Auth: e.auth, Bus: e.bus, Hooks: reg})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e.srv = srv

	if _, err := e.auth.CreateUser(context.Background(), "kid", "longenough", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	sess, err := e.auth.LoginWithLabel(context.Background(), "kid", "longenough", "t")
	if err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "GET", "/v1/hooks", sess.Token, ""); resp.StatusCode != 403 || errCode(t, resp) != "forbidden" {
		t.Errorf("member = %d", resp.StatusCode)
	}

	resp := e.do(t, "GET", "/v1/hooks", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("admin = %d", resp.StatusCode)
	}
	var got HookListResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	pm := got.Hooks[hooks.PointPreModelCall]
	if len(got.Hooks) != 4 || len(pm) != 2 || pm[0] != (HookResponse{"builtin.redact_secrets", "go"}) ||
		pm[1] != (HookResponse{"script-1-scrub", "script"}) || got.Hooks[hooks.PointPreToolCall] == nil {
		t.Errorf("hooks = %+v", got)
	}
}
