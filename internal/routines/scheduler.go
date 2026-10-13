package routines

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/hooks"
)

// Invocation describes why a run was started.
type Invocation struct {
	// Run is the started run (ID, Source, StartedAt, ...), already recorded
	// with status running.
	Run Run
	// Event is the triggering event for Source == SourceEvent, else nil.
	Event *hooks.Event
}

// Runner executes one routine run as its owner. It returns the run with
// ConversationID, Status, Error and Reply filled in; the Scheduler records
// the outcome (MarkFinished) and announces it. A non-nil error with an
// empty Status is recorded as StatusError.
type Runner interface {
	RunRoutine(ctx context.Context, r Routine, inv Invocation) (Run, error)
}

// Scheduler defaults.
const (
	DefaultTick        = 30 * time.Second
	DefaultConcurrency = 2
)

// Options configure a Scheduler.
type Options struct {
	// Tick is how often due cron routines are checked (default 30s).
	Tick time.Duration
	// Concurrency bounds simultaneous runs across all users (default 2).
	// Runs beyond it wait for a slot.
	Concurrency int
	// Bus receives routine_finished events for the owner (optional).
	Bus events.Publisher
	// Log is the operational logger.
	Log *slog.Logger
}

// Scheduler runs due cron routines, manual runs, and event-triggered
// routines, each as its owning user.
type Scheduler struct {
	st     *Store
	runner Runner
	tick   time.Duration
	bus    events.Publisher
	log    *slog.Logger
	sem    chan struct{}

	runCtx    context.Context // in-flight runs; canceled by Close
	cancelRun context.CancelFunc

	mu     sync.Mutex
	active map[int64]int // routine ID -> runs in flight (or waiting for a slot)
	closed bool
	stop   chan struct{}  // closed by Close; ends the Run loop
	wg     sync.WaitGroup // launched runs
	loopWG sync.WaitGroup // the Run loop
}

// New returns a Scheduler. Call Run to start the cron loop and Close to
// stop.
func New(st *Store, runner Runner, opts Options) *Scheduler {
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{st: st, runner: runner, tick: opts.Tick, bus: opts.Bus, log: opts.Log,
		sem: make(chan struct{}, opts.Concurrency), runCtx: ctx, cancelRun: cancel, active: map[int64]int{},
		stop: make(chan struct{})}
}

// Run checks for due cron routines immediately (which picks up anything
// that became due while the daemon was down) and then every tick, until
// ctx ends. Runs already started keep going after Run returns; Close stops
// them.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.loopWG.Add(1)
	s.mu.Unlock()
	defer s.loopWG.Done()
	if _, err := s.RunDue(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
		s.log.Error("routines: check due routines", "err", err)
	}
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			if _, err := s.RunDue(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrClosed) {
				s.log.Error("routines: check due routines", "err", err)
			}
		}
	}
}

// RunDue starts every enabled cron routine whose next run time has passed
// and returns how many it started. A routine whose previous run is still in
// flight is left for a later tick. A routine that is overdue by more than a
// tick (the daemon was down or busy) runs once, not once per missed
// activation, and the log says so.
func (s *Scheduler) RunDue(ctx context.Context) (int, error) {
	if s.isClosed() {
		return 0, ErrClosed
	}
	now := s.st.now()
	due, err := s.st.Due(ctx, now)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, r := range due {
		if !s.activate(r.ID, true) {
			continue
		}
		if late := now.Sub(r.NextRunAt); late > s.tick {
			missed := s.countMissed(r, now)
			s.log.Info("routines: routine is overdue; running it once now (missed activations are not replayed)",
				"routine_id", r.ID, "routine", r.Name, "user_id", r.UserID,
				"was_due", r.NextRunAt.In(s.st.loc).Format(time.RFC3339), "missed_activations", missed)
		}
		run, err := s.st.MarkStarted(ctx, r, SourceSchedule, "")
		if err != nil {
			s.deactivate(r.ID)
			if errors.Is(err, ErrConflict) {
				continue // claimed elsewhere or changed since Due read it
			}
			return started, err
		}
		if s.launch(r, run, nil) {
			started++
		}
	}
	return started, nil
}

// countMissed counts activations from r.NextRunAt up to now (capped).
func (s *Scheduler) countMissed(r Routine, now time.Time) int {
	sch, err := ParseCron(r.Trigger.Cron)
	if err != nil {
		return 1
	}
	n := 1
	for t := sch.Next(r.NextRunAt, s.st.loc); !t.IsZero() && !t.After(now) && n < 10000; t = sch.Next(t, s.st.loc) {
		n++
	}
	return n
}

// RunNow starts userID's routine id immediately, whether or not it is
// enabled, and returns the started run (status running). The run itself
// proceeds in the background. ErrBusy if the routine is already running.
func (s *Scheduler) RunNow(ctx context.Context, userID, id int64) (Run, error) {
	r, err := s.st.Get(ctx, userID, id)
	if err != nil {
		return Run{}, err
	}
	if s.isClosed() {
		return Run{}, ErrClosed
	}
	if !s.activate(r.ID, true) {
		return Run{}, ErrBusy
	}
	run, err := s.st.MarkStarted(ctx, r, SourceManual, "")
	if err != nil {
		s.deactivate(r.ID)
		return Run{}, err
	}
	if !s.launch(r, run, nil) {
		return Run{}, ErrClosed
	}
	return run, nil
}

