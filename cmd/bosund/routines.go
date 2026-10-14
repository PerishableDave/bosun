package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/routines"
)

// routineShutdownGrace is how long Close waits for in-flight routine runs
// before canceling them.
const routineShutdownGrace = 5 * time.Second

// setupRoutines builds the routine store and scheduler, marks runs left
// over from a previous process as canceled, seeds routines from config, and
// subscribes the scheduler to on_event hooks. The scheduler loop is started
// by newDaemon once everything else is wired.
func (d *daemon) setupRoutines(ctx context.Context, dp deps) error {
	loc, err := d.cfg.Daemon.Location()
	if err != nil {
		return err
	}
	d.routines = routines.NewStore(d.store, loc)
	n, err := d.routines.AbandonInterrupted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		d.log.Warn("routine runs interrupted by the previous shutdown were marked canceled", "count", n)
	}
	rep, err := routines.Seed(ctx, d.routines, d.store, d.cfg.Routines, routines.SeedOptions{Log: d.log})
	if err != nil {
		return err
	}
	d.log.Info("routines seeded from config", "created", rep.Created, "updated", rep.Updated, "unchanged", rep.Unchanged,
		"kept_db_value", rep.Kept, "missing_users", strings.Join(rep.MissingUsers, ","), "timezone", loc.String())

	runner := routines.NewAgentRunner(d.agent, d.store, d.routines, d.bus, d.log)
	d.sched = routines.New(d.routines, runner, routines.Options{Tick: dp.RoutineTick, Bus: d.bus, Log: d.log})
	if err := d.hooks.OnEvent("routines", d.sched.HandleEvent); err != nil {
		return fmt.Errorf("routines: %w", err)
	}
	return nil
}
