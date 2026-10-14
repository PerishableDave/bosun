package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/hooks"
	"github.com/perishabledave/bosun/internal/hooks/builtin"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

func TestBuildHooks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "approved")
	scriptPath := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\ncat > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	parse := func(y string) *config.Config {
		t.Helper()
		cfg, err := config.Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	scripts := "    scripts:\n      - { point: on_approval, command: [" + scriptPath + "] }\n      - { point: pre_tool_call, command: [/bin/true], match: \"email.*\" }\n"

	// Defaults: redaction only.
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	reg, err := buildHooks(parse(""), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if l := reg.List(); len(l) != 1 || l[0].Name != builtin.RedactSecretsName || l[0].Point != hooks.PointPreModelCall {
		t.Errorf("default hooks = %+v", l)
	}

	// Scripts configured but disabled: not registered.
	buf.Reset()
	reg, err = buildHooks(parse("hooks:\n  redact_secrets: false\n  script_hooks:\n    enabled: false\n"+scripts), nil, log)
	if err != nil || len(reg.List()) != 0 || !strings.Contains(buf.String(), "configured but") {
		t.Errorf("disabled: %+v, %v\n%s", reg.List(), err, buf.String())
	}

	// Enabled: registered and logged loudly.
	buf.Reset()
	reg, err = buildHooks(parse("hooks:\n  script_hooks:\n    enabled: true\n"+scripts), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	l := reg.List()
	if len(l) != 3 || l[1].Type != hooks.TypeScript || l[1].Point != hooks.PointOnApproval || l[2].Point != hooks.PointPreToolCall {
		t.Errorf("enabled hooks = %+v", l)
	}
	if n := strings.Count(buf.String(), "level=WARN msg=\"script hook enabled"); n != 2 {
		t.Errorf("want 2 WARN lines, got %d:\n%s", n, buf.String())
	}

	// on_approval runs in the background via the approval manager callback.
	onApprovalHooks(reg)(context.Background(), approval.Approval{ID: "appr_x", UserID: 1}, approval.Approve)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil && strings.Contains(string(b), "appr_x") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("on_approval script did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonHooksEndpointAndRedaction(t *testing.T) {
	h := newHarness(t)
	alice := h.login("alice", "alice-password") // admin
	bob := h.login("bob", "bob-password")       // member

	var hl api.HookListResponse
	if c := h.do("GET", "/v1/hooks", alice, nil, &hl); c != 200 {
		t.Fatalf("GET /v1/hooks = %d", c)
	}
	if pm := hl.Hooks[hooks.PointPreModelCall]; len(pm) != 1 || pm[0].Name != builtin.RedactSecretsName || pm[0].Type != "go" {
		t.Errorf("hooks = %+v", hl)
	}
	if c := h.do("GET", "/v1/hooks", bob, nil, nil); c != 403 {
		t.Errorf("member GET /v1/hooks = %d", c)
	}

	conv := h.newConversation(alice)
	h.llm.Push(fake.Text("noted"))
	h.mem.Push(fake.Text(`{"memories":[]}`))
	h.turn(alice, conv, "my key is sk-ant-api03-TOPSECRETVALUE please keep it", nil)
	reqs := h.llm.Requests()
	sent := reqs[len(reqs)-1].Messages
	if got := sent[len(sent)-1].Text(); strings.Contains(got, "TOPSECRETVALUE") || !strings.Contains(got, "[REDACTED:anthropic-key]") {
		t.Errorf("sent to model: %q", got)
	}
}
