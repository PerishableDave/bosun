package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Routine is a routines row. TriggerKind is "cron" or "event" and
// TriggerSpec the cron expression or event name. The routines package owns
// validation and next-run computation.
type Routine struct {
	ID          int64
	UserID      int64
	Name        string
	TriggerKind string
	TriggerSpec string
	Prompt      string
	Model       string
	Deliver     []string
	Enabled     bool
	NextRunAt   time.Time // zero if none (event routines, disabled routines)
	LastRunAt   time.Time // zero if never run
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RoutineRun is a routine_runs row.
type RoutineRun struct {
	ID             int64
	RoutineID      int64
	UserID         int64
	Source         string // "schedule" | "event" | "manual"
	Event          string // event name for source "event"
	ScheduledFor   time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	Status         string // "running" | "waiting_approval" | "done" | "error" | "canceled"
	ConversationID string
	Error          string
	Reply          string
}

const routineCols = `id, user_id, name, trigger_kind, trigger_spec, prompt, model, deliver, enabled,
	next_run_at, last_run_at, created_at, updated_at`

func scanRoutine(r interface{ Scan(...any) error }) (*Routine, error) {
	var rt Routine
	var deliver string
	var next, last sql.NullInt64
	var cr, up int64
	if err := r.Scan(&rt.ID, &rt.UserID, &rt.Name, &rt.TriggerKind, &rt.TriggerSpec, &rt.Prompt, &rt.Model,
		&deliver, &rt.Enabled, &next, &last, &cr, &up); err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal([]byte(deliver), &rt.Deliver); err != nil {
		return nil, fmt.Errorf("routine %d: decode deliver: %w", rt.ID, err)
	}
	rt.NextRunAt, rt.LastRunAt, rt.CreatedAt, rt.UpdatedAt = fromMS(next.Int64), fromMS(last.Int64), fromMS(cr), fromMS(up)
	return &rt, nil
}

func scanRoutines(rows *sql.Rows) ([]*Routine, error) {
	defer rows.Close()
	var out []*Routine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func encodeDeliver(d []string) (string, error) {
	if d == nil {
		d = []string{}
	}
	b, err := json.Marshal(d)
	return string(b), err
}

// CreateRoutine inserts r (ID, CreatedAt and UpdatedAt are set from the
// insert). Returns ErrConflict if the user already has a routine with that
// name.
func (s *Store) CreateRoutine(ctx context.Context, r *Routine) (*Routine, error) {
	deliver, err := encodeDeliver(r.Deliver)
	if err != nil {
		return nil, fmt.Errorf("create routine: %w", err)
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO routines(user_id, name, trigger_kind, trigger_spec, prompt, model,
		deliver, enabled, next_run_at, last_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.UserID, r.Name, r.TriggerKind, r.TriggerSpec, r.Prompt, r.Model, deliver, r.Enabled,
		nullInt(ms(r.NextRunAt)), nullInt(ms(r.LastRunAt)), ms(now), ms(now))
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create routine %q: %w", r.Name, ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create routine: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create routine: %w", err)
	}
	return s.GetRoutine(ctx, r.UserID, id)
}

// GetRoutine returns a routine only if it belongs to userID; otherwise
// ErrNotFound.
func (s *Store) GetRoutine(ctx context.Context, userID, id int64) (*Routine, error) {
	r, err := scanRoutine(s.db.QueryRowContext(ctx,
		`SELECT `+routineCols+` FROM routines WHERE id = ? AND user_id = ?`, id, userID))
	if err != nil {
		return nil, fmt.Errorf("get routine %d: %w", id, err)
	}
	return r, nil
}

// GetRoutineByName returns userID's routine called name.
func (s *Store) GetRoutineByName(ctx context.Context, userID int64, name string) (*Routine, error) {
	r, err := scanRoutine(s.db.QueryRowContext(ctx,
		`SELECT `+routineCols+` FROM routines WHERE user_id = ? AND name = ?`, userID, name))
	if err != nil {
		return nil, fmt.Errorf("get routine %q: %w", name, err)
	}
	return r, nil
}

// ListRoutines returns userID's routines ordered by name.
func (s *Store) ListRoutines(ctx context.Context, userID int64) ([]*Routine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routineCols+` FROM routines WHERE user_id = ? ORDER BY name, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list routines: %w", err)
	}
	out, err := scanRoutines(rows)
	if err != nil {
		return nil, fmt.Errorf("list routines: %w", err)
	}
	return out, nil
}

// UpdateRoutine replaces the editable fields (name, trigger, prompt, model,
// deliver, enabled, next_run_at) of r, which must belong to r.UserID.
func (s *Store) UpdateRoutine(ctx context.Context, r *Routine) (*Routine, error) {
	deliver, err := encodeDeliver(r.Deliver)
	if err != nil {
		return nil, fmt.Errorf("update routine: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE routines SET name = ?, trigger_kind = ?, trigger_spec = ?, prompt = ?,
		model = ?, deliver = ?, enabled = ?, next_run_at = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		r.Name, r.TriggerKind, r.TriggerSpec, r.Prompt, r.Model, deliver, r.Enabled, nullInt(ms(r.NextRunAt)),
		ms(s.now()), r.ID, r.UserID)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("update routine %q: %w", r.Name, ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("update routine: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("update routine %d: %w", r.ID, ErrNotFound)
	}
	return s.GetRoutine(ctx, r.UserID, r.ID)
}

// DeleteRoutine deletes userID's routine and its runs. Conversations it
// created are kept (their routine_id is cleared).
func (s *Store) DeleteRoutine(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM routines WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete routine: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete routine %d: %w", id, ErrNotFound)
	}
	return nil
}

// DueRoutines returns enabled cron routines whose next_run_at is at or
// before now, oldest first, across all users. Routines of disabled users
// are skipped.
func (s *Store) DueRoutines(ctx context.Context, now time.Time) ([]*Routine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routineCols+` FROM routines
		WHERE enabled = 1 AND trigger_kind = 'cron' AND next_run_at IS NOT NULL AND next_run_at <= ?
		AND user_id IN (SELECT id FROM users WHERE disabled = 0)
		ORDER BY next_run_at, id`, ms(now))
	if err != nil {
		return nil, fmt.Errorf("due routines: %w", err)
	}
	out, err := scanRoutines(rows)
	if err != nil {
		return nil, fmt.Errorf("due routines: %w", err)
	}
	return out, nil
}

// EventRoutines returns enabled routines triggered by event, for userID or,
// when userID is 0, for every (non-disabled) user.
func (s *Store) EventRoutines(ctx context.Context, event string, userID int64) ([]*Routine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routineCols+` FROM routines
		WHERE enabled = 1 AND trigger_kind = 'event' AND trigger_spec = ? AND (? = 0 OR user_id = ?)
		AND user_id IN (SELECT id FROM users WHERE disabled = 0)
		ORDER BY user_id, id`, event, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("event routines: %w", err)
	}
	out, err := scanRoutines(rows)
	if err != nil {
		return nil, fmt.Errorf("event routines: %w", err)
	}
	return out, nil
}

// RoutineClaim makes StartRoutineRun advance a cron routine's schedule
// atomically with recording the run: the run is only recorded if the
// routine is still enabled and its next_run_at still equals Expected (so
// two schedulers, or a scheduler and a stale read, cannot both run it).
type RoutineClaim struct {
	Expected time.Time // the next_run_at the caller saw
	Next     time.Time // the new next_run_at (zero = none)
}

// StartRoutineRun records run as started (status running unless set) and
// sets the routine's last_run_at to run.StartedAt; with a claim it also
// advances next_run_at, all in one transaction. A lost claim returns
// ErrConflict and records nothing.
func (s *Store) StartRoutineRun(ctx context.Context, run *RoutineRun, claim *RoutineClaim) (*RoutineRun, error) {
	if run.StartedAt.IsZero() {
		run.StartedAt = s.now()
	}
	if run.Status == "" {
		run.Status = "running"
	}
	if run.Source == "" {
		run.Source = "manual"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("start routine run: %w", err)
	}
	defer tx.Rollback()
	var res sql.Result
	if claim != nil {
		res, err = tx.ExecContext(ctx, `UPDATE routines SET next_run_at = ?, last_run_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND next_run_at = ?`,
			nullInt(ms(claim.Next)), ms(run.StartedAt), run.RoutineID, run.UserID, ms(claim.Expected))
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE routines SET last_run_at = ? WHERE id = ? AND user_id = ?`,
			ms(run.StartedAt), run.RoutineID, run.UserID)
	}
	if err != nil {
		return nil, fmt.Errorf("start routine run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if claim != nil {
			return nil, fmt.Errorf("start routine run %d: claim lost: %w", run.RoutineID, ErrConflict)
		}
		return nil, fmt.Errorf("start routine run %d: %w", run.RoutineID, ErrNotFound)
	}
	ins, err := tx.ExecContext(ctx, `INSERT INTO routine_runs(routine_id, user_id, source, event, scheduled_for,
		started_at, status, conversation_id, error, reply) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		run.RoutineID, run.UserID, run.Source, run.Event, nullInt(ms(run.ScheduledFor)), ms(run.StartedAt),
		run.Status, nullStr(run.ConversationID))
	if err != nil {
		return nil, fmt.Errorf("start routine run: %w", err)
	}
	if run.ID, err = ins.LastInsertId(); err != nil {
		return nil, fmt.Errorf("start routine run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("start routine run: %w", err)
	}
	out := *run
	out.StartedAt, out.ScheduledFor = fromMS(ms(run.StartedAt)), fromMS(ms(run.ScheduledFor))
	return &out, nil
}

// SetRoutineRunConversation records the conversation a run is using.
func (s *Store) SetRoutineRunConversation(ctx context.Context, runID int64, conversationID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET conversation_id = ? WHERE id = ?`,
		nullStr(conversationID), runID); err != nil {
		return fmt.Errorf("set routine run conversation: %w", err)
	}
	return nil
}

