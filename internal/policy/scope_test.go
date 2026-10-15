package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

func TestDecideScopedPrecedence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	other := mkUser(t, s, "erin")
	e := NewEngine(s, Options{Defaults: stdDefaults})
	const c = integration.Capability("browser.act")
	w := integration.RiskWrite

	// No rules: risk default, scoped or not.
	if got := e.DecideScoped(ctx, u.ID, c, "opentable.com", w, nil); got != Ask {
		t.Errorf("default = %s", got)
	}
	// Capability-wide rule applies to every scope.
	if _, err := e.SetRule(ctx, u.ID, c, Deny); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "opentable.com", w, nil); got != Deny {
		t.Errorf("capability rule = %s", got)
	}
	// A scoped rule beats the capability-wide rule, for that scope (and its
	// subdomains) only.
	if _, err := e.SetScopedRule(ctx, u.ID, c, "opentable.com", Allow); err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[string]Decision{
		"opentable.com":    Allow,
		"m.opentable.com":  Allow,
		"notopentable.com": Deny,
		"opentable.co":     Deny,
		"":                 Deny,
	} {
		if got := e.DecideScoped(ctx, u.ID, c, scope, w, nil); got != want {
			t.Errorf("DecideScoped(%q) = %s, want %s", scope, got, want)
		}
	}
	// Decide (unscoped) ignores scoped rules.
	if got := e.Decide(ctx, u.ID, c, w, nil); got != Deny {
		t.Errorf("Decide = %s", got)
	}
	// The most specific scope wins.
	if _, err := e.SetScopedRule(ctx, u.ID, c, "m.opentable.com", Deny); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "m.opentable.com", w, nil); got != Deny {
		t.Errorf("subdomain rule = %s", got)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "www2.opentable.com", w, nil); got != Allow {
		t.Errorf("parent rule = %s", got)
	}
	// Rules are per user.
	if got := e.DecideScoped(ctx, other.ID, c, "opentable.com", w, nil); got != Ask {
		t.Errorf("other user = %s", got)
	}
	// Deleting the capability-wide rule keeps scoped rules; the default
	// applies elsewhere.
	if err := e.DeleteRule(ctx, u.ID, c); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "opentable.com", w, nil); got != Allow {
		t.Errorf("after delete = %s", got)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "example.com", w, nil); got != Ask {
		t.Errorf("after delete, other scope = %s", got)
	}
	rules, err := e.ListRules(ctx, u.ID)
	if err != nil || len(rules) != 2 || rules[0].Scope != "m.opentable.com" || rules[1].Scope != "opentable.com" {
		t.Fatalf("ListRules = %+v, %v", rules, err)
	}
	if err := e.DeleteScopedRule(ctx, u.ID, c, "opentable.com"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteScopedRule(ctx, u.ID, c, "opentable.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "opentable.com", w, nil); got != Ask {
		t.Errorf("after scoped delete = %s", got)
	}
}

func TestSetScopedRuleValidates(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(s, Options{Defaults: stdDefaults})
	for _, sc := range []string{"WWW.OpenTable.com", "https://opentable.com", "not a site"} {
		if _, err := e.SetScopedRule(ctx, u.ID, "browser.act", sc, Allow); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetScopedRule(%q) = %v, want ErrInvalid", sc, err)
		}
	}
	r, err := e.SetScopedRule(ctx, u.ID, "browser.act", "", Ask)
	if err != nil || r.Scope != "" || r.Decision != Ask {
		t.Fatalf("empty scope = %+v, %v", r, err)
	}
}

// Scoped lookups fail closed like unscoped ones.
func TestDecideScopedStoreErrorDenies(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	e := NewEngine(&scopedFaulty{Store: s}, Options{Defaults: Defaults{Read: Allow, Write: Allow, Destructive: Allow}})
	if got := e.DecideScoped(ctx, u.ID, "browser.act", "example.com", integration.RiskRead, nil); got != Deny {
		t.Errorf("store error = %s", got)
	}
	if got := e.Decide(ctx, u.ID, "browser.act", integration.RiskRead, nil); got != Allow {
		t.Errorf("unscoped = %s (scoped lookup must not run)", got)
	}
}

type scopedFaulty struct{ Store }

func (f *scopedFaulty) GetScopedPolicyRule(context.Context, int64, string, string) (*store.PolicyRule, error) {
	return nil, errors.New("boom")
}
