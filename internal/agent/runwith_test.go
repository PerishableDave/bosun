package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/llm/fake"
)

func TestRunWithModelOverride(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.llm.Push(fake.Text("hi"))
	if err := e.a.RunWithModel(ctx, e.user, e.conv, "hello", "fake/other-model"); err != nil {
		t.Fatal(err)
	}
	if reqs := e.llm.Requests(); len(reqs) != 1 || reqs[0].Model != "other-model" {
		t.Fatalf("requests = %+v", reqs)
	}
	err := e.a.RunWith(ctx, e.user, e.conv, "hello", RunOptions{Model: "nope/x"})
	if err == nil || !strings.Contains(err.Error(), "model not available") {
		t.Errorf("unknown model err = %v", err)
	}
	if len(e.llm.Requests()) != 1 {
		t.Error("model called for an unknown override")
	}
	// Empty override uses the default model.
	e.llm.Push(fake.Text("hi"))
	if err := e.a.RunWith(ctx, e.user, e.conv, "again", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if reqs := e.llm.Requests(); reqs[len(reqs)-1].Model != "m" {
		t.Errorf("default model = %q", reqs[len(reqs)-1].Model)
	}
}
