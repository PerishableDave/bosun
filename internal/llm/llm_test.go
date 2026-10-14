package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

func TestRegistry(t *testing.T) {
	r := llm.NewRegistry()
	if err := r.Register(fake.New("anthropic")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fake.New("anthropic")); err == nil {
		t.Error("duplicate accepted")
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil accepted")
	}
	p, model, err := r.Resolve("anthropic/claude-sonnet-5-5")
	if err != nil || p.Name() != "anthropic" || model != "claude-sonnet-5-5" {
		t.Fatalf("Resolve = %v, %q, %v", p, model, err)
	}
	if _, _, err := r.Resolve("ollama/llama"); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Errorf("unknown provider: %v", err)
	}
	if _, _, err := r.Resolve("nomodel"); err == nil {
		t.Error("bad ref accepted")
	}
	if got := r.Names(); len(got) != 1 || got[0] != "anthropic" {
		t.Errorf("Names = %v", got)
	}
}

func TestFakeComplete(t *testing.T) {
	ctx := context.Background()
	p := fake.New("", fake.ToolCall("t1", "notes.search", `{"q":"x"}`), fake.Text("done"), fake.Error(errors.New("boom")))
	r, err := p.Complete(ctx, llm.Request{Model: "m"})
	if err != nil || r.StopReason != llm.StopToolUse || len(r.Message.ToolUses()) != 1 || r.Model != "m" {
		t.Fatalf("turn 1 = %+v, %v", r, err)
	}
	r, err = p.Complete(ctx, llm.Request{})
	if err != nil || r.Message.Text() != "done" {
		t.Fatalf("turn 2 = %+v, %v", r, err)
	}
	if _, err := p.Complete(ctx, llm.Request{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("turn 3 err = %v", err)
	}
	if _, err := p.Complete(ctx, llm.Request{}); !errors.Is(err, fake.ErrScriptExhausted) {
		t.Errorf("exhausted: %v", err)
	}
	if len(p.Requests()) != 4 {
		t.Errorf("requests = %d", len(p.Requests()))
	}
}

func TestFakeStream(t *testing.T) {
	ctx := context.Background()
	p := fake.New("", fake.Text("hello world"), fake.ToolCall("t1", "x.y", `{}`))
	ch, err := p.Stream(ctx, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var done *llm.Response
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			text.WriteString(ev.Text)
		case llm.EventDone:
			done = ev.Response
		}
	}
	if text.String() != "hello world" || done == nil || done.Message.Text() != "hello world" {
		t.Fatalf("stream text = %q, done = %+v", text.String(), done)
	}
	ch, _ = p.Stream(ctx, llm.Request{})
	var types []llm.EventType
	for ev := range ch {
		types = append(types, ev.Type)
	}
	want := []llm.EventType{llm.EventToolUseStart, llm.EventToolUseDelta, llm.EventToolUseStop, llm.EventDone}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}
}
