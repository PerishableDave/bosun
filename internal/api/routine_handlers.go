package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dperedo/bosun/internal/routines"
)

// Routines is what the routine endpoints need (usually
// routines.Service). Every method is scoped by user ID; another user's
// routine is routines.ErrNotFound.
type Routines interface {
	List(ctx context.Context, userID int64) ([]routines.Routine, error)
	Create(ctx context.Context, userID int64, r routines.Routine) (routines.Routine, error)
	Get(ctx context.Context, userID, id int64) (routines.Routine, error)
	Update(ctx context.Context, userID int64, r routines.Routine) (routines.Routine, error)
	Delete(ctx context.Context, userID, id int64) error
	RunNow(ctx context.Context, userID, id int64) (routines.Run, error)
	Runs(ctx context.Context, userID, routineID int64, limit int) ([]routines.Run, error)
}

var _ Routines = routines.Service{}

// Routine run list paging.
const (
	DefaultRoutineRunLimit = 20
	MaxRoutineRunLimit     = 200
)

// RoutineTrigger is a routine's trigger: exactly one of Cron or Event.
type RoutineTrigger struct {
	Cron  string `json:"cron,omitempty"`
	Event string `json:"event,omitempty"`
}

// RoutineResponse describes a routine.
type RoutineResponse struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Prompt    string         `json:"prompt"`
	Trigger   RoutineTrigger `json:"trigger"`
	Model     string         `json:"model,omitempty"`
	Deliver   []string       `json:"deliver"`
	Enabled   bool           `json:"enabled"`
	NextRunAt *time.Time     `json:"next_run_at,omitempty"`
	LastRunAt *time.Time     `json:"last_run_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// RoutineListResponse is the body of GET /v1/routines.
type RoutineListResponse struct {
	Routines []RoutineResponse `json:"routines"`
}

// CreateRoutineRequest is the body of POST /v1/routines. Enabled defaults
// to true and Deliver to ["web","cli"].
type CreateRoutineRequest struct {
	Name    string         `json:"name"`
	Prompt  string         `json:"prompt"`
	Trigger RoutineTrigger `json:"trigger"`
	Model   string         `json:"model,omitempty"`
	Deliver []string       `json:"deliver,omitempty"`
	Enabled *bool          `json:"enabled,omitempty"`
}

// UpdateRoutineRequest is the body of PUT /v1/routines/{id}. Omitted fields
// keep their current value; a trigger replaces the whole trigger; model ""
// clears the override.
type UpdateRoutineRequest struct {
	Name    *string         `json:"name,omitempty"`
	Prompt  *string         `json:"prompt,omitempty"`
	Trigger *RoutineTrigger `json:"trigger,omitempty"`
	Model   *string         `json:"model,omitempty"`
	Deliver []string        `json:"deliver,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
}

// RoutineRunResponse describes one run of a routine.
type RoutineRunResponse struct {
	ID             int64      `json:"id"`
	RoutineID      int64      `json:"routine_id"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	Event          string     `json:"event,omitempty"`
	ConversationID string     `json:"conversation_id,omitempty"`
	ScheduledFor   *time.Time `json:"scheduled_for,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Reply          string     `json:"reply,omitempty"`
}

