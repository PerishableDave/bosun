// Package dispatch is the single choke point between the model and a tool
// Handler. Every tool call the agent (or a routine) makes goes through
// Dispatcher.Dispatch, which runs, in order:
//
//  1. tool lookup (unknown tool → error),
//  2. pre_tool_call hooks (may rewrite args or cancel),
//  3. argument validation against the tool's InputSchema,
//  4. policy.Decide (deny → *DeniedError), after the tool's RiskFunc (if
//     any) has had the chance to escalate the risk, and with the call's
//     scope (the tool's ScopeArg, normalized) when the engine is a
//     policy.ScopedEngine; an escalated call is never silently allowed,
//  5. on ask: the integration's preview (integration.Previewer /
//     ApprovalDetailer, if implemented), then an approval, blocking until
//     approved / denied / expired,
//  6. the Handler, with the user and conversation in ctx, a timeout, and
//     panic recovery,
//  7. post_tool_call hooks (may rewrite the result),
//
// and records an audit entry for every outcome. No other package may invoke a
// Tool's Handler (enforced by TestOnlyDispatcherInvokesHandlers).
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/hooks"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/policy"
)

// DefaultHandlerTimeout bounds a single Handler invocation.
const DefaultHandlerTimeout = 60 * time.Second

// Sentinel errors. Use errors.Is; the concrete errors carry details.
var (
	// ErrDenied: policy denied the call, or the user denied / did not answer
	// the approval. The error text is safe to show the model.
	ErrDenied = errors.New("tool call denied")
	// ErrCanceled: a pre_tool_call hook canceled the call.
	ErrCanceled = errors.New("tool call canceled by hook")
	// ErrUnknownTool: no tool with that name is registered.
	ErrUnknownTool = errors.New("unknown tool")
	// ErrInvalidArgs: the arguments are not valid JSON or do not match the
	// tool's InputSchema.
	ErrInvalidArgs = errors.New("invalid tool arguments")
	// ErrIntegrationDisabled: the integration panicked earlier and is
	// disabled until the daemon restarts.
	ErrIntegrationDisabled = errors.New("integration disabled")
	// ErrHandlerPanic: the handler panicked (its integration is now disabled).
	ErrHandlerPanic = errors.New("tool handler panicked")
	// ErrHandlerTimeout: the handler did not return within the timeout.
	ErrHandlerTimeout = errors.New("tool handler timed out")
	// ErrHook: a hook failed; the call is not run (pre) or its result is
	// withheld (post). Fail closed.
	ErrHook = errors.New("tool hook failed")
)

// DeniedError is returned when a call is not permitted. errors.Is(err,
// ErrDenied) is true.
type DeniedError struct {
	Tool       string
	Capability integration.Capability
	// Reason is one of "policy", "user denied", "approval expired",
	// "approval canceled", "approval failed".
	Reason     string
	ApprovalID string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("tool call %s denied (%s): the user has not permitted capability %q", e.Tool, e.Reason, e.Capability)
}

// Is makes errors.Is(err, ErrDenied) true.
func (e *DeniedError) Is(target error) bool { return target == ErrDenied }

// CanceledError is returned when a pre_tool_call hook cancels a call.
// errors.Is(err, ErrCanceled) is true.
type CanceledError struct {
	Tool   string
	Hook   string
	Reason string
}

func (e *CanceledError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("tool call %s canceled by hook %s", e.Tool, e.Hook)
	}
	return fmt.Sprintf("tool call %s canceled by hook %s: %s", e.Tool, e.Hook, e.Reason)
}

// Is makes errors.Is(err, ErrCanceled) true.
func (e *CanceledError) Is(target error) bool { return target == ErrCanceled }

