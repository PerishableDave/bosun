// Package approval defines the human-approval flow for tool calls that policy
// marks "ask". This package holds types only; the Manager implementation is
// built separately.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Status is an approval's lifecycle state.
type Status string

// Statuses. Pending is the only non-terminal state.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
	StatusExpired  Status = "expired"  // timed out; treated as denied
	StatusCanceled Status = "canceled" // the waiting run went away
)

// Decision is a user's answer. The JSON values match the API body
// {"decision":"approve"|"deny"}.
type Decision string

// Decisions.
const (
	Approve Decision = "approve"
	Deny    Decision = "deny"
)

// Valid reports whether d is a known decision.
func (d Decision) Valid() bool { return d == Approve || d == Deny }

// Approval is a pending or resolved request for a human to allow a tool call.
type Approval struct {
	ID             string                 `json:"id"`
	UserID         int64                  `json:"user_id"`
	ConversationID string                 `json:"conversation_id,omitempty"`
	ToolCallID     string                 `json:"tool_call_id,omitempty"`
	Tool           string                 `json:"tool"`
	Capability     integration.Capability `json:"capability"`
	Risk           integration.Risk       `json:"risk"`
	Args           json.RawMessage        `json:"args"`
	Status         Status                 `json:"status"`
	CreatedAt      time.Time              `json:"created_at"`
	ExpiresAt      time.Time              `json:"expires_at"`
	ResolvedAt     time.Time              `json:"resolved_at,omitzero"`
	ResolvedBy     int64                  `json:"resolved_by,omitempty"`
}

// Params describe a new approval request.
type Params struct {
	UserID         int64
	ConversationID string
	ToolCallID     string
	Tool           string
	Capability     integration.Capability
	Risk           integration.Risk
	Args           json.RawMessage
	// Timeout overrides the configured policy.approval_timeout when > 0.
	Timeout time.Duration
}

// Errors returned by Manager implementations.
var (
	ErrNotFound        = errors.New("approval: not found")
	ErrAlreadyResolved = errors.New("approval: already resolved") // first answer wins
	ErrExpired         = errors.New("approval: expired")
	ErrForbidden       = errors.New("approval: not owned by user")
)

// Manager creates approvals, delivers them to the user's clients (as
// approval_required events), and blocks tool calls until answered.
type Manager interface {
	// Request persists a pending approval and notifies the user's clients.
	Request(ctx context.Context, p Params) (*Approval, error)
	// Wait blocks until the approval is resolved, expires, or ctx ends.
	// On expiry it returns (Deny, ErrExpired). If ctx ends first the
	// approval is canceled and ctx.Err() is returned with Deny.
	Wait(ctx context.Context, id string) (Decision, error)
	// Resolve records userID's decision. Only the owning user may resolve;
	// otherwise ErrForbidden (implementations may return ErrNotFound to avoid
	// leaking existence). A second resolution returns ErrAlreadyResolved.
	Resolve(ctx context.Context, id string, userID int64, d Decision) error
	// ListPending returns the user's unexpired pending approvals.
	ListPending(ctx context.Context, userID int64) ([]*Approval, error)
}
