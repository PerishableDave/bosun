// Package routines runs scheduled and event-driven agent turns ("routines")
// on behalf of their owning user.
//
// A routine is a named prompt plus a trigger (a cron expression or an event
// name). The Scheduler claims due cron routines from the Store, runs them
// through a Runner (normally AgentRunner, which goes through the same agent,
// dispatcher and policy as interactive turns) with a concurrency limit, and
// subscribes to hooks events for event-triggered routines.
package routines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/store"
)

// Trigger says when a routine runs. Exactly one field is set.
type Trigger struct {
	Cron  string `json:"cron,omitempty"`
	Event string `json:"event,omitempty"`
}

// Routine is a named prompt run as its owner on a trigger.
type Routine struct {
	ID      int64   `json:"id"`
	UserID  int64   `json:"user_id"`
	Name    string  `json:"name"`
	Prompt  string  `json:"prompt"`
	Trigger Trigger `json:"trigger"`
	// Model is an optional "<provider>/<model>" override of llm.default.
	Model string `json:"model,omitempty"`
	// Deliver lists where the result is posted ("web", "cli"). In v1 both
	// mean the reply is stored in the run's conversation and announced with
	// a routine_finished event; the field exists for future chat-app
	// deliveries.
	Deliver   []string  `json:"deliver"`
	Enabled   bool      `json:"enabled"`
	NextRunAt time.Time `json:"next_run_at,omitzero"`
	LastRunAt time.Time `json:"last_run_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Run statuses. Running and WaitingApproval are transient.
const (
	StatusRunning         = "running"
	StatusWaitingApproval = "waiting_approval" // blocked on an approval the owner must answer
	StatusDone            = "done"
	StatusError           = "error"
	StatusCanceled        = "canceled"
)

// Run sources: what started a run.
const (
	SourceSchedule = "schedule"
	SourceEvent    = "event"
	SourceManual   = "manual"
)

// Run is one execution of a routine.
type Run struct {
	ID             int64     `json:"id"`
	RoutineID      int64     `json:"routine_id"`
	UserID         int64     `json:"user_id"`
	Source         string    `json:"source"`
	Event          string    `json:"event,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ScheduledFor   time.Time `json:"scheduled_for,omitzero"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	// Reply is the final assistant text of the run.
	Reply string `json:"reply,omitempty"`
}

// Errors.
var (
	ErrNotFound = errors.New("routines: not found")
	ErrInvalid  = errors.New("routines: invalid")
	ErrConflict = errors.New("routines: a routine with that name already exists")
	ErrBusy     = errors.New("routines: routine is already running")
	ErrClosed   = errors.New("routines: scheduler is shut down")
)

// Limits.
const (
	MaxNameLen   = 100
	MaxPromptLen = 16 << 10
	MaxModelLen  = 200
)

// Deliver targets.
var validDeliver = map[string]bool{"web": true, "cli": true}

