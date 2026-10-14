package policy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func mkUser(t *testing.T, s *store.Store, name string) *store.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "h", "member")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var stdDefaults = Defaults{Read: Allow, Write: Ask, Destructive: Deny}

func TestDecidePrecedence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(s, Options{Defaults: stdDefaults})

	// No rule: risk defaults.
	for risk, want := range map[integration.Risk]Decision{
		integration.RiskRead: Allow, integration.RiskWrite: Ask, integration.RiskDestructive: Deny,
	} {
		if got := e.Decide(ctx, u.ID, "email.send", risk, nil); got != want {
			t.Errorf("default %s = %s, want %s", risk, got, want)
		}
	}
	// Explicit rule beats the default in both directions.
	if _, err := e.SetRule(ctx, u.ID, "email.send", Allow); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(ctx, u.ID, "email.send", integration.RiskDestructive, nil); got != Allow {
		t.Errorf("explicit allow over destructive default = %s", got)
	}
	if _, err := e.SetRule(ctx, u.ID, "calendar.read", Deny); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(ctx, u.ID, "calendar.read", integration.RiskRead, nil); got != Deny {
		t.Errorf("explicit deny over read default = %s", got)
	}
	if _, err := e.SetRule(ctx, u.ID, "calendar.read", Ask); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(ctx, u.ID, "calendar.read", integration.RiskRead, nil); got != Ask {
		t.Errorf("rule update not seen immediately: %s", got)
	}
	// Rules are per user.
	other := mkUser(t, s, "kid")
	if got := e.Decide(ctx, other.ID, "email.send", integration.RiskWrite, nil); got != Ask {
		t.Errorf("other user should get default, got %s", got)
	}
	// Delete reverts to default.
	if err := e.DeleteRule(ctx, u.ID, "email.send"); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(ctx, u.ID, "email.send", integration.RiskWrite, nil); got != Ask {
		t.Errorf("after delete = %s", got)
	}
	if err := e.DeleteRule(ctx, u.ID, "email.send"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}
	rules, err := e.ListRules(ctx, u.ID)
	if err != nil || len(rules) != 1 || rules[0].Capability != "calendar.read" || rules[0].Decision != Ask {
		t.Errorf("ListRules = %+v, %v", rules, err)
	}
}

func TestDecideNoDefaultsDenies(t *testing.T) {
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(s, Options{})
	if got := e.Decide(context.Background(), u.ID, "x.read", integration.RiskRead, nil); got != Deny {
		t.Errorf("unset default = %s", got)
	}
}

func TestDecideFailClosedInputs(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(s, Options{Defaults: Defaults{Read: Allow, Write: Allow, Destructive: Allow},
		Known: func(c integration.Capability) bool { return c != "ghost.read" }})
	if _, err := e.SetRule(ctx, u.ID, "ghost.read", Allow); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		user int64
		cap  integration.Capability
		risk integration.Risk
	}{
		{"invalid capability", u.ID, "Not A Cap", integration.RiskRead},
		{"empty capability", u.ID, "", integration.RiskRead},
		{"no namespace", u.ID, "email", integration.RiskRead},
		{"invalid risk", u.ID, "email.read", "spicy"},
		{"unknown capability (even with allow rule)", u.ID, "ghost.read", integration.RiskRead},
		{"unknown user", 9999, "email.read", integration.RiskRead},
		{"zero user", 0, "email.read", integration.RiskRead},
	}
	for _, c := range cases {
		if got := e.Decide(ctx, c.user, c.cap, c.risk, nil); got != Deny {
			t.Errorf("%s: got %s, want deny", c.name, got)
		}
	}
	if err := s.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(ctx, u.ID, "email.read", integration.RiskRead, nil); got != Deny {
		t.Errorf("disabled user: %s", got)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := e.Decide(cctx, u.ID, "email.read", integration.RiskRead, nil); got != Deny {
		t.Errorf("canceled ctx: %s", got)
	}
}

func TestDecideConditionalRuleDenies(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	if _, err := s.UpsertPolicyRule(ctx, u.ID, "email.send", "allow", json.RawMessage(`{"to_domain":"family.example"}`)); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, Options{Defaults: Defaults{Read: Allow, Write: Allow, Destructive: Allow}})
	if got := e.Decide(ctx, u.ID, "email.send", integration.RiskWrite, nil); got != Deny {
		t.Errorf("conditional rule = %s, want deny", got)
	}
}

// faultyStore wraps a real store and injects errors.
type faultyStore struct {
	Store
	userErr, ruleErr error
	badDecision      bool
}

