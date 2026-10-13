package hooks

import (
	"context"
	"testing"

	"github.com/dperedo/bosun/internal/integration"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	pre := func(context.Context, ToolCall) (PreToolCallResult, error) { return PreToolCallResult{}, nil }
	post := func(context.Context, ToolCall, integration.Result) (PostToolCallResult, error) {
		return PostToolCallResult{}, nil
	}
	if err := r.AddPreToolCall("a", pre); err != nil {
		t.Fatal(err)
	}
	if err := r.AddPreToolCall("b", pre); err != nil {
		t.Fatal(err)
	}
	if err := r.AddPostToolCall("a", post); err == nil {
		t.Error("duplicate name accepted")
	}
	if err := r.AddPostToolCall("", post); err == nil {
		t.Error("empty name accepted")
	}
	if err := r.AddPreToolCall("c", nil); err == nil {
		t.Error("nil fn accepted")
	}
	got := r.PreToolCalls()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("PreToolCalls = %+v", got)
	}
	if len(r.PostToolCalls()) != 0 {
		t.Error("unexpected post hooks")
	}
}
