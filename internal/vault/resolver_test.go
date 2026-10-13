package vault

import (
	"context"
	"testing"

	"github.com/dperedo/bosun/internal/integration"
)

type recResolver struct {
	owner *int64
	kind  string
}

func (r *recResolver) Resolve(_ context.Context, owner *int64, kind string) (integration.Credential, error) {
	r.owner, r.kind = owner, kind
	return integration.Credential{Kind: kind, Secret: "resolved"}, nil
}

func TestKindResolver(t *testing.T) {
	f := setup(t)
	ctx := integration.WithUserID(context.Background(), f.a)
	// The integration's own row for the kind is bypassed once a resolver is
	// registered for its prefix.
	if err := f.v.Put(context.Background(), &f.a, "email", "oauth2:google", integration.Credential{Secret: "own-row"}); err != nil {
		t.Fatal(err)
	}
	r := &recResolver{}
	f.v.RegisterResolver("oauth2:", r)
	src := f.v.SourceFor("email", integration.ScopePerUser)
	c, err := src.Get(ctx, "oauth2:google")
	if err != nil || c.Secret != "resolved" || r.owner == nil || *r.owner != f.a || r.kind != "oauth2:google" {
		t.Fatalf("resolved %+v %v (owner %v kind %q)", c, err, r.owner, r.kind)
	}
	// Other kinds still come from the integration's rows.
	if err := f.v.Put(context.Background(), &f.a, "email", "apikey", integration.Credential{Secret: "k"}); err != nil {
		t.Fatal(err)
	}
	if c, err := src.Get(ctx, "apikey"); err != nil || c.Secret != "k" {
		t.Errorf("apikey = %+v %v", c, err)
	}
	// The per-user rule still applies before the resolver: no user, no call.
	r.owner, r.kind = nil, ""
	if _, err := src.Get(context.Background(), "oauth2:google"); err == nil || r.kind != "" {
		t.Errorf("no-user lookup reached the resolver: %v", err)
	}
	// Shared sources pass a nil owner.
	if _, err := f.v.SourceFor("mcp", integration.ScopeShared).Get(ctx, "oauth2:google"); err != nil || r.owner != nil || r.kind != "oauth2:google" {
		t.Errorf("shared: owner %v kind %q err %v", r.owner, r.kind, err)
	}
}
