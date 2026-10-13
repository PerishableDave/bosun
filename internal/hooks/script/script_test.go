package script

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/llm"
)

type memAudit struct {
	mu sync.Mutex
	es []audit.Entry
}

func (m *memAudit) Record(_ context.Context, e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.es = append(m.es, e)
	return nil
}

func (m *memAudit) entries() []audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]audit.Entry(nil), m.es...)
}

// writeScript writes an executable /bin/sh script whose stdin is saved to
// <dir>/in.json before body runs, and returns its path and the dir.
func writeScript(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hook.sh")
	src := "#!/bin/sh\ncat > '" + filepath.Join(dir, "in.json") + "'\n" + body + "\n"
	if err := os.WriteFile(p, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, dir
}

func newHook(t *testing.T, point hooks.Point, body string, mod ...func(*Spec)) (*Hook, string, *memAudit) {
	t.Helper()
	p, dir := writeScript(t, body)
	spec := Spec{Point: point, Command: []string{p}, Timeout: 5 * time.Second}
	for _, m := range mod {
		m(&spec)
	}
	aud := &memAudit{}
	h, err := New(spec, 0, Options{Audit: aud})
	if err != nil {
		t.Fatal(err)
	}
	return h, dir, aud
}

func readInput(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "in.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("input %s: %v", b, err)
	}
	return m
}

var call = hooks.ToolCall{UserID: 7, ID: "tu1", Tool: "email.send", Integration: "email", Capability: "email.send",
	Risk: integration.RiskWrite, Args: json.RawMessage(`{"to":"a@b.example"}`)}

func TestPreToolContinue(t *testing.T) {
	h, dir, aud := newHook(t, hooks.PointPreToolCall, `printf '{"action":"continue"}'`)
	r, err := h.PreToolCall(context.Background(), call)
	if err != nil || r.Cancel || r.Args != nil {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
	in := readInput(t, dir)
	if in["point"] != "pre_tool_call" || in["user_id"] != 7.0 || in["tool"] != "email.send" || in["risk"] != "write" ||
		in["args"].(map[string]any)["to"] != "a@b.example" {
		t.Errorf("input = %v", in)
	}
	if len(aud.entries()) != 0 {
		t.Error("success audited as failure")
	}
}

func TestPreToolCancel(t *testing.T) {
	h, _, _ := newHook(t, hooks.PointPreToolCall, `printf '{"action":"cancel","reason":"no mail today"}'`)
	r, err := h.PreToolCall(context.Background(), call)
	if err != nil || !r.Cancel || r.Reason != "no mail today" {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
}

func TestPreToolRewrite(t *testing.T) {
	h, _, _ := newHook(t, hooks.PointPreToolCall, `printf '{"action":"continue","args":{"to":"safe@example.com"}}'`)
	r, err := h.PreToolCall(context.Background(), call)
	if err != nil || string(r.Args) != `{"to":"safe@example.com"}` {
		t.Fatalf("r = %+v (%s), err = %v", r, r.Args, err)
	}
}

func TestFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"bad json":       {`printf 'not json'`, "invalid output"},
		"empty":          {`true`, "invalid output"},
		"trailing":       {`printf '{"action":"continue"} {}'`, "trailing"},
		"missing action": {`printf '{}'`, "action must be"},
		"exit":           {`printf '{"action":"continue"}'; exit 3`, "exit status 3"},
		"timeout":        {`sleep 5`, "timed out"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, aud := newHook(t, hooks.PointPreToolCall, tc.body, func(s *Spec) { s.Timeout = 300 * time.Millisecond })
			start := time.Now()
			_, err := h.PreToolCall(context.Background(), call)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("took %s", d)
			}
			es := aud.entries()
			if len(es) != 1 || es[0].Kind != audit.KindHook || es[0].UserID != 7 || es[0].Tool != "email.send" ||
				!strings.Contains(es[0].Error, tc.want) {
				t.Errorf("audit = %+v", es)
			}
		})
	}
}