func (f *faultyStore) GetUser(ctx context.Context, id int64) (*store.User, error) {
	if f.userErr != nil {
		return nil, f.userErr
	}
	return f.Store.GetUser(ctx, id)
}

func (f *faultyStore) GetPolicyRule(ctx context.Context, uid int64, c string) (*store.PolicyRule, error) {
	if f.ruleErr != nil {
		return nil, f.ruleErr
	}
	if f.badDecision {
		return &store.PolicyRule{ID: 1, UserID: uid, Capability: c, Decision: "maybe"}, nil
	}
	return f.Store.GetPolicyRule(ctx, uid, c)
}

func TestDecideStoreErrorsDeny(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	allowAll := Defaults{Read: Allow, Write: Allow, Destructive: Allow}
	boom := errors.New("disk on fire")
	for name, fs := range map[string]*faultyStore{
		"user lookup error": {Store: s, userErr: boom},
		"rule lookup error": {Store: s, ruleErr: boom},
		"invalid decision":  {Store: s, badDecision: true},
	} {
		e := NewEngine(fs, Options{Defaults: allowAll})
		if got := e.Decide(ctx, u.ID, "email.read", integration.RiskRead, nil); got != Deny {
			t.Errorf("%s: %s, want deny", name, got)
		}
	}
	// A closed database also denies.
	s.Close()
	e := NewEngine(s, Options{Defaults: allowAll})
	if got := e.Decide(ctx, u.ID, "email.read", integration.RiskRead, nil); got != Deny {
		t.Errorf("closed db: %s", got)
	}
}

func TestSetRuleValidation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(s, Options{})
	if _, err := e.SetRule(ctx, u.ID, "bad cap", Allow); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad cap: %v", err)
	}
	if _, err := e.SetRule(ctx, u.ID, "email.send", "sure"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad decision: %v", err)
	}
}

func TestDefaultsFromConfig(t *testing.T) {
	d, err := DefaultsFromConfig(config.PolicyDefaults{Read: "allow", Write: "ask", Destructive: "deny"})
	if err != nil || d != (Defaults{Allow, Ask, Deny}) {
		t.Errorf("%+v %v", d, err)
	}
	if _, err := DefaultsFromConfig(config.PolicyDefaults{Read: "allow", Write: "yes", Destructive: "deny"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid: %v", err)
	}
}

func TestSeedRules(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	seed := map[string]config.UserSeed{
		"dave":  {Rules: map[string]string{"calendar.read": "allow", "email.delete": "deny"}},
		"ghost": {Rules: map[string]string{"calendar.read": "allow"}},
	}
	rep, err := SeedRules(ctx, s, seed, SeedOptions{})
	if err != nil || rep.Created != 2 || len(rep.MissingUsers) != 1 || rep.MissingUsers[0] != "ghost" {
		t.Fatalf("first seed: %+v %v", rep, err)
	}
	rep, err = SeedRules(ctx, s, seed, SeedOptions{})
	if err != nil || rep.Created != 0 || rep.Unchanged != 2 {
		t.Fatalf("idempotent seed: %+v %v", rep, err)
	}
	// User tightens a rule in the UI; restart must not loosen it.
	e := NewEngine(s, Options{})
	if _, err := e.SetRule(ctx, u.ID, "calendar.read", Deny); err != nil {
		t.Fatal(err)
	}
	rep, err = SeedRules(ctx, s, seed, SeedOptions{})
	if err != nil || rep.Kept != 1 {
		t.Fatalf("keep seed: %+v %v", rep, err)
	}
	if r, _ := s.GetPolicyRule(ctx, u.ID, "calendar.read"); r.Decision != "deny" {
		t.Errorf("seed reverted user change: %s", r.Decision)
	}
	rep, err = SeedRules(ctx, s, seed, SeedOptions{Overwrite: true})
	if err != nil || rep.Updated != 1 {
		t.Fatalf("overwrite seed: %+v %v", rep, err)
	}
	if r, _ := s.GetPolicyRule(ctx, u.ID, "calendar.read"); r.Decision != "allow" {
		t.Errorf("overwrite not applied: %s", r.Decision)
	}
	// Invalid input writes nothing.
	_, err = SeedRules(ctx, s, map[string]config.UserSeed{"dave": {Rules: map[string]string{"notes.read": "allow", "BAD": "allow"}}}, SeedOptions{})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid seed: %v", err)
	}
	if _, err := s.GetPolicyRule(ctx, u.ID, "notes.read"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("partial seed written: %v", err)
	}
}
