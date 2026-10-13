// Package hooks defines hook points and a registry of Go hook functions.
// Running hooks (ordering, error handling, script hooks) is implemented
// separately; this package holds types and registration only.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dperedo/bosun/internal/integration"
)

// Point names a hook point.
type Point string

// Hook points.
const (
	PointPreToolCall  Point = "pre_tool_call"
	PointPostToolCall Point = "post_tool_call"
	PointOnSchedule   Point = "on_schedule"
	PointOnEvent      Point = "on_event"
	PointPreModelCall Point = "pre_model_call"
	PointOnApproval   Point = "on_approval"
)

// ToolCall describes a tool invocation passed to tool hooks.
type ToolCall struct {
	UserID      int64
	ID          string // model-assigned tool_use ID
	Tool        string
	Integration string
	Capability  integration.Capability
	Risk        integration.Risk
	Args        json.RawMessage
}

// PreToolCallResult is what a pre_tool_call hook may change.
type PreToolCallResult struct {
	Args       json.RawMessage // non-nil replaces the arguments
	Cancel     bool            // true stops the call (recorded in audit)
	Reason     string          // why it was canceled; shown to the model
	AuditNotes []string
}

// PostToolCallResult is what a post_tool_call hook may change.
type PostToolCallResult struct {
	Result     *integration.Result // non-nil replaces the result
	AuditNotes []string
}

// PreToolCall runs before policy is consulted. It may rewrite args, cancel,
// or annotate. Rewritten args are re-checked by policy.
type PreToolCall func(ctx context.Context, call ToolCall) (PreToolCallResult, error)

// PostToolCall runs after the handler returns. It may rewrite the result or
// annotate.
type PostToolCall func(ctx context.Context, call ToolCall, res integration.Result) (PostToolCallResult, error)

// Registry holds named hook functions in registration order. It is safe for
// concurrent use.
type Registry struct {
	mu   sync.RWMutex
	pre  []Named[PreToolCall]
	post []Named[PostToolCall]
}

// Named pairs a hook function with a unique name (for audit and logs).
type Named[F any] struct {
	Name string
	Fn   F
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// AddPreToolCall registers a pre_tool_call hook.
func (r *Registry) AddPreToolCall(name string, fn PreToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkName(name, fn == nil, r.names()); err != nil {
		return err
	}
	r.pre = append(r.pre, Named[PreToolCall]{name, fn})
	return nil
}

// AddPostToolCall registers a post_tool_call hook.
func (r *Registry) AddPostToolCall(name string, fn PostToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkName(name, fn == nil, r.names()); err != nil {
		return err
	}
	r.post = append(r.post, Named[PostToolCall]{name, fn})
	return nil
}

// PreToolCalls returns the pre_tool_call hooks in registration order.
func (r *Registry) PreToolCalls() []Named[PreToolCall] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Named[PreToolCall](nil), r.pre...)
}

// PostToolCalls returns the post_tool_call hooks in registration order.
func (r *Registry) PostToolCalls() []Named[PostToolCall] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Named[PostToolCall](nil), r.post...)
}

func (r *Registry) names() map[string]bool {
	m := map[string]bool{}
	for _, h := range r.pre {
		m[h.Name] = true
	}
	for _, h := range r.post {
		m[h.Name] = true
	}
	return m
}

func checkName(name string, nilFn bool, existing map[string]bool) error {
	switch {
	case name == "":
		return fmt.Errorf("hooks: empty hook name")
	case nilFn:
		return fmt.Errorf("hooks: hook %q has nil function", name)
	case existing[name]:
		return fmt.Errorf("hooks: hook %q already registered", name)
	}
	return nil
}