// Call is one model-issued tool call.
type Call struct {
	UserID         int64
	ConversationID string
	ToolCallID     string
	Name           string
	Args           json.RawMessage
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithHandlerTimeout sets the per-call Handler timeout (default 60s).
func WithHandlerTimeout(d time.Duration) Option {
	return func(x *Dispatcher) {
		if d > 0 {
			x.timeout = d
		}
	}
}

// WithApprovalTimeout overrides the approval manager's default timeout for
// approvals created by this dispatcher (0 = the manager's default, i.e.
// policy.approval_timeout).
func WithApprovalTimeout(d time.Duration) Option {
	return func(x *Dispatcher) { x.approvalTimeout = d }
}

// WithLogger sets the logger for operational errors (e.g. audit failures).
func WithLogger(l *slog.Logger) Option {
	return func(x *Dispatcher) {
		if l != nil {
			x.log = l
		}
	}
}

// Dispatcher routes tool calls through hooks, policy, approvals, the handler,
// and audit. It is safe for concurrent use.
type Dispatcher struct {
	reg             *integration.Registry
	pol             policy.Engine
	appr            approval.Manager
	aud             audit.Logger
	hk              *hooks.Registry
	bus             events.Publisher
	timeout         time.Duration
	approvalTimeout time.Duration
	log             *slog.Logger

	// disabled holds integrations whose handler panicked, keyed by name, with
	// the panic message. integration.Registry deliberately has no mutation
	// API beyond Register, so the dispatcher keeps this set itself; it lasts
	// until the process restarts (ARCHITECTURE.md: "recovered by the
	// dispatcher and disabled until restart").
	mu       sync.RWMutex
	disabled map[string]string
}

// New returns a Dispatcher. hk and bus may be nil. bus, if set, receives a
// tool_result event (ID = Call.ToolCallID, Summary) for every dispatched
// call, whatever the outcome; the agent loop should therefore publish
// tool_call events but not tool_result events.
func New(reg *integration.Registry, pol policy.Engine, appr approval.Manager, aud audit.Logger, hk *hooks.Registry, bus events.Publisher, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		reg: reg, pol: pol, appr: appr, aud: aud, hk: hk, bus: bus,
		timeout: DefaultHandlerTimeout, log: slog.New(slog.DiscardHandler), disabled: map[string]string{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Disabled returns the integrations disabled after a handler panic, mapped to
// the panic message.
func (d *Dispatcher) Disabled() map[string]string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]string, len(d.disabled))
	for k, v := range d.disabled {
		out[k] = v
	}
	return out
}

func (d *Dispatcher) isDisabled(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.disabled[name]
	return ok
}

func (d *Dispatcher) disable(name, why string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disabled[name] = why
}

// Dispatch runs one tool call. See the package doc for the sequence. The
// returned error is suitable for showing to the model (errors.Is against the
// sentinels to classify it); the Result is only meaningful when err is nil.
func (d *Dispatcher) Dispatch(ctx context.Context, c Call) (res integration.Result, err error) {
	e := audit.Entry{UserID: c.UserID, Kind: audit.KindToolCall, Tool: c.Name, Args: c.Args}
	defer func() {
		if err != nil {
			e.Error = err.Error()
			res = integration.Result{}
		} else {
			e.ResultSummary = audit.Summarize(res)
		}
		d.record(e)
		d.publishResult(c, res, err)
	}()

	if c.UserID <= 0 {
		return res, fmt.Errorf("dispatch %s: no user: %w", c.Name, ErrDenied)
	}
	tool, integ, ok := d.reg.Tool(c.Name)
	if !ok {
		return res, fmt.Errorf("%w: %q", ErrUnknownTool, c.Name)
	}
	e.Integration, e.Capability, e.Risk = integ, tool.Capability, tool.Risk
	if d.isDisabled(integ) {
		return res, fmt.Errorf("%w: %s (a previous call panicked; restart the daemon to re-enable)", ErrIntegrationDisabled, integ)
	}

	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return res, fmt.Errorf("%w: not valid JSON", ErrInvalidArgs)
	}
	call := hooks.ToolCall{UserID: c.UserID, ID: c.ToolCallID, Tool: c.Name, Integration: integ,
		Capability: tool.Capability, Risk: tool.Risk, Args: args}

	// 2. pre_tool_call hooks.
	if err := d.runPreHooks(ctx, &call, &e); err != nil {
		return res, err
	}
	e.Args = call.Args

	// 3. Schema validation (after rewrites, so rewritten args are checked too).
	if err := ValidateArgs(tool.InputSchema, call.Args); err != nil {
		return res, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}

	// 4. Risk escalation, scope, and policy.
	risk, escalated := d.effectiveRisk(ctx, c, tool, call.Args, &e)
	e.Risk = risk
	scope := ""
	if tool.ScopeArg != "" {
		scope = ScopeOf(call.Args, tool.ScopeArg)
		if scope != "" {
			e.Notes = append(e.Notes, "scope: "+scope)
		}
	}
	var dec policy.Decision
	if se, ok := d.pol.(policy.ScopedEngine); ok && scope != "" {
		dec = se.DecideScoped(ctx, c.UserID, tool.Capability, scope, risk, call.Args)
	} else {
		dec = d.pol.Decide(ctx, c.UserID, tool.Capability, risk, call.Args)
	}
	if escalated && dec == policy.Allow {
		dec = policy.Ask
		e.Notes = append(e.Notes, "escalated risk: allow rule does not cover this call; asking")
	}
	e.Decision = string(dec)
	switch dec {
	case policy.Allow:
	case policy.Ask:
		if err := d.awaitApproval(ctx, c, integ, tool, risk, scope, call.Args, &e); err != nil {
			return res, err
		}
	default: // Deny and anything unexpected.
		e.Decision = string(policy.Deny)
		return res, &DeniedError{Tool: c.Name, Capability: tool.Capability, Reason: "policy"}
	}

	// 6. Handler.
	res, err = d.invoke(ctx, c, integ, tool, call.Args)
	if err != nil {
		return res, err
	}
	if len(res.AuditDetail) > 0 && json.Valid(res.AuditDetail) {
		e.Detail = res.AuditDetail
	}

	// 7. post_tool_call hooks.
	return d.runPostHooks(ctx, call, res, &e)
}

