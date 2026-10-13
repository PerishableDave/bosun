// Package hooks defines hook points and a registry of hook functions.
//
// Tool hooks (pre_tool_call, post_tool_call) are run by the dispatcher.
// pre_model_call hooks are run by the agent before every model call
// (Registry.RunPreModelCall), and on_approval hooks by the approval manager's
// OnResolve callback (Registry.RunOnApproval), which the daemon wires up.
// Hooks are Go functions; script hooks (package hooks/script) adapt a
// user-supplied executable to the same function types.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/llm"
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

// Hook types reported by Registry.List.
const (
	TypeGo     = "go"
	TypeScript = "script"
)

// ErrHook wraps every failure returned by RunPreModelCall.
var ErrHook = errors.New("hook failed")

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

// PreModelCall runs before every model call with the fully assembled request.
// It may rewrite req.System and req.Messages (e.g. to redact). The messages
// and content-block slices it receives are private copies, so assigning to
// req.Messages[i].Content[j] does not touch the conversation history. An
// error fails the turn. The calling user is available via UserIDFromContext.
type PreModelCall func(ctx context.Context, req *llm.Request) error

// OnApproval is a notification hook run after an approval is resolved by the
// user, expires, or is canceled. d is approval.Approve, approval.Deny, or
// Decision(approval.DecisionExpired) / Decision(approval.DecisionCanceled).
type OnApproval func(ctx context.Context, a approval.Approval, d approval.Decision)

// Named pairs a hook function with a unique name (for audit and logs).
type Named[F any] struct {
	Name string
	Fn   F
}

// Info describes a registered hook (GET /v1/hooks).
type Info struct {
	Point Point  `json:"point"`
	Name  string `json:"name"`
	Type  string `json:"type"` // TypeGo | TypeScript
}

// AddOption modifies a registration.
type AddOption func(*addOpts)

type addOpts struct{ typ string }

// AsType records the hook's type for listing (default TypeGo).
func AsType(t string) AddOption { return func(o *addOpts) { o.typ = t } }

// Registry holds named hook functions in registration order. It is safe for
// concurrent use. The Run and listing methods accept a nil *Registry (no
// hooks).
type Registry struct {
	mu       sync.RWMutex
	pre      []Named[PreToolCall]
	post     []Named[PostToolCall]
	model    []Named[PreModelCall]
	approval []Named[OnApproval]
	// events are on_event handlers (see events.go).
	events []Named[EventHandler]
	infos  []Info
	log    *slog.Logger
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// SetLogger sets the logger used to report on_approval hook panics.
func (r *Registry) SetLogger(l *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = l
}

func (r *Registry) add(p Point, name string, nilFn bool, opts []AddOption, push func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkName(name, nilFn, r.names()); err != nil {
		return err
	}
	o := addOpts{typ: TypeGo}
	for _, f := range opts {
		f(&o)
	}
	push()
	r.infos = append(r.infos, Info{Point: p, Name: name, Type: o.typ})
	return nil
}

// AddPreToolCall registers a pre_tool_call hook.
func (r *Registry) AddPreToolCall(name string, fn PreToolCall, opts ...AddOption) error {
	return r.add(PointPreToolCall, name, fn == nil, opts, func() { r.pre = append(r.pre, Named[PreToolCall]{name, fn}) })
}

// AddPostToolCall registers a post_tool_call hook.
func (r *Registry) AddPostToolCall(name string, fn PostToolCall, opts ...AddOption) error {
	return r.add(PointPostToolCall, name, fn == nil, opts, func() { r.post = append(r.post, Named[PostToolCall]{name, fn}) })
}

// AddPreModelCall registers a pre_model_call hook.
func (r *Registry) AddPreModelCall(name string, fn PreModelCall, opts ...AddOption) error {
	return r.add(PointPreModelCall, name, fn == nil, opts, func() { r.model = append(r.model, Named[PreModelCall]{name, fn}) })
}

// AddOnApproval registers an on_approval hook.
func (r *Registry) AddOnApproval(name string, fn OnApproval, opts ...AddOption) error {
	return r.add(PointOnApproval, name, fn == nil, opts, func() { r.approval = append(r.approval, Named[OnApproval]{name, fn}) })
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

// PreModelCalls returns the pre_model_call hooks in registration order.
func (r *Registry) PreModelCalls() []Named[PreModelCall] {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Named[PreModelCall](nil), r.model...)
}

// OnApprovals returns the on_approval hooks in registration order.
func (r *Registry) OnApprovals() []Named[OnApproval] {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Named[OnApproval](nil), r.approval...)
}

// List returns every registered hook in registration order.
func (r *Registry) List() []Info {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Info(nil), r.infos...)
}

// RunPreModelCall runs the pre_model_call hooks in order on req, with userID
// available to them via UserIDFromContext. Hooks see and rewrite private
// copies of req.Messages and their content blocks, so the caller's history is
// never modified. The first failing (or panicking) hook stops the chain; the
// error wraps ErrHook and the request must not be sent.
func (r *Registry) RunPreModelCall(ctx context.Context, userID int64, req *llm.Request) error {
	hs := r.PreModelCalls()
	if len(hs) == 0 {
		return nil
	}
	ctx = WithUserID(ctx, userID)
	req.Messages = cloneMessages(req.Messages)
	for _, h := range hs {
		if err := safeModel(ctx, h, req); err != nil {
			return fmt.Errorf("%w: pre_model_call %s: %v", ErrHook, h.Name, err)
		}
	}
	return nil
}

// RunOnApproval runs the on_approval hooks in order. Panics are logged and
// otherwise ignored (these are notifications). It blocks until every hook
// returns; callers on a hot path should run it in a goroutine.
func (r *Registry) RunOnApproval(ctx context.Context, a approval.Approval, d approval.Decision) {
	hs := r.OnApprovals()
	if len(hs) == 0 {
		return
	}
	ctx = WithUserID(ctx, a.UserID)
	for _, h := range hs {
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.logger().Error("hooks: on_approval hook panicked", "hook", h.Name, "panic", fmt.Sprint(p))
				}
			}()
			h.Fn(ctx, a, d)
		}()
	}
}

func (r *Registry) logger() *slog.Logger {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.log
}

func safeModel(ctx context.Context, h Named[PreModelCall], req *llm.Request) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h.Fn(ctx, req)
}

func cloneMessages(in []llm.Message) []llm.Message {
	if in == nil {
		return nil
	}
	out := make([]llm.Message, len(in))
	for i, m := range in {
		out[i] = llm.Message{Role: m.Role, Content: append([]llm.ContentBlock(nil), m.Content...)}
		for j := range out[i].Content {
			if raw := out[i].Content[j].Input; raw != nil {
				out[i].Content[j].Input = append(json.RawMessage(nil), raw...)
			}
		}
	}
	return out
}

type userIDKey struct{}

// WithUserID returns ctx carrying the user a model-call or approval hook runs
// for.
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext returns the user set by RunPreModelCall / RunOnApproval.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDKey{}).(int64)
	return id, ok
}

func (r *Registry) names() map[string]bool {
	m := map[string]bool{}
	for _, h := range r.infos {
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
