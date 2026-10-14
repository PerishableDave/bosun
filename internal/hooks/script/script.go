// Package script implements script hooks: admin-configured executables that
// run at a hook point with a JSON document on stdin and answer with JSON on
// stdout. They are the one deliberate exception to "no shell" in Bosun (see
// ARCHITECTURE.md, "Hooks"): off by default, config-only, admin-only.
//
// Protocol. The script receives one JSON object on stdin:
//
//	{"point": "pre_tool_call", "user_id": 1, "tool": "email.send",
//	 "integration": "email", "capability": "email.send", "risk": "write",
//	 "tool_call_id": "...", "args": {...},
//	 "result": {"content": "...", "untrusted": true},          // post_tool_call
//	 "model": "...", "system": "...", "messages": [...],       // pre_model_call
//	 "approval": {...}, "decision": "approve"}                 // on_approval
//
// and must print one JSON object on stdout:
//
//	pre_tool_call:  {"action": "continue"|"cancel", "args": <optional>, "reason": "..."}
//	post_tool_call: {"result": <optional replacement content string>}
//	pre_model_call: {"system": <optional string>, "messages": <optional array>}
//	on_approval:    anything (ignored)
//
// A non-zero exit, a timeout, oversized output, or invalid JSON fails closed:
// the tool call is not run (pre), its result is withheld (post), or the turn
// fails (pre_model_call). Every failure is recorded in the audit log as a
// "hook" entry. on_approval failures are logged and audited only.
//
// Scripts run with a minimal environment (PATH and HOME from the daemon),
// the daemon's working directory, and no secrets.
package script

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/hooks"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm"
)

// Limits.
const (
	DefaultTimeout = 5 * time.Second
	MaxOutputBytes = 8 << 20 // stdout
	maxStderrBytes = 4 << 10
	waitDelay      = time.Second
)

// Spec declares one script hook (the config form is
// config.ScriptHookConfig).
type Spec struct {
	Name    string // optional; derived from the command if empty
	Point   hooks.Point
	Command []string // argv; Command[0] must be an absolute path
	Timeout time.Duration
	Match   string // optional path.Match glob on tool or capability (tool points only)
}

// Options configure script hooks.
type Options struct {
	// Audit receives a KindHook entry for every failed run. Optional.
	Audit audit.Logger
	// Log receives operational messages (stderr of failed runs). Optional.
	Log *slog.Logger
	// Env is the child environment; nil means PATH and HOME from the
	// daemon's environment.
	Env []string
}

// Hook is one configured script hook.
type Hook struct {
	spec Spec
	name string
	env  []string
	aud  audit.Logger
	log  *slog.Logger
}