// riskRank orders risks for escalation.
func riskRank(r integration.Risk) int {
	switch r {
	case integration.RiskRead:
		return 1
	case integration.RiskWrite:
		return 2
	case integration.RiskDestructive:
		return 3
	}
	return 0
}

// effectiveRisk returns the call's risk: the tool's static Risk, escalated
// (never lowered) by its RiskFunc. A RiskFunc that panics escalates to
// Destructive (fail closed).
func (d *Dispatcher) effectiveRisk(ctx context.Context, c Call, tool integration.Tool, args json.RawMessage, e *audit.Entry) (integration.Risk, bool) {
	if tool.RiskFunc == nil {
		return tool.Risk, false
	}
	rctx := integration.WithConversationID(integration.WithUserID(ctx, c.UserID), c.ConversationID)
	r := func() (r integration.Risk) {
		defer func() {
			if p := recover(); p != nil {
				d.log.Error("dispatch: RiskFunc panicked; treating the call as destructive", "tool", tool.Name, "panic", fmt.Sprint(p))
				r = integration.RiskDestructive
			}
		}()
		return tool.RiskFunc(rctx, args)
	}()
	if !r.Valid() || riskRank(r) <= riskRank(tool.Risk) {
		return tool.Risk, false
	}
	e.Notes = append(e.Notes, fmt.Sprintf("risk escalated from %s to %s", tool.Risk, r))
	return r, true
}

// ScopeOf extracts and normalizes the scope argument name from args ("" if
// absent or not a string).
func ScopeOf(args json.RawMessage, name string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	var v string
	if json.Unmarshal(m[name], &v) != nil {
		return ""
	}
	return integration.NormalizeScope(v)
}

// preview asks the integration for an approval preview and details. Errors
// (and panics) abort the call.
func (d *Dispatcher) preview(ctx context.Context, c Call, integ string, tool integration.Tool, args json.RawMessage) (img []byte, mime string, display json.RawMessage, err error) {
	display = args
	in, ok := d.reg.Integration(integ)
	if !ok {
		return nil, "", display, nil
	}
	pv, hasPreview := in.(integration.Previewer)
	dt, hasDetails := in.(integration.ApprovalDetailer)
	if !hasPreview && !hasDetails {
		return nil, "", display, nil
	}
	pctx, cancel := context.WithTimeout(integration.WithConversationID(integration.WithUserID(ctx, c.UserID), c.ConversationID), d.timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("dispatch: preview panicked", "tool", tool.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			img, mime, display, err = nil, "", args, fmt.Errorf("preview panicked: %v", p)
		}
	}()
	if hasPreview {
		img, mime, err = pv.Preview(pctx, tool.Name, args)
		if err != nil {
			return nil, "", args, err
		}
		if len(img) > integration.MaxPreviewBytes {
			d.log.Warn("dispatch: preview too large; dropped", "tool", tool.Name, "bytes", len(img))
			img, mime = nil, ""
		}
	}
	if hasDetails {
		det, err := dt.ApprovalDetails(pctx, tool.Name, args)
		if err != nil {
			return nil, "", args, err
		}
		if len(det) > 0 && json.Valid(det) {
			var m map[string]json.RawMessage
			if json.Unmarshal(args, &m) == nil {
				if m == nil {
					m = map[string]json.RawMessage{}
				}
				m["details"] = det
				if b, err := json.Marshal(m); err == nil {
					display = b
				}
			}
		}
	}
	return img, mime, display, nil
}