// RoutineRunListResponse is the body of GET /v1/routines/{id}/runs.
type RoutineRunListResponse struct {
	Runs []RoutineRunResponse `json:"runs"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toRoutine(r routines.Routine) RoutineResponse {
	d := r.Deliver
	if d == nil {
		d = []string{}
	}
	return RoutineResponse{ID: r.ID, Name: r.Name, Prompt: r.Prompt,
		Trigger: RoutineTrigger{Cron: r.Trigger.Cron, Event: r.Trigger.Event}, Model: r.Model, Deliver: d,
		Enabled: r.Enabled, NextRunAt: timePtr(r.NextRunAt), LastRunAt: timePtr(r.LastRunAt),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func toRoutineRun(r routines.Run) RoutineRunResponse {
	return RoutineRunResponse{ID: r.ID, RoutineID: r.RoutineID, Status: r.Status, Source: r.Source, Event: r.Event,
		ConversationID: r.ConversationID, ScheduledFor: timePtr(r.ScheduledFor), StartedAt: r.StartedAt,
		FinishedAt: timePtr(r.FinishedAt), Error: r.Error, Reply: r.Reply}
}

// routineError writes the response for a routines error.
func (s *Server) routineError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, routines.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "routine not found")
	case errors.Is(err, routines.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", strings.TrimPrefix(err.Error(), routines.ErrInvalid.Error()+": "))
	case errors.Is(err, routines.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "you already have a routine with that name")
	case errors.Is(err, routines.ErrBusy):
		writeError(w, http.StatusConflict, "conflict", "this routine is already running")
	case errors.Is(err, routines.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the daemon is shutting down")
	default:
		s.internalError(w, r, what, err)
	}
}

// routineID parses {id}; a malformed id is reported as not found.
func routineID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "routine not found")
		return 0, false
	}
	return id, true
}

func (s *Server) handleListRoutines(w http.ResponseWriter, r *http.Request) {
	if s.routines == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	rs, err := s.routines.List(r.Context(), u.ID)
	if err != nil {
		s.routineError(w, r, "list routines", err)
		return
	}
	out := RoutineListResponse{Routines: make([]RoutineResponse, 0, len(rs))}
	for _, rt := range rs {
		out.Routines = append(out.Routines, toRoutine(rt))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateRoutine(w http.ResponseWriter, r *http.Request) {
	var req CreateRoutineRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.routines == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	rt := routines.Routine{Name: req.Name, Prompt: req.Prompt,
		Trigger: routines.Trigger{Cron: req.Trigger.Cron, Event: req.Trigger.Event},
		Model:   req.Model, Deliver: req.Deliver, Enabled: req.Enabled == nil || *req.Enabled}
	created, err := s.routines.Create(r.Context(), u.ID, rt)
	if err != nil {
		s.routineError(w, r, "create routine", err)
		return
	}
	writeJSON(w, http.StatusCreated, toRoutine(created))
}

func (s *Server) handleGetRoutine(w http.ResponseWriter, r *http.Request) {
	if s.routines == nil {
		notImplemented(w)
		return
	}
	id, ok := routineID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	rt, err := s.routines.Get(r.Context(), u.ID, id)
	if err != nil {
		s.routineError(w, r, "get routine", err)
		return
	}
	writeJSON(w, http.StatusOK, toRoutine(rt))
}

func (s *Server) handleUpdateRoutine(w http.ResponseWriter, r *http.Request) {
	var req UpdateRoutineRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.routines == nil {
		notImplemented(w)
		return
	}
	id, ok := routineID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	rt, err := s.routines.Get(r.Context(), u.ID, id)
	if err != nil {
		s.routineError(w, r, "get routine", err)
		return
	}
	if req.Name != nil {
		rt.Name = *req.Name
	}
	if req.Prompt != nil {
		rt.Prompt = *req.Prompt
	}
	if req.Trigger != nil {
		rt.Trigger = routines.Trigger{Cron: req.Trigger.Cron, Event: req.Trigger.Event}
	}
	if req.Model != nil {
		rt.Model = *req.Model
	}
	if req.Deliver != nil {
		rt.Deliver = req.Deliver
	}
	if req.Enabled != nil {
		rt.Enabled = *req.Enabled
	}
	updated, err := s.routines.Update(r.Context(), u.ID, rt)
	if err != nil {
		s.routineError(w, r, "update routine", err)
		return
	}
	writeJSON(w, http.StatusOK, toRoutine(updated))
}

func (s *Server) handleDeleteRoutine(w http.ResponseWriter, r *http.Request) {
	if s.routines == nil {
		notImplemented(w)
		return
	}
	id, ok := routineID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	if err := s.routines.Delete(r.Context(), u.ID, id); err != nil {
		s.routineError(w, r, "delete routine", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunRoutine(w http.ResponseWriter, r *http.Request) {
	if s.routines == nil {
		notImplemented(w)
		return
	}
	id, ok := routineID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	run, err := s.routines.RunNow(r.Context(), u.ID, id)
	if err != nil {
		s.routineError(w, r, "run routine", err)
		return
	}
	writeJSON(w, http.StatusAccepted, toRoutineRun(run))
}

func (s *Server) handleListRoutineRuns(w http.ResponseWriter, r *http.Request) {
	if s.routines == nil {
		notImplemented(w)
		return
	}
	id, ok := routineID(w, r)
	if !ok {
		return
	}
	limit, ok := queryInt(r, "limit", DefaultRoutineRunLimit, 1, MaxRoutineRunLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer from 1 to "+strconv.Itoa(MaxRoutineRunLimit))
		return
	}
	u, _ := UserFromContext(r.Context())
	runs, err := s.routines.Runs(r.Context(), u.ID, id, limit)
	if err != nil {
		s.routineError(w, r, "list routine runs", err)
		return
	}
	out := RoutineRunListResponse{Runs: make([]RoutineRunResponse, 0, len(runs))}
	for _, run := range runs {
		out.Runs = append(out.Runs, toRoutineRun(run))
	}
	writeJSON(w, http.StatusOK, out)
}
