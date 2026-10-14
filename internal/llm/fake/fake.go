// Package fake provides a scripted llm.Provider for tests. Each Complete or
// Stream call consumes the next scripted Turn in order.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/perishabledave/bosun/internal/llm"
)

// ErrScriptExhausted is returned when more calls are made than turns scripted.
var ErrScriptExhausted = errors.New("fake: no scripted turns left")

// Turn is one scripted reply: either a Response or an Err.
type Turn struct {
	Response llm.Response
	Err      error
}

// Text scripts an assistant reply consisting of text, ending the turn.
func Text(text string) Turn {
	return Turn{Response: llm.Response{
		Message:    llm.NewTextMessage(llm.RoleAssistant, text),
		StopReason: llm.StopEndTurn,
	}}
}

// ToolCall scripts an assistant reply that calls one tool. args must be JSON.
func ToolCall(id, name, args string) Turn {
	return Turn{Response: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock(id, name, json.RawMessage(args)),
		}},
		StopReason: llm.StopToolUse,
	}}
}

// Error scripts a failed call.
func Error(err error) Turn { return Turn{Err: err} }

// Provider is a scripted llm.Provider. It is safe for concurrent use.
type Provider struct {
	name string

	mu       sync.Mutex
	script   []Turn
	requests []llm.Request
	// ChunkSize controls how text is split into EventTextDelta events when
	// streaming (default 4 bytes).
	ChunkSize int
}

// New returns a fake provider named name (default "fake") with a script.
func New(name string, script ...Turn) *Provider {
	if name == "" {
		name = "fake"
	}
	return &Provider{name: name, script: script, ChunkSize: 4}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

// Push appends turns to the script.
func (p *Provider) Push(turns ...Turn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append(p.script, turns...)
}

// Requests returns a copy of every request received so far.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

// Remaining returns how many scripted turns are left.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.script)
}

func (p *Provider) next(req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.script) == 0 {
		return llm.Response{}, ErrScriptExhausted
	}
	t := p.script[0]
	p.script = p.script[1:]
	if t.Err != nil {
		return llm.Response{}, t.Err
	}
	r := t.Response
	if r.Model == "" {
		r.Model = req.Model
	}
	r.Message.Role = llm.RoleAssistant
	return r, nil
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	r, err := p.next(req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("fake complete: %w", err)
	}
	return r, nil
}

// Stream implements llm.Provider. Scripted errors are delivered as an
// EventError on the channel; ctx errors are returned directly.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := p.next(req)
	var events []llm.Event
	if err != nil {
		events = []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("fake stream: %w", err)}}
	} else {
		events = p.toEvents(r)
	}
	ch := make(chan llm.Event)
	go func() {
		defer close(ch)
		for _, ev := range events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (p *Provider) toEvents(r llm.Response) []llm.Event {
	n := p.ChunkSize
	if n <= 0 {
		n = 4
	}
	var evs []llm.Event
	for i, b := range r.Message.Content {
		switch b.Type {
		case llm.BlockText:
			for s := b.Text; s != ""; {
				k := min(n, len(s))
				evs = append(evs, llm.Event{Type: llm.EventTextDelta, Index: i, Text: s[:k]})
				s = s[k:]
			}
		case llm.BlockToolUse:
			evs = append(evs, llm.Event{Type: llm.EventToolUseStart, Index: i, ToolUseID: b.ID, ToolName: b.Name})
			if len(b.Input) > 0 {
				evs = append(evs, llm.Event{Type: llm.EventToolUseDelta, Index: i, InputDelta: string(b.Input)})
			}
			evs = append(evs, llm.Event{Type: llm.EventToolUseStop, Index: i})
		}
	}
	resp := r
	return append(evs, llm.Event{Type: llm.EventDone, Response: &resp})
}

var _ llm.Provider = (*Provider)(nil)
