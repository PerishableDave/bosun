// Package policy defines how Bosun decides whether a tool call may run:
// per user × capability, explicit rule > risk default > deny. Unknown
// capabilities are denied. This package holds types only; the Engine
// implementation is built separately.
package policy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Decision is the outcome of a policy check.
type Decision string

// Decisions.
const (
	Allow Decision = "allow" // run without asking
	Ask   Decision = "ask"   // require human approval first
	Deny  Decision = "deny"  // refuse
)

// Valid reports whether d is a known decision.
func (d Decision) Valid() bool { return d == Allow || d == Ask || d == Deny }

// Rule is an explicit per-user decision for one capability.
type Rule struct {
	ID         int64
	UserID     int64
	Capability integration.Capability
	Decision   Decision
	// Condition is reserved for argument conditions (e.g. email.send only to
	// @family.example). v1 ignores it; nil means unconditional.
	Condition json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Defaults are the per-risk decisions applied when no explicit rule matches.
type Defaults struct {
	Read        Decision
	Write       Decision
	Destructive Decision
}

// ForRisk returns the default for risk, or Deny for an unknown risk.
func (d Defaults) ForRisk(r integration.Risk) Decision {
	var out Decision
	switch r {
	case integration.RiskRead:
		out = d.Read
	case integration.RiskWrite:
		out = d.Write
	case integration.RiskDestructive:
		out = d.Destructive
	}
	if !out.Valid() {
		return Deny
	}
	return out
}

// Engine decides whether userID may exercise capability c at the given risk
// with args.
//
// Implementations must be fail-closed: any internal error (e.g. a DB failure),
// unknown capability, or unknown user yields Deny. Decide therefore returns
// no error; implementations should log/audit the cause.
type Engine interface {
	Decide(ctx context.Context, userID int64, c integration.Capability, risk integration.Risk, args json.RawMessage) Decision
}