// New validates spec and returns a Hook. index (0-based) is used to derive a
// default name.
func New(spec Spec, index int, opts Options) (*Hook, error) {
	switch spec.Point {
	case hooks.PointPreToolCall, hooks.PointPostToolCall, hooks.PointPreModelCall, hooks.PointOnApproval:
	default:
		return nil, fmt.Errorf("script hook: unsupported point %q", spec.Point)
	}
	if len(spec.Command) == 0 || !filepath.IsAbs(spec.Command[0]) {
		return nil, fmt.Errorf("script hook: command must start with an absolute path")
	}
	if spec.Match != "" {
		if spec.Point != hooks.PointPreToolCall && spec.Point != hooks.PointPostToolCall {
			return nil, fmt.Errorf("script hook: match is only valid for tool hook points")
		}
		if _, err := path.Match(spec.Match, ""); err != nil {
			return nil, fmt.Errorf("script hook: match %q: %w", spec.Match, err)
		}
	}
	if spec.Timeout <= 0 {
		spec.Timeout = DefaultTimeout
	}
	h := &Hook{spec: spec, name: spec.Name, env: opts.Env, aud: opts.Audit, log: opts.Log}
	if h.name == "" {
		h.name = fmt.Sprintf("script-%d-%s", index+1, sanitize(filepath.Base(spec.Command[0])))
	}
	if h.env == nil {
		h.env = minimalEnv()
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h, nil
}

// Name returns the hook's registry name.
func (h *Hook) Name() string { return h.name }

// Spec returns the hook's (defaulted) spec.
func (h *Hook) Spec() Spec { return h.spec }

// Register adds h to reg at its point, typed hooks.TypeScript.
func (h *Hook) Register(reg *hooks.Registry) error {
	typ := hooks.AsType(hooks.TypeScript)
	switch h.spec.Point {
	case hooks.PointPreToolCall:
		return reg.AddPreToolCall(h.name, h.PreToolCall, typ)
	case hooks.PointPostToolCall:
		return reg.AddPostToolCall(h.name, h.PostToolCall, typ)
	case hooks.PointPreModelCall:
		return reg.AddPreModelCall(h.name, h.PreModelCall, typ)
	case hooks.PointOnApproval:
		return reg.AddOnApproval(h.name, h.OnApproval, typ)
	}
	return fmt.Errorf("script hook %s: unsupported point %q", h.name, h.spec.Point)
}

// Register builds and registers every spec, returning the hooks in order. It
// stops at the first error.
func Register(reg *hooks.Registry, specs []Spec, opts Options) ([]*Hook, error) {
	out := make([]*Hook, 0, len(specs))
	for i, s := range specs {
		h, err := New(s, i, opts)
		if err != nil {
			return out, err
		}
		if err := h.Register(reg); err != nil {
			return out, err
		}
		out = append(out, h)
	}
	return out, nil
}

// input is the JSON document written to the script's stdin.
type input struct {
	Point       hooks.Point        `json:"point"`
	UserID      int64              `json:"user_id"`
	Tool        string             `json:"tool,omitempty"`
	Integration string             `json:"integration,omitempty"`
	Capability  string             `json:"capability,omitempty"`
	Risk        string             `json:"risk,omitempty"`
	ToolCallID  string             `json:"tool_call_id,omitempty"`
	Args        json.RawMessage    `json:"args,omitempty"`
	Result      *resultIn          `json:"result,omitempty"`
	Model       string             `json:"model,omitempty"`
	System      *string            `json:"system,omitempty"`
	Messages    []llm.Message      `json:"messages,omitempty"`
	Approval    *approval.Approval `json:"approval,omitempty"`
	Decision    string             `json:"decision,omitempty"`
}

type resultIn struct {
	Content   string `json:"content"`
	Untrusted bool   `json:"untrusted"`
}

func toolInput(p hooks.Point, c hooks.ToolCall) input {
	return input{Point: p, UserID: c.UserID, Tool: c.Tool, Integration: c.Integration, Capability: string(c.Capability),
		Risk: string(c.Risk), ToolCallID: c.ID, Args: c.Args}
}

// matches reports whether a tool hook applies to call.
func (h *Hook) matches(c hooks.ToolCall) bool {
	if h.spec.Match == "" {
		return true
	}
	ok1, _ := path.Match(h.spec.Match, c.Tool)
	ok2, _ := path.Match(h.spec.Match, string(c.Capability))
	return ok1 || ok2
}

// PreToolCall implements hooks.PreToolCall.
func (h *Hook) PreToolCall(ctx context.Context, c hooks.ToolCall) (hooks.PreToolCallResult, error) {
	if !h.matches(c) {
		return hooks.PreToolCallResult{}, nil
	}
	in := toolInput(hooks.PointPreToolCall, c)
	out, err := h.run(ctx, in)
	if err != nil {
		return hooks.PreToolCallResult{}, h.fail(in, err)
	}
	var resp struct {
		Action string          `json:"action"`
		Args   json.RawMessage `json:"args"`
		Reason string          `json:"reason"`
	}
	if err := decodeObject(out, &resp); err != nil {
		return hooks.PreToolCallResult{}, h.fail(in, err)
	}
	switch resp.Action {
	case "continue":
		r := hooks.PreToolCallResult{}
		if present(resp.Args) {
			r.Args = resp.Args
		}
		return r, nil
	case "cancel":
		return hooks.PreToolCallResult{Cancel: true, Reason: resp.Reason}, nil
	default:
		return hooks.PreToolCallResult{}, h.fail(in, fmt.Errorf("invalid output: action must be \"continue\" or \"cancel\", got %q", resp.Action))
	}
}

// PostToolCall implements hooks.PostToolCall. A rewritten result keeps the
// original's Untrusted flag and drops its structured Data (which may hold
// what the script meant to remove).
func (h *Hook) PostToolCall(ctx context.Context, c hooks.ToolCall, res integration.Result) (hooks.PostToolCallResult, error) {
	if !h.matches(c) {
		return hooks.PostToolCallResult{}, nil
	}
	in := toolInput(hooks.PointPostToolCall, c)
	in.Result = &resultIn{Content: res.Content, Untrusted: res.Untrusted}
	out, err := h.run(ctx, in)
	if err != nil {
		return hooks.PostToolCallResult{}, h.fail(in, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := decodeObject(out, &resp); err != nil {
		return hooks.PostToolCallResult{}, h.fail(in, err)
	}
	if !present(resp.Result) {
		return hooks.PostToolCallResult{}, nil
	}
	var content string
	if err := json.Unmarshal(resp.Result, &content); err != nil {
		return hooks.PostToolCallResult{}, h.fail(in, errors.New("invalid output: result must be a string"))
	}
	return hooks.PostToolCallResult{Result: &integration.Result{Content: content, Untrusted: res.Untrusted}}, nil
}

// PreModelCall implements hooks.PreModelCall.
func (h *Hook) PreModelCall(ctx context.Context, req *llm.Request) error {
	uid, _ := hooks.UserIDFromContext(ctx)
	sys := req.System
	in := input{Point: hooks.PointPreModelCall, UserID: uid, Model: req.Model, System: &sys, Messages: req.Messages}
	out, err := h.run(ctx, in)
	if err != nil {
		return h.fail(in, err)
	}
	var resp struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := decodeObject(out, &resp); err != nil {
		return h.fail(in, err)
	}
	var newSys *string
	if present(resp.System) {
		var s string
		if err := json.Unmarshal(resp.System, &s); err != nil {
			return h.fail(in, errors.New("invalid output: system must be a string"))
		}
		newSys = &s
	}
	var newMsgs []llm.Message
	if present(resp.Messages) {
		if err := json.Unmarshal(resp.Messages, &newMsgs); err != nil {
			return h.fail(in, fmt.Errorf("invalid output: messages: %v", err))
		}
		if len(newMsgs) == 0 {
			return h.fail(in, errors.New("invalid output: messages must not be empty"))
		}
		for i, m := range newMsgs {
			if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant && m.Role != llm.RoleTool {
				return h.fail(in, fmt.Errorf("invalid output: messages[%d]: bad role %q", i, m.Role))
			}
		}
	}
	if newSys != nil {
		req.System = *newSys
	}
	if newMsgs != nil {
		req.Messages = newMsgs
	}
	return nil
}

// OnApproval implements hooks.OnApproval. Output is ignored; failures are
// logged and audited.
func (h *Hook) OnApproval(ctx context.Context, a approval.Approval, d approval.Decision) {
	in := input{Point: hooks.PointOnApproval, UserID: a.UserID, Tool: a.Tool, Capability: string(a.Capability),
		Risk: string(a.Risk), ToolCallID: a.ToolCallID, Args: a.Args, Approval: &a, Decision: string(d)}
	if _, err := h.run(ctx, in); err != nil {
		_ = h.fail(in, err)
	}
}

// run executes the script with in on stdin and returns its stdout.
func (h *Hook) run(ctx context.Context, in input) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, h.spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, h.spec.Command[0], h.spec.Command[1:]...)
	cmd.Env = h.env
	cmd.Stdin = bytes.NewReader(b)
	stdout := &capBuffer{max: MaxOutputBytes}
	stderr := &capBuffer{max: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A grandchild holding the pipes open must not hang the call.
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	if se := strings.TrimSpace(stderr.String()); se != "" {
		h.log.Warn("script hook stderr", "hook", h.name, "stderr", se)
	}
	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return nil, fmt.Errorf("timed out after %s", h.spec.Timeout)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("script failed: %v", err)
	case stdout.overflow:
		return nil, fmt.Errorf("output exceeds %d bytes", MaxOutputBytes)
	}
	return stdout.Bytes(), nil
}

// fail records the failure and returns the error the hook reports.
func (h *Hook) fail(in input, err error) error {
	h.log.Warn("script hook failed", "hook", h.name, "point", in.Point, "user_id", in.UserID, "tool", in.Tool, "err", err)
	if h.aud != nil {
		detail, _ := json.Marshal(map[string]any{"hook": h.name, "point": in.Point, "command": h.spec.Command[0]})
		e := audit.Entry{UserID: in.UserID, Kind: audit.KindHook, Tool: in.Tool, Capability: integration.Capability(in.Capability),
			Risk: integration.Risk(in.Risk), Error: err.Error(), Notes: []string{"script hook " + h.name + " failed (fail closed)"}, Detail: detail}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if aerr := h.aud.Record(ctx, e); aerr != nil {
			h.log.Error("script hook: audit failed", "hook", h.name, "err", aerr)
		}
	}
	return fmt.Errorf("script hook %s: %w", h.name, err)
}

func decodeObject(b []byte, v any) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return errors.New("invalid output: expected a JSON object on stdout")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid output: %v", err)
	}
	if dec.More() {
		return errors.New("invalid output: trailing data after JSON object")
	}
	return nil
}

func present(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

func minimalEnv() []string {
	env := []string{}
	p := os.Getenv("PATH")
	if p == "" {
		p = "/usr/local/bin:/usr/bin:/bin"
	}
	env = append(env, "PATH="+p)
	if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+home)
	}
	return env
}

var unsafeName = regexp.MustCompile(`[^a-z0-9_-]+`)

func sanitize(s string) string {
	s = unsafeName.ReplaceAllString(strings.ToLower(s), "-")
	if s = strings.Trim(s, "-"); s == "" {
		return "hook"
	}
	return s
}

// capBuffer keeps at most max bytes and records overflow.
type capBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room < len(p) {
		c.overflow = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