func (d *Dispatcher) awaitApproval(ctx context.Context, c Call, integ string, tool integration.Tool, risk integration.Risk, scope string, args json.RawMessage, e *audit.Entry) error {
	denied := func(reason, id string) error {
		return &DeniedError{Tool: c.Name, Capability: tool.Capability, Reason: reason, ApprovalID: id}
	}
	if d.appr == nil {
		return denied("approval failed", "")
	}
	img, mime, display, err := d.preview(ctx, c, integ, tool, args)
	if err != nil {
		e.Notes = append(e.Notes, "preview failed: "+err.Error())
		return fmt.Errorf("%s: %w", tool.Name, err)
	}
	a, err := d.appr.Request(ctx, approval.Params{
		UserID: c.UserID, ConversationID: c.ConversationID, ToolCallID: c.ToolCallID, Tool: c.Name,
		Capability: tool.Capability, Risk: risk, Args: display, Timeout: d.approvalTimeout,
		Scope: scope, Preview: img, PreviewMIME: mime,
	})
	if err != nil {
		e.Notes = append(e.Notes, "approval request failed: "+err.Error())
		return denied("approval failed", "")
	}
	e.ApprovalID = a.ID
	ans, err := d.appr.Wait(ctx, a.ID)
	switch {
	case err == nil && ans == approval.Approve:
		e.Decision, e.ApproverID = string(approval.Approve), c.UserID // only the owner can resolve
		return nil
	case err == nil:
		e.Decision, e.ApproverID = string(approval.Deny), c.UserID
		return denied("user denied", a.ID)
	case errors.Is(err, approval.ErrExpired):
		e.Decision = approval.DecisionExpired
		return denied("approval expired", a.ID)
	case ctx.Err() != nil:
		e.Decision = approval.DecisionCanceled
		return fmt.Errorf("waiting for approval %s: %w", a.ID, ctx.Err())
	case errors.Is(err, approval.ErrCanceled):
		e.Decision = approval.DecisionCanceled
		return denied("approval canceled", a.ID)
	default:
		e.Decision = string(approval.Deny)
		e.Notes = append(e.Notes, "approval wait failed: "+err.Error())
		return denied("approval failed", a.ID)
	}
}

// invoke runs the handler with the user in ctx and a timeout, converting a
// panic into ErrHandlerPanic (and disabling the integration).
//
// A handler that ignores ctx cannot be stopped: on timeout Dispatch returns
// but the handler's goroutine keeps running until it returns on its own; its
// eventual result is discarded.
func (d *Dispatcher) invoke(ctx context.Context, c Call, integ string, tool integration.Tool, args json.RawMessage) (integration.Result, error) {
	hctx, cancel := context.WithTimeout(integration.WithConversationID(integration.WithUserID(ctx, c.UserID), c.ConversationID), d.timeout)
	defer cancel()
	type outcome struct {
		res integration.Result
		err error
	}
	ch := make(chan outcome, 1) // buffered: a timed-out handler must not block forever
	go func() {
		defer func() {
			if p := recover(); p != nil {
				msg := fmt.Sprint(p)
				d.disable(integ, msg)
				d.log.Error("dispatch: handler panic; integration disabled",
					"integration", integ, "tool", tool.Name, "panic", msg, "stack", string(debug.Stack()))
				ch <- outcome{err: fmt.Errorf("%w: %s (integration %s disabled until restart)", ErrHandlerPanic, msg, integ)}
			}
		}()
		r, err := tool.Handler(hctx, args)
		ch <- outcome{r, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			if errors.Is(o.err, ErrHandlerPanic) {
				return integration.Result{}, o.err
			}
			if hctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				return integration.Result{}, fmt.Errorf("%w after %s: %w", ErrHandlerTimeout, d.timeout, o.err)
			}
			return integration.Result{}, fmt.Errorf("%s: %w", tool.Name, o.err)
		}
		return o.res, nil
	case <-hctx.Done():
		if ctx.Err() != nil {
			return integration.Result{}, fmt.Errorf("%s: %w", tool.Name, ctx.Err())
		}
		return integration.Result{}, fmt.Errorf("%w after %s", ErrHandlerTimeout, d.timeout)
	}
}

