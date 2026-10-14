package routines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
)

// UserLookup resolves usernames (usually *store.Store).
type UserLookup interface {
	GetUserByUsername(ctx context.Context, username string) (*store.User, error)
}

// SeedOptions control Seed.
type SeedOptions struct {
	// Overwrite replaces an existing routine's prompt, trigger, model and
	// deliver targets with the config values. When false (the default) a
	// routine that already exists is left alone, so edits made through the
	// UI/CLI survive restarts. Either way seeding is idempotent and never
	// changes whether a routine is enabled.
	Overwrite bool
	Log       *slog.Logger
}

// SeedReport summarises a Seed run.
type SeedReport struct {
	Created      int
	Updated      int
	Unchanged    int
	Kept         int      // existing routine differed from config and Overwrite was false
	MissingUsers []string // owners in config with no DB user (skipped)
}

// Seed loads the routines: section of bosun.yaml, keyed by (owner, name).
// Owners that do not exist yet are skipped and reported (seeding runs at
// every start). An invalid routine (bad cron, bad event name, ...) aborts
// before anything is written.
func Seed(ctx context.Context, st *Store, users UserLookup, cfgs []config.RoutineConfig, opts SeedOptions) (SeedReport, error) {
	var rep SeedReport
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	want := make([]Routine, len(cfgs))
	for i, c := range cfgs {
		r := Routine{Name: c.Name, Prompt: c.Prompt, Trigger: Trigger{Cron: c.Trigger.Cron, Event: c.Trigger.Event},
			Model: c.Model, Deliver: append([]string(nil), c.Deliver...), Enabled: true}
		if err := r.Normalize(); err != nil {
			return rep, fmt.Errorf("seed routines: routines.%s (owner %s): %w", c.Name, c.Owner, err)
		}
		want[i] = r
	}
	missing := map[string]bool{}
	for i, c := range cfgs {
		u, err := users.GetUserByUsername(ctx, c.Owner)
		if errors.Is(err, store.ErrNotFound) {
			if !missing[c.Owner] {
				log.WarnContext(ctx, "routine seed: owner not found; skipping", "owner", c.Owner)
				missing[c.Owner] = true
				rep.MissingUsers = append(rep.MissingUsers, c.Owner)
			}
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("seed routines for %s: %w", c.Owner, err)
		}
		cur, err := st.GetByName(ctx, u.ID, want[i].Name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, err := st.Create(ctx, u.ID, want[i]); err != nil {
				return rep, fmt.Errorf("seed routine %s/%s: %w", c.Owner, want[i].Name, err)
			}
			rep.Created++
		case err != nil:
			return rep, fmt.Errorf("seed routine %s/%s: %w", c.Owner, want[i].Name, err)
		case sameDefinition(cur, want[i]):
			rep.Unchanged++
		case !opts.Overwrite:
			log.InfoContext(ctx, "routine seed: keeping existing routine that differs from config",
				"owner", c.Owner, "routine", cur.Name)
			rep.Kept++
		default:
			upd := want[i]
			upd.ID, upd.Enabled = cur.ID, cur.Enabled
			if _, err := st.Update(ctx, u.ID, upd); err != nil {
				return rep, fmt.Errorf("seed routine %s/%s: %w", c.Owner, want[i].Name, err)
			}
			rep.Updated++
		}
	}
	return rep, nil
}

func sameDefinition(a, b Routine) bool {
	return a.Prompt == b.Prompt && a.Trigger == b.Trigger && a.Model == b.Model && slices.Equal(a.Deliver, b.Deliver)
}
