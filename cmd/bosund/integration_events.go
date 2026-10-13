package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/vault"
)

// wireIntegrationEvents hands background-capable integrations their
// optional hooks: an event emitter (integration.EventEmitter) bound to the
// on_event hooks, and a user lister (integration.UserLister). It runs after
// routines subscribe to on_event, so no early event is lost.
func (d *daemon) wireIntegrationEvents(vlt *vault.Vault) {
	for _, in := range d.integrations {
		name := in.Manifest().Name
		if ul, ok := in.(integration.UserLister); ok {
			ul.SetUserLister(d.credentialedUsers(vlt, name))
		}
		if em, ok := in.(integration.EventEmitter); ok {
			em.SetEmitter(d.integrationEmitter(name))
		}
	}
}

// integrationEmitter returns an emitter that only accepts events in the
// integration's own namespace ("<name>.") for a single, real user (user 0
// would fan out to everyone).
func (d *daemon) integrationEmitter(name string) func(context.Context, int64, string, json.RawMessage) error {
	hk := d.hooks
	return func(ctx context.Context, userID int64, ev string, payload json.RawMessage) error {
		if !strings.HasPrefix(ev, name+".") {
			return fmt.Errorf("integration %s may only emit %s.* events, not %q", name, name, ev)
		}
		if userID <= 0 {
			return fmt.Errorf("integration %s: event %s needs a user", name, ev)
		}
		return hk.EmitEvent(ctx, hooks.Event{Name: ev, UserID: userID, Payload: payload})
	}
}

// credentialedUsers lists enabled users holding a vault credential for the
// integration, or any OAuth credential (linked accounts such as Google may
// be stored under the provider rather than the integration). The
// integration still resolves credentials through its own CredentialSource
// and skips users it has nothing for.
func (d *daemon) credentialedUsers(vlt *vault.Vault, name string) func(context.Context) ([]int64, error) {
	return func(ctx context.Context) ([]int64, error) {
		users, err := d.store.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		var out []int64
		for _, u := range users {
			if u.Disabled {
				continue
			}
			id := u.ID
			infos, err := vlt.List(ctx, &id, "")
			if err != nil {
				return nil, err
			}
			for _, inf := range infos {
				if inf.Integration == name || strings.HasPrefix(inf.Kind, "oauth2:") {
					out = append(out, id)
					break
				}
			}
		}
		return out, nil
	}
}
