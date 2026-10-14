package routines

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type fixture struct {
	db           *store.Store
	st           *Store
	clk          *clock
	alice, bob   int64
	aliceU, bobU *store.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, clk: &clock{t: time.Date(2026, 10, 12, 6, 50, 0, 0, time.UTC)}}
	db.SetClock(f.clk.Now)
	if f.aliceU, err = db.CreateUser(ctx, "alice", "x", "member"); err != nil {
		t.Fatal(err)
	}
	if f.bobU, err = db.CreateUser(ctx, "bob", "x", "member"); err != nil {
		t.Fatal(err)
	}
	f.alice, f.bob = f.aliceU.ID, f.bobU.ID
	f.st = NewStore(db, time.UTC)
	f.st.SetClock(f.clk.Now)
	return f
}

func cronRoutine(name, expr string) Routine {
	return Routine{Name: name, Prompt: "Do the thing.", Trigger: Trigger{Cron: expr}, Enabled: true}
}

func TestNormalize(t *testing.T) {
	ok := Routine{Name: " brief ", Prompt: " hi ", Trigger: Trigger{Cron: "0 7 * * 1-5"}, Deliver: []string{"WEB", "web", "cli"}}
	if err := ok.Normalize(); err != nil {
		t.Fatal(err)
	}
	if ok.Name != "brief" || ok.Prompt != "hi" || strings.Join(ok.Deliver, ",") != "web,cli" {
		t.Errorf("normalized = %+v", ok)
	}
	def := Routine{Name: "x", Prompt: "p", Trigger: Trigger{Event: "email.received"}}
	if err := def.Normalize(); err != nil || strings.Join(def.Deliver, ",") != "web,cli" {
		t.Errorf("default deliver = %v %v", def.Deliver, err)
	}
	bad := map[string]Routine{
		"no name":       {Prompt: "p", Trigger: Trigger{Cron: "@daily"}},
		"long name":     {Name: strings.Repeat("n", MaxNameLen+1), Prompt: "p", Trigger: Trigger{Cron: "@daily"}},
		"control":       {Name: "a\nb", Prompt: "p", Trigger: Trigger{Cron: "@daily"}},
		"no prompt":     {Name: "n", Prompt: "  ", Trigger: Trigger{Cron: "@daily"}},
		"no trigger":    {Name: "n", Prompt: "p"},
		"both triggers": {Name: "n", Prompt: "p", Trigger: Trigger{Cron: "@daily", Event: "x.y"}},
		"bad cron":      {Name: "n", Prompt: "p", Trigger: Trigger{Cron: "61 * * * *"}},
		"bad event":     {Name: "n", Prompt: "p", Trigger: Trigger{Event: "Email Received"}},
		"bad model":     {Name: "n", Prompt: "p", Trigger: Trigger{Cron: "@daily"}, Model: "haiku"},
		"bad deliver":   {Name: "n", Prompt: "p", Trigger: Trigger{Cron: "@daily"}, Deliver: []string{"sms"}},
	}
	for name, r := range bad {
		if err := r.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	r := Routine{Name: "n", Prompt: "p", Trigger: Trigger{Cron: "61 * * * *"}}
	if err := r.Normalize(); err == nil || !strings.Contains(err.Error(), "minute") {
		t.Errorf("cron error not surfaced: %v", err)
	}
}

func TestStoreCRUDAndIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r, err := f.st.Create(ctx, f.alice, cronRoutine("brief", "0 7 * * *"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.UserID != f.alice || !r.NextRunAt.Equal(time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)) || !r.LastRunAt.IsZero() {
		t.Fatalf("created = %+v", r)
	}
	if _, err := f.st.Create(ctx, f.alice, cronRoutine("brief", "@daily")); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name = %v", err)
	}
	if _, err := f.st.Create(ctx, f.bob, cronRoutine("brief", "@daily")); err != nil {
		t.Errorf("same name for another user = %v", err)
	}
	ev, err := f.st.Create(ctx, f.alice, Routine{Name: "mail", Prompt: "triage", Trigger: Trigger{Event: "email.received"}, Enabled: true})
	if err != nil || !ev.NextRunAt.IsZero() {
		t.Fatalf("event routine = %+v %v", ev, err)
	}

	if _, err := f.st.Get(ctx, f.bob, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob reads alice's routine: %v", err)
	}
	if _, err := f.st.Update(ctx, f.bob, r); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob updates alice's routine: %v", err)
	}
	if err := f.st.Delete(ctx, f.bob, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob deletes alice's routine: %v", err)
	}
	if _, err := f.st.Runs(ctx, f.bob, r.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob lists alice's runs: %v", err)
	}
	list, err := f.st.List(ctx, f.alice)
	if err != nil || len(list) != 2 || list[0].Name != "brief" || list[1].Name != "mail" {
		t.Fatalf("list = %+v %v", list, err)
	}

	// Disable: no next run; re-enable later: recomputed from now.
	r.Enabled = false
	if r, err = f.st.Update(ctx, f.alice, r); err != nil || !r.NextRunAt.IsZero() {
		t.Fatalf("disabled = %+v %v", r, err)
	}
	f.clk.Set(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	r.Enabled = true
	r.Trigger = Trigger{Cron: "30 8 * * *"}
	if r, err = f.st.Update(ctx, f.alice, r); err != nil || !r.NextRunAt.Equal(time.Date(2026, 10, 15, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("re-enabled = %+v %v", r, err)
	}
	r.Name = "mail"
	if _, err := f.st.Update(ctx, f.alice, r); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto existing = %v", err)
	}
	if err := f.st.Delete(ctx, f.alice, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Get(ctx, f.alice, ev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted routine still there: %v", err)
	}
}

func TestDueAndClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r, _ := f.st.Create(ctx, f.alice, cronRoutine("brief", "0 7 * * *"))
	off, _ := f.st.Create(ctx, f.alice, cronRoutine("off", "0 7 * * *"))
	off.Enabled = false
	f.st.Update(ctx, f.alice, off)
	f.st.Create(ctx, f.alice, Routine{Name: "ev", Prompt: "p", Trigger: Trigger{Event: "x.y"}, Enabled: true})

	if due, _ := f.st.Due(ctx, f.clk.Now()); len(due) != 0 {
		t.Fatalf("due before 07:00 = %+v", due)
	}
	// The daemon was down for three days: the routine is due once.
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	f.clk.Set(now)
	due, err := f.st.Due(ctx, now)
	if err != nil || len(due) != 1 || due[0].ID != r.ID {
		t.Fatalf("due = %+v %v", due, err)
	}
	run, err := f.st.MarkStarted(ctx, due[0], SourceSchedule, "")
	if err != nil || run.Status != StatusRunning || !run.ScheduledFor.Equal(r.NextRunAt) {
		t.Fatalf("MarkStarted = %+v %v", run, err)
	}
	// A second claim from the same (stale) read loses.
	if _, err := f.st.MarkStarted(ctx, due[0], SourceSchedule, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("double claim = %v, want ErrConflict", err)
	}
	got, _ := f.st.Get(ctx, f.alice, r.ID)
	if !got.NextRunAt.Equal(time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)) || !got.LastRunAt.Equal(now) {
		t.Errorf("after claim next=%s last=%s (missed activations must collapse into one)", got.NextRunAt, got.LastRunAt)
	}
	if due, _ := f.st.Due(ctx, now); len(due) != 0 {
		t.Errorf("still due after claim: %+v", due)
	}

	run.Status, run.Reply = StatusDone, "All clear."
	fin, err := f.st.MarkFinished(ctx, run)
	if err != nil || fin.Status != StatusDone || fin.Reply != "All clear." || fin.FinishedAt.IsZero() {
		t.Fatalf("MarkFinished = %+v %v", fin, err)
	}
	if _, err := f.st.MarkFinished(ctx, run); err == nil {
		t.Error("finished twice")
	}
	runs, err := f.st.Runs(ctx, f.alice, r.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || runs[0].Source != SourceSchedule {
		t.Errorf("runs = %+v %v", runs, err)
	}
	if _, err := f.st.GetRun(ctx, f.bob, run.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob reads alice's run: %v", err)
	}

	// Disabled users' routines are not due.
	f.clk.Set(time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC))
	if err := f.db.SetUserDisabled(ctx, f.alice, true); err != nil {
		t.Fatal(err)
	}
	if due, _ := f.st.Due(ctx, f.clk.Now()); len(due) != 0 {
		t.Errorf("disabled user's routine due: %+v", due)
	}
}

