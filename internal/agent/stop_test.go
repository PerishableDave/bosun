package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/llm"
)

// stallProvider streams some text, then waits until the request is canceled.
type stallProvider struct{ text string }

func (stallProvider) Name() string { return "stall" }
func (stallProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}
func (p stallProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 1)
	go func() {
		defer close(ch)
		select {
		case ch <- llm.Event{Type: llm.EventTextDelta, Text: p.text}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
		ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

func waitFor(t *testing.T, sub *events.Subscription, typ string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub.C:
			if ev.Type == typ {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

func TestStopEndsTurnWithDoneAndKeepsPartialText(t *testing.T) {
	e := newEnv(t)
	reg := llm.NewRegistry()
	reg.Register(stallProvider{text: "Half an ans"})
	a := newAgent(reg, "stall/m", nil, nil, e.st, e.bus)
	sub := e.bus.Subscribe(e.user, 0)
	defer sub.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, e.user, e.conv, "explain") }()
	waitFor(t, sub, events.TypeText)
	cancel(ErrStopped)
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil for a stopped turn", err)
	}
	evs := collect(sub)
	if len(evs) == 0 {
		t.Fatal("no terminal event")
	}
	last := evs[len(evs)-1]
	if d, ok := last.Data.(events.DoneData); !ok || !d.Stopped || d.ConversationID != e.conv {
		t.Fatalf("last event = %+v", last)
	}
	ms, _ := e.st.ListMessages(context.Background(), e.conv)
	if len(ms) != 2 || ms[1].Role != "assistant" || !strings.Contains(string(ms[1].Content), "Half an ans") {
		t.Errorf("stored %d messages", len(ms))
	}

	// A plain cancellation (client gone) still ends in error.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { done <- a.Run(ctx2, e.user, e.conv, "again") }()
	waitFor(t, sub, events.TypeText)
	cancel2()
	if err := <-done; err == nil {
		t.Fatal("canceled turn returned nil")
	}
	evs = collect(sub)
	if len(evs) == 0 || evs[len(evs)-1].Type != events.TypeError {
		t.Errorf("canceled turn events = %+v", evs)
	}
}
