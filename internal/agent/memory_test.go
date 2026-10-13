package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/fake"
	"github.com/dperedo/bosun/internal/memory"
)

// withMem enables memory on e's agent, with extraction on a separate
// provider ext (nil = no extractor).
func withMem(e *env, ext llm.Provider) *memory.Store {
	ms := memory.NewStore(e.st)
	var x *memory.Extractor
	if ext != nil {
		x = memory.NewExtractor(ms, ext, "cheap")
	}
	WithMemory(ms, x)(e.a)
	return ms
}

func TestRecallInSystemPrompt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ms := withMem(e, nil)
	if _, err := ms.Add(ctx, e.user, memory.KindFact, "The user's dentist is\nDr. Lee.", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Add(ctx, e.user, memory.KindPreference, "Prefers short answers.", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Add(ctx, e.user, memory.KindNote, "Boat is blue.", "user"); err != nil {
		t.Fatal(err)
	}
	e.llm.Push(fake.Text("Dr. Lee."))
	if err := e.a.Run(ctx, e.user, e.conv, "who is my dentist?"); err != nil {
		t.Fatal(err)
	}
	sys := e.llm.Requests()[0].System
	want := SystemPrompt + "\n\n" + MemoryHeader + "\n"
	if !strings.HasPrefix(sys, want) || !strings.HasSuffix(sys, MemoryFooter) {
		t.Fatalf("system prompt:\n%s", sys)
	}
	if !strings.Contains(sys, "- (fact) The user's dentist is Dr. Lee.\n") || !strings.Contains(sys, "- (preference) Prefers short answers.\n") {
		t.Errorf("memories missing from system prompt:\n%s", sys)
	}
	if strings.Contains(sys, "Boat") {
		t.Errorf("irrelevant note recalled:\n%s", sys)
	}
	// Memories are not sent as messages.
	for _, m := range e.llm.Requests()[0].Messages {
		if strings.Contains(m.Text(), "Dr. Lee") {
			t.Errorf("memory leaked into messages: %+v", m)
		}
	}
}

func TestNoMemoriesNoSection(t *testing.T) {
	e := newEnv(t)
	withMem(e, nil)
	e.llm.Push(fake.Text("hi"))
	if err := e.a.Run(context.Background(), e.user, e.conv, "hello"); err != nil {
		t.Fatal(err)
	}
	if sys := e.llm.Requests()[0].System; sys != SystemPrompt {
		t.Errorf("system = %q", sys)
	}
}

func TestExtractionAfterDone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ext := fake.New("ext", fake.Text(`{"memories":[{"kind":"note","content":"Boat is blue."}]}`))
	ms := withMem(e, ext)
	e.llm.Push(fake.Text("Noted."))
	if err := e.a.Run(ctx, e.user, e.conv, "remember that my boat is blue"); err != nil {
		t.Fatal(err)
	}
	e.a.Wait()
	got, err := ms.List(ctx, e.user, "", 0, 0)
	if err != nil || len(got) != 1 || got[0].Content != "Boat is blue." || got[0].Source != "conv:"+e.conv {
		t.Fatalf("memories = %+v, %v", got, err)
	}
	req := ext.Requests()[0]
	if req.Model != "cheap" || !strings.Contains(req.Messages[0].Text(), "remember that my boat is blue") ||
		!strings.Contains(req.Messages[0].Text(), "Noted.") {
		t.Errorf("extraction request = %+v", req)
	}
}

func TestNoExtractionAfterError(t *testing.T) {
	e := newEnv(t)
	ext := fake.New("ext")
	withMem(e, ext)
	e.llm.Push(fake.Error(errors.New("boom")))
	if err := e.a.Run(context.Background(), e.user, e.conv, "remember that x"); err == nil {
		t.Fatal("want error")
	}
	e.a.Wait()
	if n := len(ext.Requests()); n != 0 {
		t.Errorf("extraction ran after a failed turn (%d calls)", n)
	}
}

// blocking is a provider whose Complete blocks until ctx is done.
type blocking struct{ started chan struct{} }

func (b *blocking) Name() string { return "blocking" }
func (b *blocking) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	close(b.started)
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}
func (b *blocking) Stream(context.Context, llm.Request) (<-chan llm.Event, error) {
	return nil, errors.New("not used")
}

func TestCloseCancelsExtraction(t *testing.T) {
	e := newEnv(t)
	b := &blocking{started: make(chan struct{})}
	withMem(e, b)
	e.llm.Push(fake.Text("ok"), fake.Text("ok"))
	if err := e.a.Run(context.Background(), e.user, e.conv, "hello"); err != nil {
		t.Fatal(err)
	}
	<-b.started
	closed := make(chan struct{})
	go func() { e.a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the pending extraction")
	}
	e.a.Close() // idempotent
	// Turns still work after Close, without starting extraction.
	if err := e.a.Run(context.Background(), e.user, e.conv, "again"); err != nil {
		t.Fatal(err)
	}
	e.a.Wait()
}

func TestBaseContextCancelsExtraction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := newEnv(t, WithBaseContext(ctx))
	b := &blocking{started: make(chan struct{})}
	withMem(e, b)
	e.llm.Push(fake.Text("ok"))
	if err := e.a.Run(context.Background(), e.user, e.conv, "hello"); err != nil {
		t.Fatal(err)
	}
	<-b.started
	cancel()
	waited := make(chan struct{})
	go func() { e.a.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the base context did not stop extraction")
	}
}

func TestFormatMemories(t *testing.T) {
	if FormatMemories(nil) != "" {
		t.Error("empty list should render nothing")
	}
	got := FormatMemories([]memory.Memory{{Kind: memory.KindNote, Content: "a\n\tb  c"}})
	if !strings.Contains(got, "\n- (note) a b c\n"+MemoryFooter) {
		t.Errorf("FormatMemories = %q", got)
	}
}