// HandleEvent starts every enabled routine subscribed to ev.Name for
// ev.UserID (every user when 0). It is the scheduler's on_event hook:
// register it with hooks.Registry.OnEvent. It returns once the runs are
// recorded; they execute in the background.
func (s *Scheduler) HandleEvent(ctx context.Context, ev hooks.Event) {
	rs, err := s.st.ForEvent(ctx, ev.Name, ev.UserID)
	if err != nil {
		s.log.Error("routines: find routines for event", "event", ev.Name, "err", err)
		return
	}
	for _, r := range rs {
		if s.isClosed() {
			s.log.Warn("routines: scheduler shut down; event not delivered", "event", ev.Name, "routine_id", r.ID)
			return
		}
		s.activate(r.ID, false)
		run, err := s.st.MarkStarted(ctx, r, SourceEvent, ev.Name)
		if err != nil {
			s.deactivate(r.ID)
			s.log.Error("routines: start event run", "event", ev.Name, "routine_id", r.ID, "err", err)
			continue
		}
		evCopy := ev
		s.launch(r, run, &evCopy)
	}
}

func (s *Scheduler) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// activate marks a run of id in flight. With exclusive it fails if one
// already is.
func (s *Scheduler) activate(id int64, exclusive bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if exclusive && s.active[id] > 0 {
		return false
	}
	s.active[id]++
	return true
}

func (s *Scheduler) deactivate(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[id]--; s.active[id] <= 0 {
		delete(s.active, id)
	}
}

// launch runs r in the background (after waiting for a concurrency slot)
// and records the outcome. It takes over the activation of r.ID. It returns
// false, finishing the run as canceled, if the scheduler is closed.
func (s *Scheduler) launch(r Routine, run Run, ev *hooks.Event) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.deactivate(r.ID)
		run.Status, run.Error = StatusCanceled, "scheduler shut down before the run started"
		s.finish(r, run)
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer s.deactivate(r.ID)
		select {
		case s.sem <- struct{}{}:
		case <-s.runCtx.Done():
			run.Status, run.Error = StatusCanceled, "scheduler shut down before the run started"
			s.finish(r, run)
			return
		}
		defer func() { <-s.sem }()
		s.log.Info("routines: run started", "routine_id", r.ID, "routine", r.Name, "user_id", r.UserID,
			"run_id", run.ID, "source", run.Source)
		res, err := s.safeRun(r, Invocation{Run: run, Event: ev})
		res.ID, res.RoutineID, res.UserID, res.Source, res.Event = run.ID, run.RoutineID, run.UserID, run.Source, run.Event
		switch {
		case s.runCtx.Err() != nil && (err != nil || res.Status == StatusCanceled || res.Status == ""):
			res.Status = StatusCanceled
			if res.Error == "" {
				res.Error = "canceled: the daemon is shutting down"
			}
		case err != nil && (res.Status == "" || res.Status == StatusRunning || res.Status == StatusDone):
			res.Status = StatusError
			if res.Error == "" {
				res.Error = err.Error()
			}
		case res.Status == "" || res.Status == StatusRunning || res.Status == StatusWaitingApproval:
			res.Status = StatusDone
		}
		s.finish(r, res)
	}()
	return true
}

// safeRun calls the runner, converting a panic into an error.
func (s *Scheduler) safeRun(r Routine, inv Invocation) (res Run, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("routines: runner panicked", "routine_id", r.ID, "panic", v)
			res, err = Run{}, errors.New("internal error")
		}
	}()
	return s.runner.RunRoutine(s.runCtx, r, inv)
}

// finish records the outcome and notifies the owner.
func (s *Scheduler) finish(r Routine, run Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := s.st.MarkFinished(ctx, run)
	if err != nil {
		s.log.Error("routines: record run outcome", "routine_id", r.ID, "run_id", run.ID, "err", err)
		stored = run
	}
	log := s.log.Info
	if stored.Status != StatusDone {
		log = s.log.Warn
	}
	log("routines: run finished", "routine_id", r.ID, "routine", r.Name, "user_id", r.UserID,
		"run_id", stored.ID, "status", stored.Status, "error", stored.Error, "conversation_id", stored.ConversationID)
	if s.bus != nil {
		s.bus.Publish(r.UserID, events.Event{Type: events.TypeRoutineFinished, Data: events.RoutineFinishedData{
			RoutineID: r.ID, RunID: stored.ID, ConversationID: stored.ConversationID, Status: stored.Status}})
	}
}

// Close stops new runs from starting, waits up to grace for in-flight runs
// to finish, then cancels the rest (they are recorded as canceled; pending
// approvals they were waiting on are canceled) and waits for them to wind
// down. It is idempotent.
func (s *Scheduler) Close(grace time.Duration) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	s.loopWG.Wait() // the loop finishes its current check; new runs are refused
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		s.log.Warn("routines: canceling in-flight routine runs at shutdown")
		s.cancelRun()
		<-done
	}
	s.cancelRun()
}