// DefaultDeliver is used when a routine is created without deliver targets.
var DefaultDeliver = []string{"web", "cli"}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Normalize trims and validates r's user-editable fields in place.
func (r *Routine) Normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	switch {
	case r.Name == "":
		return invalid("name is required")
	case len(r.Name) > MaxNameLen:
		return invalid("name is longer than %d bytes", MaxNameLen)
	case !utf8.ValidString(r.Name) || strings.IndexFunc(r.Name, unicode.IsControl) >= 0:
		return invalid("name contains control characters")
	}
	r.Prompt = strings.TrimSpace(r.Prompt)
	if r.Prompt == "" {
		return invalid("prompt is required")
	}
	if len(r.Prompt) > MaxPromptLen {
		return invalid("prompt is longer than %d bytes", MaxPromptLen)
	}
	r.Trigger.Cron, r.Trigger.Event = strings.TrimSpace(r.Trigger.Cron), strings.TrimSpace(r.Trigger.Event)
	if (r.Trigger.Cron == "") == (r.Trigger.Event == "") {
		return invalid("trigger: exactly one of cron or event is required")
	}
	if r.Trigger.Cron != "" {
		if _, err := ParseCron(r.Trigger.Cron); err != nil {
			return invalid("trigger: %v", err)
		}
	} else if !hooks.ValidEventName(r.Trigger.Event) {
		return invalid("trigger: invalid event name %q (want dotted lower-case words, e.g. email.received)", r.Trigger.Event)
	}
	r.Model = strings.TrimSpace(r.Model)
	if r.Model != "" {
		p, m, ok := strings.Cut(r.Model, "/")
		if !ok || p == "" || m == "" || len(r.Model) > MaxModelLen {
			return invalid("model %q: must be <provider>/<model>", r.Model)
		}
	}
	if len(r.Deliver) == 0 {
		r.Deliver = append([]string(nil), DefaultDeliver...)
	}
	seen := map[string]bool{}
	out := r.Deliver[:0:0]
	for _, d := range r.Deliver {
		d = strings.ToLower(strings.TrimSpace(d))
		if !validDeliver[d] {
			return invalid("deliver %q: must be web or cli", d)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	r.Deliver = out
	return nil
}

// Store persists routines and their runs. All per-user methods are scoped
// by user ID: another user's routine is ErrNotFound.
type Store struct {
	st  *store.Store
	loc *time.Location
	now func() time.Time
}

// NewStore returns a Store over st. Cron schedules are evaluated in loc
// (nil = time.Local).
func NewStore(st *store.Store, loc *time.Location) *Store {
	if loc == nil {
		loc = time.Local
	}
	return &Store{st: st, loc: loc, now: time.Now}
}

// SetClock overrides the time source (tests only).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Location returns the time zone cron schedules are evaluated in.
func (s *Store) Location() *time.Location { return s.loc }

func fromRow(r *store.Routine) Routine {
	out := Routine{ID: r.ID, UserID: r.UserID, Name: r.Name, Prompt: r.Prompt, Model: r.Model,
		Deliver: r.Deliver, Enabled: r.Enabled, NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if out.Deliver == nil {
		out.Deliver = []string{}
	}
	if r.TriggerKind == "cron" {
		out.Trigger.Cron = r.TriggerSpec
	} else {
		out.Trigger.Event = r.TriggerSpec
	}
	return out
}

func toRow(r Routine) *store.Routine {
	out := &store.Routine{ID: r.ID, UserID: r.UserID, Name: r.Name, Prompt: r.Prompt, Model: r.Model,
		Deliver: r.Deliver, Enabled: r.Enabled, NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt}
	if r.Trigger.Cron != "" {
		out.TriggerKind, out.TriggerSpec = "cron", r.Trigger.Cron
	} else {
		out.TriggerKind, out.TriggerSpec = "event", r.Trigger.Event
	}
	return out
}

func fromRunRow(r *store.RoutineRun) Run {
	return Run{ID: r.ID, RoutineID: r.RoutineID, UserID: r.UserID, Source: r.Source, Event: r.Event,
		ConversationID: r.ConversationID, ScheduledFor: r.ScheduledFor, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, Status: r.Status, Error: r.Error, Reply: r.Reply}
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

// NextRun returns the next activation of a cron routine strictly after
// `after`, or the zero time for event routines.
func (s *Store) NextRun(r Routine, after time.Time) time.Time {
	if r.Trigger.Cron == "" {
		return time.Time{}
	}
	sch, err := ParseCron(r.Trigger.Cron)
	if err != nil {
		return time.Time{}
	}
	return sch.Next(after, s.loc)
}

// schedule sets r.NextRunAt from now for enabled cron routines.
func (s *Store) schedule(r *Routine) {
	r.NextRunAt = time.Time{}
	if r.Enabled {
		r.NextRunAt = s.NextRun(*r, s.now())
	}
}

// Create validates r and stores it for userID (r.ID and r.UserID are
// ignored). The first run time is computed from now.
func (s *Store) Create(ctx context.Context, userID int64, r Routine) (Routine, error) {
	if err := r.Normalize(); err != nil {
		return Routine{}, err
	}
	r.ID, r.UserID, r.LastRunAt = 0, userID, time.Time{}
	s.schedule(&r)
	row, err := s.st.CreateRoutine(ctx, toRow(r))
	if err != nil {
		return Routine{}, mapErr(err)
	}
	return fromRow(row), nil
}

// Get returns userID's routine id.
func (s *Store) Get(ctx context.Context, userID, id int64) (Routine, error) {
	row, err := s.st.GetRoutine(ctx, userID, id)
	if err != nil {
		return Routine{}, mapErr(err)
	}
	return fromRow(row), nil
}

// GetByName returns userID's routine called name.
func (s *Store) GetByName(ctx context.Context, userID int64, name string) (Routine, error) {
	row, err := s.st.GetRoutineByName(ctx, userID, name)
	if err != nil {
		return Routine{}, mapErr(err)
	}
	return fromRow(row), nil
}

// List returns userID's routines ordered by name.
func (s *Store) List(ctx context.Context, userID int64) ([]Routine, error) {
	rows, err := s.st.ListRoutines(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Routine, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Update validates r and replaces userID's routine r.ID with it. The next
// run time is recomputed from now (so changing the schedule or re-enabling
// a routine never fires it for times that passed while it was disabled).
func (s *Store) Update(ctx context.Context, userID int64, r Routine) (Routine, error) {
	if err := r.Normalize(); err != nil {
		return Routine{}, err
	}
	r.UserID = userID
	s.schedule(&r)
	row, err := s.st.UpdateRoutine(ctx, toRow(r))
	if err != nil {
		return Routine{}, mapErr(err)
	}
	return fromRow(row), nil
}

// Delete removes userID's routine id and its run history.
func (s *Store) Delete(ctx context.Context, userID, id int64) error {
	return mapErr(s.st.DeleteRoutine(ctx, userID, id))
}

// Due returns enabled cron routines (of all users) whose next run time is
// at or before now.
func (s *Store) Due(ctx context.Context, now time.Time) ([]Routine, error) {
	rows, err := s.st.DueRoutines(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]Routine, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// ForEvent returns the enabled routines triggered by event for userID (0 =
// every user).
func (s *Store) ForEvent(ctx context.Context, event string, userID int64) ([]Routine, error) {
	rows, err := s.st.EventRoutines(ctx, event, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Routine, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// MarkStarted records the start of a run of r.
//
// For source SourceSchedule it is also the claim: in one transaction it
// checks that r is still enabled with the next_run_at the caller read,
// advances next_run_at to the first activation after now (not after the
// missed time, so any number of missed activations collapse into this one
// run), and inserts the run. Because next_run_at is persisted before the
// run executes, a crash or restart can never run the same activation twice.
// A lost claim returns ErrConflict.
func (s *Store) MarkStarted(ctx context.Context, r Routine, source, event string) (Run, error) {
	now := s.now()
	run := &store.RoutineRun{RoutineID: r.ID, UserID: r.UserID, Source: source, Event: event,
		StartedAt: now, Status: StatusRunning}
	var claim *store.RoutineClaim
	if source == SourceSchedule {
		run.ScheduledFor = r.NextRunAt
		claim = &store.RoutineClaim{Expected: r.NextRunAt, Next: s.NextRun(r, now)}
	}
	row, err := s.st.StartRoutineRun(ctx, run, claim)
	if err != nil {
		return Run{}, mapErr(err)
	}
	return fromRunRow(row), nil
}

// MarkFinished records run's final Status, Error and Reply. It returns the
// stored run.
func (s *Store) MarkFinished(ctx context.Context, run Run) (Run, error) {
	if err := s.st.FinishRoutineRun(ctx, run.ID, run.Status, run.Error, run.Reply, s.now()); err != nil {
		return Run{}, mapErr(err)
	}
	return s.GetRun(ctx, run.UserID, run.ID)
}

// SetRunConversation records the conversation a run uses.
func (s *Store) SetRunConversation(ctx context.Context, runID int64, conversationID string) error {
	return s.st.SetRoutineRunConversation(ctx, runID, conversationID)
}

// SetRunStatus sets the status of an unfinished run (running <->
// waiting_approval).
func (s *Store) SetRunStatus(ctx context.Context, runID int64, status string) error {
	return s.st.SetRoutineRunStatus(ctx, runID, status)
}

// GetRun returns userID's run runID.
func (s *Store) GetRun(ctx context.Context, userID, runID int64) (Run, error) {
	row, err := s.st.GetRoutineRun(ctx, userID, runID)
	if err != nil {
		return Run{}, mapErr(err)
	}
	return fromRunRow(row), nil
}

// Runs returns the runs of userID's routine routineID, newest first
// (limit <= 0 = all). ErrNotFound if the routine is not userID's.
func (s *Store) Runs(ctx context.Context, userID, routineID int64, limit int) ([]Run, error) {
	if _, err := s.Get(ctx, userID, routineID); err != nil {
		return nil, err
	}
	rows, err := s.st.ListRoutineRuns(ctx, userID, routineID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRunRow(r))
	}
	return out, nil
}

// AbandonInterrupted marks runs left unfinished by a previous daemon
// process as canceled and returns how many there were. Call it at startup
// before the scheduler runs.
func (s *Store) AbandonInterrupted(ctx context.Context) (int, error) {
	return s.st.AbandonRoutineRuns(ctx, StatusCanceled, "interrupted: the daemon stopped while this run was in progress")
}
