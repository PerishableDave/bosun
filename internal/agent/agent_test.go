package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/dispatch"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/fake"
	"github.com/dperedo/bosun/internal/policy"
	"github.com/dperedo/bosun/internal/store"
)

type writer struct{}

func (writer) Manifest() integration.Manifest {
	return integration.Manifest{Name: "w", Scope: integration.ScopeShared, Capabilities: []integration.Capability{"w.write"}}
}
func (writer) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (writer) Close() error { return nil }
func (writer) Tools() []integration.Tool {
	return []integration.Tool{{Name: "w.write", Capability: "w.write", Risk: integration.RiskWrite,
		Handler: func(context.Context, json.RawMessage) (integration.Result, error) {
			return integration.Result{Content: "ok"}, nil
		}}}
}

type env struct {
	st   *store.Store
	bus  *events.Bus
	llm  *fake.Provider
	user int64
	conv string
	a    *Agent
}

func newEnv(t *testing.T, opts ...Option) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, "dave", "x", "admin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateConversation(ctx, u.ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	tools := integration.NewRegistry()
	if err := tools.Register(writer{}); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	eng := policy.NewEngine(st, policy.Options{Defaults: policy.Defaults{Read: policy.Allow, Write: policy.Ask, Destructive: policy.Deny}})
	appr := approval.NewManager(st, bus, approval.Options{})
	disp := dispatch.New(tools, eng, appr, nil, nil, bus)
	p := fake.New("fake")
	reg := llm.NewRegistry()
	reg.Register(p)
	return &env{st: st, bus: bus, llm: p, user: u.ID, conv: c.ID,
		a: New(reg, "fake/m", disp, tools, st, bus, opts...)}
}

func (e *env) roles(t *testing.T) string {
	t.Helper()
	ms, err := e.st.ListMessages(context.Background(), e.conv)
	if err != nil {
		t.Fatal(err)
	}
	var rs []string
	for _, m := range ms {
		rs = append(rs, m.Role)
	}
	return strings.Join(rs, ",")
}

func collect(sub *events.Subscription) []events.Event {
	var out []events.Event
	for {
		select {
		case ev := <-sub.C:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestTextTurn(t *testing.T) {
	e := newEnv(t)
	sub := e.bus.Subscribe(e.user, 0)
	defer sub.Close()
	e.llm.Push(fake.Text("Hello there"))
	if err := e.a.Run(context.Background(), e.user, e.conv, "hi"); err != nil {
		t.Fatal(err)
	}
	evs := collect(sub)
	var text strings.Builder
	for _, ev := range evs[:len(evs)-1] {
		if ev.Type != events.TypeText {
			t.Fatalf("unexpected event %s", ev.Type)
		}
		text.WriteString(ev.Data.(events.TextData).Delta)
	}
	if text.String() != "Hello there" || evs[len(evs)-1].Type != events.TypeDone {
		t.Errorf("text %q, last %s", text.String(), evs[len(evs)-1].Type)
	}
	if got := e.roles(t); got != "user,assistant" {
		t.Errorf("stored roles = %s", got)
	}
	if req := e.llm.Requests()[0]; req.Model != "m" || req.System != SystemPrompt || len(req.Tools) != 1 || req.Tools[0].Name != "w__write" {
		t.Errorf("request = %+v", req)
	}
}

func TestStepBudget(t *testing.T) {
	e := newEnv(t, WithStepBudget(2))
	sub := e.bus.Subscribe(e.user, 0)
	defer sub.Close()
	e.llm.Push(fake.ToolCall("a", "nope.tool", `{}`), fake.ToolCall("b", "nope.tool", `{}`), fake.Text("never"))
	err := e.a.Run(context.Background(), e.user, e.conv, "loop")
	if !errors.Is(err, ErrStepBudget) {
		t.Fatalf("err = %v", err)
	}
	evs := collect(sub)
	last := evs[len(evs)-1]
	if last.Type != events.TypeError || !strings.Contains(last.Data.(events.ErrorData).Message, "step budget of 2") {
		t.Errorf("last event = %+v", last)
	}
	for _, ev := range evs {
		if ev.Type == events.TypeToolResult {
			// Published by the dispatcher, not the agent: exactly one per call.
			continue
		}
		if ev.Type == events.TypeDone {
			t.Error("done published after error")
		}
	}
	if got := e.roles(t); got != "user,assistant,tool,assistant,tool" {
		t.Errorf("stored roles = %s", got)
	}
	// The unknown tool came back to the model as an error result.
	if tr := e.llm.Requests()[1].Messages[2].Content[0]; !tr.IsError || !strings.Contains(tr.Content, "unknown tool") {
		t.Errorf("tool result = %+v", tr)
	}
}

func TestCancelWhileAwaitingApprovalKeepsHistoryValid(t *testing.T) {
	e := newEnv(t)
	sub := e.bus.Subscribe(e.user, 0)
	defer sub.Close()
	e.llm.Push(fake.ToolCall("w1", "w.write", `{}`))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.a.Run(ctx, e.user, e.conv, "write") }()
	deadline := time.After(5 * time.Second)
	for waiting := true; waiting; {
		select {
		case ev := <-sub.C:
			waiting = ev.Type != events.TypeApprovalRequired
		case <-deadline:
			t.Fatal("no approval_required")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := e.roles(t); got != "user,assistant,tool" {
		t.Errorf("stored roles = %s", got)
	}
	hist, err := e.a.loadHistory(context.Background(), e.conv)
	if err != nil || len(hist) != 3 || hist[2].Content[0].ToolUseID != "w1" || !hist[2].Content[0].IsError {
		t.Errorf("history = %+v, %v", hist, err)
	}
}

func TestRepairHistory(t *testing.T) {
	use := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolUseBlock("a", "x.y", nil), llm.ToolUseBlock("b", "x.y", nil)}}
	in := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
		use,
		{Role: llm.RoleTool, Content: []llm.ContentBlock{llm.ToolResultBlock("a", "ok", false)}},
		use,
		llm.NewTextMessage(llm.RoleUser, "again"),
	}
	out := repairHistory(in)
	if len(out) != 6 {
		t.Fatalf("len = %d", len(out))
	}
	if c := out[2].Content; len(c) != 2 || c[1].ToolUseID != "b" || !c[1].IsError {
		t.Errorf("partial results not completed: %+v", c)
	}
	if out[4].Role != llm.RoleTool || len(out[4].Content) != 2 || out[5].Role != llm.RoleUser {
		t.Errorf("missing tool message not inserted: %+v", out[3:])
	}
}