// SetRoutineRunStatus changes the status of a run that has not finished
// (finished runs are left alone).
func (s *Store) SetRoutineRunStatus(ctx context.Context, runID int64, status string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET status = ? WHERE id = ? AND finished_at IS NULL`,
		status, runID); err != nil {
		return fmt.Errorf("set routine run status: %w", err)
	}
	return nil
}

// FinishRoutineRun records a run's outcome. A run can be finished once;
// later calls return ErrConflict.
func (s *Store) FinishRoutineRun(ctx context.Context, runID int64, status, errText, reply string, finishedAt time.Time) error {
	if finishedAt.IsZero() {
		finishedAt = s.now()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET status = ?, error = ?, reply = ?, finished_at = ?
		WHERE id = ? AND finished_at IS NULL`, status, errText, reply, ms(finishedAt), runID)
	if err != nil {
		return fmt.Errorf("finish routine run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finish routine run %d: %w", runID, ErrConflict)
	}
	return nil
}

// AbandonRoutineRuns finishes every unfinished run (left over from a daemon
// that stopped mid-run) with status and errText, returning how many.
func (s *Store) AbandonRoutineRuns(ctx context.Context, status, errText string) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET status = ?, error = ?, finished_at = ?
		WHERE finished_at IS NULL`, status, errText, ms(s.now()))
	if err != nil {
		return 0, fmt.Errorf("abandon routine runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

const runCols = `id, routine_id, user_id, source, event, scheduled_for, started_at, finished_at, status,
	conversation_id, error, reply`

func scanRun(r interface{ Scan(...any) error }) (*RoutineRun, error) {
	var run RoutineRun
	var sched, fin sql.NullInt64
	var conv sql.NullString
	var started int64
	if err := r.Scan(&run.ID, &run.RoutineID, &run.UserID, &run.Source, &run.Event, &sched, &started, &fin,
		&run.Status, &conv, &run.Error, &run.Reply); err != nil {
		return nil, notFound(err)
	}
	run.ScheduledFor, run.StartedAt, run.FinishedAt = fromMS(sched.Int64), fromMS(started), fromMS(fin.Int64)
	run.ConversationID = conv.String
	return &run, nil
}

// GetRoutineRun returns a run only if it belongs to userID.
func (s *Store) GetRoutineRun(ctx context.Context, userID, runID int64) (*RoutineRun, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM routine_runs WHERE id = ? AND user_id = ?`,
		runID, userID))
	if err != nil {
		return nil, fmt.Errorf("get routine run %d: %w", runID, err)
	}
	return r, nil
}

// ListRoutineRuns returns userID's runs of routineID, newest first. limit
// <= 0 means no limit.
func (s *Store) ListRoutineRuns(ctx context.Context, userID, routineID int64, limit int) ([]*RoutineRun, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM routine_runs WHERE user_id = ? AND routine_id = ?
		ORDER BY started_at DESC, id DESC LIMIT ?`, userID, routineID, limit)
	if err != nil {
		return nil, fmt.Errorf("list routine runs: %w", err)
	}
	defer rows.Close()
	var out []*RoutineRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list routine runs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