func (d *Dispatcher) runPreHooks(ctx context.Context, call *hooks.ToolCall, e *audit.Entry) error {
	if d.hk == nil {
		return nil
	}
	for _, h := range d.hk.PreToolCalls() {
		r, err := safePre(ctx, h, *call)
		if err != nil {
			return fmt.Errorf("%w: pre_tool_call %s: %v", ErrHook, h.Name, err)
		}
		for _, n := range r.AuditNotes {
			e.Notes = append(e.Notes, h.Name+": "+n)
		}
		if r.Cancel {
			e.Decision = "canceled"
			return &CanceledError{Tool: call.Tool, Hook: h.Name, Reason: r.Reason}
		}
		if r.Args != nil {
			if !json.Valid(r.Args) {
				return fmt.Errorf("%w: pre_tool_call %s returned invalid JSON args", ErrHook, h.Name)
			}
			e.Notes = append(e.Notes, h.Name+": rewrote args")
			call.Args = append(json.RawMessage(nil), r.Args...)
		}
	}
	return nil
}

func (d *Dispatcher) runPostHooks(ctx context.Context, call hooks.ToolCall, res integration.Result, e *audit.Entry) (integration.Result, error) {
	if d.hk == nil {
		return res, nil
	}
	untrusted := res.Untrusted
	for _, h := range d.hk.PostToolCalls() {
		r, err := safePost(ctx, h, call, res)
		if err != nil {
			// The action already ran; withhold the result rather than return
			// something a (possibly redacting) hook did not see through.
			return integration.Result{}, fmt.Errorf("%w: post_tool_call %s: %v (the tool ran; its result was withheld)", ErrHook, h.Name, err)
		}
		for _, n := range r.AuditNotes {
			e.Notes = append(e.Notes, h.Name+": "+n)
		}
		if r.Result != nil {
			res = *r.Result
			e.Notes = append(e.Notes, h.Name+": rewrote result")
		}
	}
	// Untrusted is sticky: a hook cannot launder outside content into
	// trusted content.
	res.Untrusted = res.Untrusted || untrusted
	return res, nil
}

func safePre(ctx context.Context, h hooks.Named[hooks.PreToolCall], call hooks.ToolCall) (r hooks.PreToolCallResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h.Fn(ctx, call)
}

func safePost(ctx context.Context, h hooks.Named[hooks.PostToolCall], call hooks.ToolCall, res integration.Result) (r hooks.PostToolCallResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h.Fn(ctx, call, res)
}

func (d *Dispatcher) record(e audit.Entry) {
	if d.aud == nil {
		d.log.Error("dispatch: no audit logger; entry dropped", "tool", e.Tool)
		return
	}
	// Record even if the caller's ctx is done (e.g. canceled while waiting
	// for approval): the outcome must still be logged.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.aud.Record(ctx, e); err != nil {
		d.log.Error("dispatch: audit record failed", "tool", e.Tool, "user_id", e.UserID, "err", err)
	}
}

func (d *Dispatcher) publishResult(c Call, res integration.Result, err error) {
	if d.bus == nil || c.UserID <= 0 {
		return
	}
	summary := audit.Summarize(res)
	if err != nil {
		summary = "error: " + strings.TrimSpace(err.Error())
	}
	d.bus.Publish(c.UserID, events.Event{Type: events.TypeToolResult, Data: events.ToolResultData{ID: c.ToolCallID, Summary: summary, ConversationID: c.ConversationID}})
}
