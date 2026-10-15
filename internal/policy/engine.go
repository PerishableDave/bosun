package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

// Store is the subset of *store.Store the engine needs. It is an interface so
// tests can inject failures; *store.Store satisfies it.
type Store interface {
	GetUser(ctx context.Context, id int64) (*store.User, error)
	GetUserByUsername(ctx context.Context, username string) (*store.User, error)
	GetPolicyRule(ctx context.Context, userID int64, capability string) (*store.PolicyRule, error)
	ListPolicyRules(ctx context.Context, userID int64) ([]*store.PolicyRule, error)
	UpsertPolicyRule(ctx context.Context, userID int64, capability, decision string, condition json.RawMessage) (*store.PolicyRule, error)
	DeletePolicyRule(ctx context.Context, userID int64, capability string) error
	GetScopedPolicyRule(ctx context.Context, userID int64, capability, scope string) (*store.PolicyRule, error)
	UpsertScopedPolicyRule(ctx context.Context, userID int64, capability, scope, decision string, condition json.RawMessage) (*store.PolicyRule, error)
	DeleteScopedPolicyRule(ctx context.Context, userID int64, capability, scope string) error
}

var _ Store = (*store.Store)(nil)

// capabilityRe mirrors the registry's syntax for capability names.
var capabilityRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9][a-z0-9_-]*)+$`)

// ValidCapability reports whether c is syntactically a capability name.
func ValidCapability(c integration.Capability) bool { return capabilityRe.MatchString(string(c)) }

// ErrInvalid is returned by rule-management methods for a bad capability or
// decision.
var ErrInvalid = errors.New("policy: invalid rule")

// Options configure a StoreEngine.
type Options struct {
	// Defaults are the per-risk decisions used when no explicit rule matches.
	// An unset or invalid entry means Deny for that risk.
	Defaults Defaults
	// Known, if set, reports whether a capability is declared by some
	// registered integration. Capabilities for which it returns false are
	// denied regardless of rules. When nil only syntax is checked.
	Known func(integration.Capability) bool
	// CapabilityDefault, if set, returns the decision an integration's
	// manifest declares for a capability (integration.Manifest.
	// DefaultDecisions). It applies when the user has no rule for the
	// capability, before the risk default. An invalid decision is Deny.
	CapabilityDefault func(integration.Capability) (Decision, bool)
	// Log receives fail-closed causes. Nil discards.
	Log *slog.Logger
}

// StoreEngine is the database-backed Engine.
//
// Precedence: scoped rule (capability, scope) > capability-wide rule
// (capability, "") > integration default (Options.CapabilityDefault) >
// risk default > Deny. A scoped lookup tries the scope
// and then its parent domains (integration.ScopeCandidates), so a rule for
// "opentable.com" covers "m.opentable.com". It reads the
// user and rule from the store on every call: there is no cache, so a rule
// change takes effect on the very next Decide. (Two indexed point lookups per
// tool call are negligible next to an LLM round trip.)
//
// Fail-closed: an invalid capability or risk, an unknown or disabled user, a
// store error, an invalid stored decision, or a rule carrying a Condition
// (conditions are not evaluated in v1) all yield Deny.
type StoreEngine struct {
	st     Store
	def    Defaults
	known  func(integration.Capability) bool
	capDef func(integration.Capability) (Decision, bool)
	log    *slog.Logger
}

var (
	_ Engine       = (*StoreEngine)(nil)
	_ ScopedEngine = (*StoreEngine)(nil)
)

// NewEngine returns a StoreEngine.
func NewEngine(st Store, opts Options) *StoreEngine {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &StoreEngine{st: st, def: opts.Defaults, known: opts.Known, capDef: opts.CapabilityDefault, log: log}
}

// DefaultsFromConfig converts the config form of risk defaults. Invalid values
// are rejected (config.Validate already rejects them; this is belt and braces).
func DefaultsFromConfig(c config.PolicyDefaults) (Defaults, error) {
	d := Defaults{Read: Decision(c.Read), Write: Decision(c.Write), Destructive: Decision(c.Destructive)}
	for risk, v := range map[string]Decision{"read": d.Read, "write": d.Write, "destructive": d.Destructive} {
		if !v.Valid() {
			return Defaults{}, fmt.Errorf("policy.defaults.%s %q: %w", risk, v, ErrInvalid)
		}
	}
	return d, nil
}

// Decide implements Engine. It is DecideScoped with no scope.
func (e *StoreEngine) Decide(ctx context.Context, userID int64, c integration.Capability, risk integration.Risk, args json.RawMessage) Decision {
	return e.DecideScoped(ctx, userID, c, "", risk, args)
}

// DecideScoped implements ScopedEngine.
func (e *StoreEngine) DecideScoped(ctx context.Context, userID int64, c integration.Capability, scope string, risk integration.Risk, _ json.RawMessage) Decision {
	deny := func(reason string, args ...any) Decision {
		e.log.WarnContext(ctx, "policy: deny (fail closed)",
			append([]any{"reason", reason, "user_id", userID, "capability", string(c), "scope", scope, "risk", string(risk)}, args...)...)
		return Deny
	}
	if userID <= 0 {
		return deny("invalid user id")
	}
	if !ValidCapability(c) {
		return deny("invalid capability")
	}
	if !risk.Valid() {
		return deny("invalid risk")
	}
	if e.known != nil && !e.known(c) {
		return deny("unknown capability")
	}
	if err := ctx.Err(); err != nil {
		return deny("context done", "err", err)
	}
	u, err := e.st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return deny("unknown user")
		}
		return deny("store error reading user", "err", err)
	}
	if u.Disabled {
		return deny("user disabled")
	}
	ruleDecision := func(r *store.PolicyRule) Decision {
		if hasCondition(r.Condition) {
			return deny("rule has a condition; conditions are not supported in v1", "rule_id", r.ID)
		}
		d := Decision(r.Decision)
		if !d.Valid() {
			return deny("stored rule has invalid decision", "rule_id", r.ID, "decision", r.Decision)
		}
		return d
	}
	for _, sc := range integration.ScopeCandidates(scope) {
		r, err := e.st.GetScopedPolicyRule(ctx, userID, string(c), sc)
		switch {
		case err == nil:
			return ruleDecision(r)
		case errors.Is(err, store.ErrNotFound):
		default:
			return deny("store error reading scoped rule", "err", err)
		}
	}
	r, err := e.st.GetPolicyRule(ctx, userID, string(c))
	switch {
	case err == nil:
		return ruleDecision(r)
	case errors.Is(err, store.ErrNotFound):
		return e.defaultFor(c, risk)
	default:
		return deny("store error reading rule", "err", err)
	}
}

// defaultFor is the decision without a rule: the integration's default for
// c, else the risk default.
func (e *StoreEngine) defaultFor(c integration.Capability, risk integration.Risk) Decision {
	if e.capDef != nil {
		if d, ok := e.capDef(c); ok {
			if !d.Valid() {
				e.log.Warn("policy: deny (fail closed)", "reason", "invalid integration default", "capability", string(c), "decision", string(d))
				return Deny
			}
			// An integration may only tighten the admin's risk default,
			// never loosen it: a manifest declaring "allow" for a write
			// capability still asks when the admin's write default is ask.
			return stricter(d, e.def.ForRisk(risk))
		}
	}
	return e.def.ForRisk(risk)
}

// strictness orders decisions from most to least permissive.
var strictness = map[Decision]int{Allow: 0, Ask: 1, Deny: 2}

// stricter returns the less permissive of a and b.
func stricter(a, b Decision) Decision {
	if strictness[b] > strictness[a] {
		return b
	}
	return a
}

func hasCondition(c json.RawMessage) bool {
	return len(c) > 0 && string(c) != "null"
}

func toRule(r *store.PolicyRule) Rule {
	return Rule{
		ID: r.ID, UserID: r.UserID, Capability: integration.Capability(r.Capability), Scope: r.Scope, Decision: Decision(r.Decision),
		Condition: r.Condition, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// SetRule creates or replaces userID's unconditional rule for c.
func (e *StoreEngine) SetRule(ctx context.Context, userID int64, c integration.Capability, d Decision) (Rule, error) {
	if !ValidCapability(c) {
		return Rule{}, fmt.Errorf("set rule: capability %q: %w", c, ErrInvalid)
	}
	if !d.Valid() {
		return Rule{}, fmt.Errorf("set rule: decision %q: %w", d, ErrInvalid)
	}
	r, err := e.st.UpsertPolicyRule(ctx, userID, string(c), string(d), nil)
	if err != nil {
		return Rule{}, fmt.Errorf("set rule: %w", err)
	}
	return toRule(r), nil
}

// DeleteRule removes userID's rule for c, reverting to the risk default.
// Returns an error wrapping store.ErrNotFound if there was no rule.
func (e *StoreEngine) DeleteRule(ctx context.Context, userID int64, c integration.Capability) error {
	if err := e.st.DeletePolicyRule(ctx, userID, string(c)); err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// ValidScope reports whether s is a normalized, non-empty scope (see
// integration.NormalizeScope).
func ValidScope(s string) bool { return s != "" && integration.NormalizeScope(s) == s }

// SetScopedRule creates or replaces userID's rule for c limited to scope.
// An empty scope is the capability-wide rule (SetRule); a non-empty scope
// must be normalized (ValidScope).
func (e *StoreEngine) SetScopedRule(ctx context.Context, userID int64, c integration.Capability, scope string, d Decision) (Rule, error) {
	if scope != "" && !ValidScope(scope) {
		return Rule{}, fmt.Errorf("set rule: scope %q: %w", scope, ErrInvalid)
	}
	if !ValidCapability(c) {
		return Rule{}, fmt.Errorf("set rule: capability %q: %w", c, ErrInvalid)
	}
	if !d.Valid() {
		return Rule{}, fmt.Errorf("set rule: decision %q: %w", d, ErrInvalid)
	}
	r, err := e.st.UpsertScopedPolicyRule(ctx, userID, string(c), scope, string(d), nil)
	if err != nil {
		return Rule{}, fmt.Errorf("set rule: %w", err)
	}
	return toRule(r), nil
}

// DeleteScopedRule removes userID's rule for c limited to scope ("" is the
// capability-wide rule). Returns an error wrapping store.ErrNotFound if
// there was no such rule.
func (e *StoreEngine) DeleteScopedRule(ctx context.Context, userID int64, c integration.Capability, scope string) error {
	if err := e.st.DeleteScopedPolicyRule(ctx, userID, string(c), scope); err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// ListRules returns userID's explicit rules ordered by capability, then
// scope (capability-wide first).
func (e *StoreEngine) ListRules(ctx context.Context, userID int64) ([]Rule, error) {
	rs, err := e.st.ListPolicyRules(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	out := make([]Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, toRule(r))
	}
	return out, nil
}

// SeedOptions control SeedRules.
type SeedOptions struct {
	// Overwrite replaces existing DB rules with the config value. When false
	// (the default) only missing rules are created, so a rule the user later
	// changes in the UI/CLI (which may be stricter than the seed) is never
	// reverted on restart. Either way the operation is idempotent.
	Overwrite bool
	Log       *slog.Logger
}

// SeedReport summarises a SeedRules run.
type SeedReport struct {
	Created      int
	Updated      int
	Unchanged    int
	Kept         int      // existing rule differed from config and Overwrite was false
	MissingUsers []string // usernames in config with no DB user (skipped)
}

// SeedRules loads the users.<name>.rules section of bosun.yaml into
// policy_rules. Users that do not exist yet are skipped and reported (they
// are typically created later by `bosun setup`; seeding runs at every start).
// Invalid capabilities or decisions abort before anything is written.
func SeedRules(ctx context.Context, st Store, users map[string]config.UserSeed, opts SeedOptions) (SeedReport, error) {
	var rep SeedReport
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
		for capName, d := range users[n].Rules {
			if !ValidCapability(integration.Capability(capName)) {
				return rep, fmt.Errorf("seed rules: users.%s.rules.%s: capability: %w", n, capName, ErrInvalid)
			}
			if !Decision(d).Valid() {
				return rep, fmt.Errorf("seed rules: users.%s.rules.%s %q: decision: %w", n, capName, d, ErrInvalid)
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		u, err := st.GetUserByUsername(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			log.WarnContext(ctx, "policy seed: user not found; skipping", "user", name)
			rep.MissingUsers = append(rep.MissingUsers, name)
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("seed rules for %s: %w", name, err)
		}
		caps := make([]string, 0, len(users[name].Rules))
		for c := range users[name].Rules {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		for _, c := range caps {
			want := users[name].Rules[c]
			cur, err := st.GetPolicyRule(ctx, u.ID, c)
			switch {
			case errors.Is(err, store.ErrNotFound):
				if _, err := st.UpsertPolicyRule(ctx, u.ID, c, want, nil); err != nil {
					return rep, fmt.Errorf("seed rules for %s: %w", name, err)
				}
				rep.Created++
			case err != nil:
				return rep, fmt.Errorf("seed rules for %s: %w", name, err)
			case cur.Decision == want && !hasCondition(cur.Condition):
				rep.Unchanged++
			case !opts.Overwrite:
				log.InfoContext(ctx, "policy seed: keeping existing rule that differs from config",
					"user", name, "capability", c, "db", cur.Decision, "config", want)
				rep.Kept++
			default:
				if _, err := st.UpsertPolicyRule(ctx, u.ID, c, want, nil); err != nil {
					return rep, fmt.Errorf("seed rules for %s: %w", name, err)
				}
				rep.Updated++
			}
		}
	}
	return rep, nil
}
