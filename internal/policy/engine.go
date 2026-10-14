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
	// Log receives fail-closed causes. Nil discards.
	Log *slog.Logger
}

// StoreEngine is the database-backed Engine.
//
// Precedence: explicit per-user rule > risk default > Deny. It reads the
// user and rule from the store on every call: there is no cache, so a rule
// change takes effect on the very next Decide. (Two indexed point lookups per
// tool call are negligible next to an LLM round trip.)
//
// Fail-closed: an invalid capability or risk, an unknown or disabled user, a
// store error, an invalid stored decision, or a rule carrying a Condition
// (conditions are not evaluated in v1) all yield Deny.
type StoreEngine struct {
	st    Store
	def   Defaults
	known func(integration.Capability) bool
	log   *slog.Logger
}

var _ Engine = (*StoreEngine)(nil)

// NewEngine returns a StoreEngine.
func NewEngine(st Store, opts Options) *StoreEngine {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &StoreEngine{st: st, def: opts.Defaults, known: opts.Known, log: log}
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

// Decide implements Engine.
func (e *StoreEngine) Decide(ctx context.Context, userID int64, c integration.Capability, risk integration.Risk, _ json.RawMessage) Decision {
	deny := func(reason string, args ...any) Decision {
		e.log.WarnContext(ctx, "policy: deny (fail closed)",
			append([]any{"reason", reason, "user_id", userID, "capability", string(c), "risk", string(risk)}, args...)...)
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
	r, err := e.st.GetPolicyRule(ctx, userID, string(c))
	switch {
	case err == nil:
		if hasCondition(r.Condition) {
			return deny("rule has a condition; conditions are not supported in v1", "rule_id", r.ID)
		}
		d := Decision(r.Decision)
		if !d.Valid() {
			return deny("stored rule has invalid decision", "rule_id", r.ID, "decision", r.Decision)
		}
		return d
	case errors.Is(err, store.ErrNotFound):
		return e.def.ForRisk(risk)
	default:
		return deny("store error reading rule", "err", err)
	}
}

func hasCondition(c json.RawMessage) bool {
	return len(c) > 0 && string(c) != "null"
}

func toRule(r *store.PolicyRule) Rule {
	return Rule{
		ID: r.ID, UserID: r.UserID, Capability: integration.Capability(r.Capability), Decision: Decision(r.Decision),
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

// ListRules returns userID's explicit rules ordered by capability.
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
