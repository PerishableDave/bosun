package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm"
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

func TestModelAndApprovalHooks(t *testing.T) {
	var nilReg *Registry
	if err := nilReg.RunPreModelCall(context.Background(), 1, &llm.Request{}); err != nil || nilReg.List() != nil {
		t.Fatal("nil registry should be a no-op")
	}
	nilReg.RunOnApproval(context.Background(), approval.Approval{}, approval.Approve)

	r := NewRegistry()
	var order []string
	_ = r.AddPreModelCall("one", func(ctx context.Context, req *llm.Request) error {
		uid, _ := UserIDFromContext(ctx)
		if uid != 9 {
			t.Errorf("user = %d", uid)
		}
		order = append(order, "one")
		req.System = "rewritten"
		return nil
	})
	_ = r.AddPreModelCall("two", func(context.Context, *llm.Request) error { panic("boom") })
	_ = r.AddPreModelCall("three", func(context.Context, *llm.Request) error { order = append(order, "three"); return nil })
	req := llm.Request{System: "orig"}
	err := r.RunPreModelCall(context.Background(), 9, &req)
	if !errors.Is(err, ErrHook) || req.System != "rewritten" || len(order) != 1 {
		t.Fatalf("err = %v, req = %+v, order = %v", err, req, order)
	}

	var got []approval.Decision
	_ = r.AddOnApproval("panics", func(context.Context, approval.Approval, approval.Decision) { panic("x") }, AsType(TypeScript))
	_ = r.AddOnApproval("records", func(ctx context.Context, a approval.Approval, d approval.Decision) {
		if uid, _ := UserIDFromContext(ctx); uid != a.UserID {
			t.Errorf("user = %d", uid)
		}
		got = append(got, d)
	})
	r.RunOnApproval(context.Background(), approval.Approval{UserID: 4}, approval.DecisionExpired)
	if len(got) != 1 || got[0] != approval.DecisionExpired {
		t.Errorf("got = %v", got)
	}

	list := r.List()
	if len(list) != 5 || list[0] != (Info{PointPreModelCall, "one", TypeGo}) || list[3] != (Info{PointOnApproval, "panics", TypeScript}) {
		t.Errorf("List = %+v", list)
	}
	if err := r.AddOnApproval("one", func(context.Context, approval.Approval, approval.Decision) {}); err == nil {
		t.Error("name reused across points")
	}
}