func TestAbandonInterrupted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r, _ := f.st.Create(ctx, f.alice, cronRoutine("brief", "@hourly"))
	run, _ := f.st.MarkStarted(ctx, r, SourceManual, "")
	if err := f.st.SetRunStatus(ctx, run.ID, StatusWaitingApproval); err != nil {
		t.Fatal(err)
	}
	n, err := f.st.AbandonInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("abandoned %d, %v", n, err)
	}
	got, _ := f.st.GetRun(ctx, f.alice, run.ID)
	if got.Status != StatusCanceled || !strings.Contains(got.Error, "interrupted") || got.FinishedAt.IsZero() {
		t.Errorf("abandoned run = %+v", got)
	}
	// Status changes after finishing are ignored.
	f.st.SetRunStatus(ctx, run.ID, StatusRunning)
	if got, _ := f.st.GetRun(ctx, f.alice, run.ID); got.Status != StatusCanceled {
		t.Errorf("finished run status changed to %s", got.Status)
	}
}

func TestSeed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cfgs := []config.RoutineConfig{
		{Name: "brief", Owner: "alice", Trigger: config.RoutineTrigger{Cron: "0 7 * * 1-5"}, Prompt: "Summarize.", Model: "fake/small", Deliver: []string{"web"}},
		{Name: "mail", Owner: "alice", Trigger: config.RoutineTrigger{Event: "email.received"}, Prompt: "Triage."},
		{Name: "brief", Owner: "carol", Trigger: config.RoutineTrigger{Cron: "@daily"}, Prompt: "x"},
		{Name: "other", Owner: "carol", Trigger: config.RoutineTrigger{Cron: "@daily"}, Prompt: "x"},
	}
	rep, err := Seed(ctx, f.st, f.db, cfgs, SeedOptions{})
	if err != nil || rep.Created != 2 || len(rep.MissingUsers) != 1 || rep.MissingUsers[0] != "carol" {
		t.Fatalf("seed = %+v %v", rep, err)
	}
	rep, _ = Seed(ctx, f.st, f.db, cfgs, SeedOptions{})
	if rep.Created != 0 || rep.Unchanged != 2 {
		t.Errorf("reseed = %+v", rep)
	}
	got, err := f.st.GetByName(ctx, f.alice, "brief")
	if err != nil || got.Model != "fake/small" || got.Trigger.Cron != "0 7 * * 1-5" || !got.Enabled || got.NextRunAt.IsZero() {
		t.Fatalf("seeded = %+v %v", got, err)
	}
	// User edits survive restarts; seeding never re-enables.
	got.Prompt, got.Enabled = "Summarize briefly.", false
	if _, err := f.st.Update(ctx, f.alice, got); err != nil {
		t.Fatal(err)
	}
	rep, _ = Seed(ctx, f.st, f.db, cfgs, SeedOptions{})
	if rep.Kept != 1 || rep.Unchanged != 1 {
		t.Errorf("seed after edit = %+v", rep)
	}
	if g, _ := f.st.GetByName(ctx, f.alice, "brief"); g.Prompt != "Summarize briefly." {
		t.Errorf("seed overwrote user edit: %+v", g)
	}
	rep, _ = Seed(ctx, f.st, f.db, cfgs, SeedOptions{Overwrite: true})
	if g, _ := f.st.GetByName(ctx, f.alice, "brief"); rep.Updated != 1 || g.Prompt != "Summarize." || g.Enabled {
		t.Errorf("overwrite = %+v, routine %+v", rep, g)
	}
	// Invalid config aborts before writing anything.
	bad := append([]config.RoutineConfig{{Name: "new", Owner: "bob", Trigger: config.RoutineTrigger{Cron: "@daily"}, Prompt: "x"}},
		config.RoutineConfig{Name: "broken", Owner: "bob", Trigger: config.RoutineTrigger{Cron: "0 25 * * *"}, Prompt: "x"})
	if _, err := Seed(ctx, f.st, f.db, bad, SeedOptions{}); err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "hour") {
		t.Errorf("bad cron seed err = %v", err)
	}
	if list, _ := f.st.List(ctx, f.bob); len(list) != 0 {
		t.Errorf("partial seed wrote %+v", list)
	}
}