func TestMatchGlob(t *testing.T) {
	h, dir, _ := newHook(t, hooks.PointPreToolCall, `printf '{"action":"cancel"}'`, func(s *Spec) { s.Match = "email.*" })
	other := call
	other.Tool, other.Capability = "notes.read", "notes.read"
	r, err := h.PreToolCall(context.Background(), other)
	if err != nil || r.Cancel {
		t.Fatalf("non-matching: r = %+v, err = %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "in.json")); !os.IsNotExist(err) {
		t.Error("script ran for a non-matching tool")
	}
	if r, err := h.PreToolCall(context.Background(), call); err != nil || !r.Cancel {
		t.Fatalf("matching: r = %+v, err = %v", r, err)
	}
	// The capability alone may match.
	byCap := call
	byCap.Tool = "mail_alias.send"
	if r, err := h.PreToolCall(context.Background(), byCap); err != nil || !r.Cancel {
		t.Fatalf("capability match: r = %+v, err = %v", r, err)
	}
}

func TestPostToolCall(t *testing.T) {
	h, dir, _ := newHook(t, hooks.PointPostToolCall, `printf '{"result":"[redacted]"}'`)
	r, err := h.PostToolCall(context.Background(), call, integration.Result{Content: "secret", Untrusted: true, Data: "x"})
	if err != nil || r.Result == nil || r.Result.Content != "[redacted]" || !r.Result.Untrusted || r.Result.Data != nil {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
	if res := readInput(t, dir)["result"].(map[string]any); res["content"] != "secret" || res["untrusted"] != true {
		t.Errorf("result input = %v", res)
	}

	h, _, _ = newHook(t, hooks.PointPostToolCall, `printf '{}'`)
	if r, err := h.PostToolCall(context.Background(), call, integration.Result{Content: "ok"}); err != nil || r.Result != nil {
		t.Fatalf("no-op: r = %+v, err = %v", r, err)
	}
	h, _, _ = newHook(t, hooks.PointPostToolCall, `printf '{"result":5}'`)
	if _, err := h.PostToolCall(context.Background(), call, integration.Result{Content: "ok"}); err == nil {
		t.Fatal("non-string result accepted")
	}
}

func TestPreModelCall(t *testing.T) {
	h, dir, _ := newHook(t, hooks.PointPreModelCall,
		`printf '{"system":"new system","messages":[{"role":"user","content":[{"type":"text","text":"clean"}]}]}'`)
	reg := hooks.NewRegistry()
	if err := h.Register(reg); err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: "m", System: "old", Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "dirty")}}
	if err := reg.RunPreModelCall(context.Background(), 3, &req); err != nil {
		t.Fatal(err)
	}
	if req.System != "new system" || len(req.Messages) != 1 || req.Messages[0].Text() != "clean" {
		t.Errorf("req = %+v", req)
	}
	in := readInput(t, dir)
	if in["user_id"] != 3.0 || in["system"] != "old" || in["model"] != "m" || len(in["messages"].([]any)) != 1 {
		t.Errorf("input = %v", in)
	}
	if got := reg.List(); len(got) != 1 || got[0].Type != hooks.TypeScript || got[0].Point != hooks.PointPreModelCall {
		t.Errorf("List = %+v", got)
	}

	h, _, aud := newHook(t, hooks.PointPreModelCall, `printf '{"messages":[{"role":"system"}]}'`)
	req = llm.Request{System: "keep", Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "x")}}
	if err := h.PreModelCall(context.Background(), &req); err == nil || req.System != "keep" {
		t.Fatalf("bad role accepted: %v", err)
	}
	if len(aud.entries()) != 1 {
		t.Error("failure not audited")
	}
}

func TestOnApproval(t *testing.T) {
	h, dir, aud := newHook(t, hooks.PointOnApproval, `exit 0`)
	a := approval.Approval{ID: "appr_1", UserID: 2, Tool: "email.send", Capability: "email.send", Risk: integration.RiskWrite,
		Args: json.RawMessage(`{}`), Status: approval.StatusApproved}
	h.OnApproval(context.Background(), a, approval.Approve)
	in := readInput(t, dir)
	if in["decision"] != "approve" || in["approval"].(map[string]any)["id"] != "appr_1" || in["user_id"] != 2.0 {
		t.Errorf("input = %v", in)
	}
	if len(aud.entries()) != 0 {
		t.Error("success audited")
	}
	h, _, aud = newHook(t, hooks.PointOnApproval, `exit 1`)
	h.OnApproval(context.Background(), a, approval.Deny)
	if es := aud.entries(); len(es) != 1 || es[0].Kind != audit.KindHook {
		t.Errorf("audit = %+v", es)
	}
}

func TestMinimalEnvironment(t *testing.T) {
	t.Setenv("BOSUN_TEST_SECRET", "hunter2")
	h, dir, _ := newHook(t, hooks.PointOnApproval, `env > "$(dirname "$0")/env.txt"`)
	h.OnApproval(context.Background(), approval.Approval{UserID: 1}, approval.Approve)
	b, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	if strings.Contains(env, "hunter2") || !strings.Contains(env, "PATH=") || !strings.Contains(env, "HOME=") {
		t.Errorf("env = %s", env)
	}
}

func TestNewValidation(t *testing.T) {
	for name, s := range map[string]Spec{
		"point":    {Point: hooks.PointOnSchedule, Command: []string{"/bin/true"}},
		"relative": {Point: hooks.PointPreToolCall, Command: []string{"true"}},
		"empty":    {Point: hooks.PointPreToolCall},
		"match":    {Point: hooks.PointPreModelCall, Command: []string{"/bin/true"}, Match: "x*"},
		"glob":     {Point: hooks.PointPreToolCall, Command: []string{"/bin/true"}, Match: "[x"},
	} {
		if _, err := New(s, 0, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h, err := New(Spec{Point: hooks.PointPreToolCall, Command: []string{"/usr/local/bin/Bosun Redact"}}, 1, Options{})
	if err != nil || h.Name() != "script-2-bosun-redact" || h.Spec().Timeout != DefaultTimeout {
		t.Errorf("h = %+v, err = %v", h, err)
	}
	reg := hooks.NewRegistry()
	hs, err := Register(reg, []Spec{
		{Point: hooks.PointPreToolCall, Command: []string{"/bin/true"}},
		{Point: hooks.PointPostToolCall, Command: []string{"/bin/true"}},
		{Point: hooks.PointOnApproval, Command: []string{"/bin/true"}, Name: "notify"},
	}, Options{})
	if err != nil || len(hs) != 3 || len(reg.PreToolCalls()) != 1 || len(reg.PostToolCalls()) != 1 || len(reg.OnApprovals()) != 1 {
		t.Fatalf("Register: %v", err)
	}
	if _, err := Register(reg, []Spec{{Point: hooks.PointOnApproval, Command: []string{"/bin/true"}, Name: "notify"}}, Options{}); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestCanceledContext(t *testing.T) {
	h, _, _ := newHook(t, hooks.PointPreToolCall, `printf '{"action":"continue"}'`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.PreToolCall(ctx, call); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
