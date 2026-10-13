// Package audit defines the append-only action log. Every tool call,
// approval, and outbound LLM request is recorded. This package holds types
// only; the Logger implementation is built separately.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Kind classifies an audit entry.
type Kind string

// Entry kinds.
const (
	KindToolCall   Kind = "tool_call"   // a tool call, whatever the outcome
	KindApproval   Kind = "approval"    // an approval was requested/resolved/expired
	KindLLMRequest Kind = "llm_request" // an outbound model request (metadata only unless audit.log_content)
	KindOutbound   Kind = "outbound"    // another outbound network call
	KindAuth       Kind = "auth"        // login, logout, user/role changes
	KindConfig     Kind = "config"      // policy rule or integration changes
	KindHook       Kind = "hook"        // a hook failed (script hooks record their failures)
)

// Entry is one audit record.
type Entry struct {
	ID          int64     // set by the logger
	Time        time.Time // zero means now
	UserID      int64     // 0 for system
	Kind        Kind
	Integration string
	Tool        string
	Capability  integration.Capability
	Risk        integration.Risk
	// Decision is the policy decision ("allow" | "ask" | "deny") or approval
	// outcome ("approve" | "deny" | "expired") as a string, to keep this
	// package independent of policy and approval.
	Decision      string
	ApprovalID    string
	ApproverID    int64
	Args          json.RawMessage
	ResultSummary string
	Error         string
	Notes         []string        // e.g. annotations added by hooks
	Detail        json.RawMessage // kind-specific, e.g. {"provider","model","input_tokens","output_tokens"}
}

// Logger records audit entries. Record must not be skipped on failure paths:
// the dispatcher records every call regardless of outcome.
type Logger interface {
	Record(ctx context.Context, e Entry) error
}
