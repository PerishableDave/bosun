package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/memory"
)

// Memories is the subset of *memory.Store the memory endpoints need. Every
// method is scoped by user ID.
type Memories interface {
	Add(ctx context.Context, userID int64, kind memory.Kind, content, source string) (memory.Memory, error)
	Update(ctx context.Context, userID, id int64, content string) (memory.Memory, error)
	Delete(ctx context.Context, userID, id int64) error
	List(ctx context.Context, userID int64, kind memory.Kind, limit, offset int) ([]memory.Memory, error)
	SearchKind(ctx context.Context, userID int64, query string, kind memory.Kind, limit, offset int) ([]memory.Memory, error)
}

var _ Memories = (*memory.Store)(nil)

// Memory list paging.
const (
	DefaultMemoryLimit = 50
	MaxMemoryLimit     = 500
)

// MemorySourceUser is the source recorded for memories created through the
// API.
const MemorySourceUser = "user"

// MemoryResponse is one memory.
type MemoryResponse struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryListResponse is the body of GET /v1/memory.
type MemoryListResponse struct {
	Memories []MemoryResponse `json:"memories"`
}

// CreateMemoryRequest is the body of POST /v1/memory.
type CreateMemoryRequest struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// UpdateMemoryRequest is the body of PUT /v1/memory/{id}.
type UpdateMemoryRequest struct {
	Content string `json:"content"`
}

func toMemory(m memory.Memory) MemoryResponse {
	return MemoryResponse{ID: m.ID, Kind: string(m.Kind), Content: m.Content, Source: m.Source,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
}

// memoryError writes the response for a memory store error.
func (s *Server) memoryError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, memory.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "memory not found")
	case errors.Is(err, memory.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", strings.TrimPrefix(err.Error(), "memory: invalid: "))
	default:
		s.internalError(w, r, what, err)
	}
}

func queryInt(r *http.Request, key string, def, lo, hi int) (int, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	if s.memories == nil {
		notImplemented(w)
		return
	}
	limit, ok := queryInt(r, "limit", DefaultMemoryLimit, 1, MaxMemoryLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer from 1 to "+strconv.Itoa(MaxMemoryLimit))
		return
	}
	offset, ok := queryInt(r, "offset", 0, 0, 1<<30)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "offset must be a non-negative integer")
		return
	}
	kind := memory.Kind(r.URL.Query().Get("kind"))
	if kind != "" && !kind.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "kind must be fact, preference, summary, or note")
		return
	}
	u, _ := UserFromContext(r.Context())
	var ms []memory.Memory
	var err error
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		ms, err = s.memories.SearchKind(r.Context(), u.ID, q, kind, limit, offset)
	} else {
		ms, err = s.memories.List(r.Context(), u.ID, kind, limit, offset)
	}
	if err != nil {
		s.memoryError(w, r, "list memories", err)
		return
	}
	out := MemoryListResponse{Memories: make([]MemoryResponse, 0, len(ms))}
	for _, m := range ms {
		out.Memories = append(out.Memories, toMemory(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	var req CreateMemoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.memories == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	m, err := s.memories.Add(r.Context(), u.ID, memory.Kind(req.Kind), req.Content, MemorySourceUser)
	if err != nil {
		s.memoryError(w, r, "create memory", err)
		return
	}
	writeJSON(w, http.StatusCreated, toMemory(m))
}

// memoryID parses {id}; a malformed id is reported as not found.
func memoryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "memory not found")
		return 0, false
	}
	return id, true
}

func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) {
	var req UpdateMemoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.memories == nil {
		notImplemented(w)
		return
	}
	id, ok := memoryID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	m, err := s.memories.Update(r.Context(), u.ID, id, req.Content)
	if err != nil {
		s.memoryError(w, r, "update memory", err)
		return
	}
	writeJSON(w, http.StatusOK, toMemory(m))
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if s.memories == nil {
		notImplemented(w)
		return
	}
	id, ok := memoryID(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	if err := s.memories.Delete(r.Context(), u.ID, id); err != nil {
		s.memoryError(w, r, "delete memory", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
