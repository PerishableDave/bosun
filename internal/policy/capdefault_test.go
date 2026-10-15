package policy

import (
	"context"
	"testing"

	"github.com/perishabledave/bosun/internal/integration"
)

// TestIntegrationDefaultPrecedence: scoped rule > capability-wide rule >
// integration default > risk default.
func TestIntegrationDefaultPrecedence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mkUser(t, s, "dave")
	const (
		c     = integration.Capability("browser.read")
		plain = integration.Capability("email.read")
		bad   = integration.Capability("odd.read")
	)
	r := integration.RiskRead
	defaults := Defaults{Read: Allow, Write: Ask, Destructive: Ask}
	e := NewEngine(s, Options{Defaults: defaults, CapabilityDefault: func(x integration.Capability) (Decision, bool) {
		switch x {
		case c:
			return Ask, true
		case bad:
			return "maybe", true
		}
		return "", false
	}})

	// Integration default beats the risk default; capabilities without one
	// use the risk default; an invalid integration default denies.
	if got := e.Decide(ctx, u.ID, c, r, nil); got != Ask {
		t.Errorf("integration default = %s, want ask", got)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "example.com", r, nil); got != Ask {
		t.Errorf("integration default (scoped call) = %s, want ask", got)
	}
	if got := e.Decide(ctx, u.ID, plain, r, nil); got != Allow {
		t.Errorf("risk default = %s, want allow", got)
	}
	if got := e.Decide(ctx, u.ID, bad, r, nil); got != Deny {
		t.Errorf("invalid integration default = %s, want deny", got)
	}

	// A scoped rule beats the integration default on its site only.
	if _, err := e.SetScopedRule(ctx, u.ID, c, "example.com", Allow); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "www2.example.com", r, nil); got != Allow {
		t.Errorf("scoped rule = %s, want allow", got)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "other.org", r, nil); got != Ask {
		t.Errorf("other site = %s, want ask", got)
	}

	// A capability-wide rule beats the integration default.
	if _, err := e.SetRule(ctx, u.ID, c, Allow); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "other.org", r, nil); got != Allow {
		t.Errorf("capability rule = %s, want allow", got)
	}
	if _, err := e.SetRule(ctx, u.ID, c, Deny); err != nil {
		t.Fatal(err)
	}
	if got := e.DecideScoped(ctx, u.ID, c, "example.com", r, nil); got != Allow {
		t.Errorf("scoped rule over capability rule = %s, want allow", got)
	}
	if got := e.Decide(ctx, u.ID, c, r, nil); got != Deny {
		t.Errorf("capability rule deny = %s", got)
	}
}
