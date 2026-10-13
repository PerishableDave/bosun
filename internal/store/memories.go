package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// MemoryRow is a memories row. Every query below is scoped by user_id; there
// is no function that reads or writes another user's memories.
type MemoryRow struct {
	ID        int64
	UserID    int64
	Kind      string // "fact" | "preference" | "summary" | "note"
	Content   string
	Source    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const memoryCols = `m.id, m.user_id, m.kind, m.content, m.source, m.created_at, m.updated_at`

func scanMemory(r interface{ Scan(...any) error }) (*MemoryRow, error) {
	var x MemoryRow
	var c, u int64
	if err := r.Scan(&x.ID, &x.UserID, &x.Kind, &x.Content, &x.Source, &c, &u); err != nil {
		return nil, notFound(err)
	}
	x.CreatedAt, x.UpdatedAt = fromMS(c), fromMS(u)
	return &x, nil
}

func (s *Store) queryMemories(ctx context.Context, what, q string, args ...any) ([]*MemoryRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []*MemoryRow
	for rows.Next() {
		x, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// CreateMemory inserts a memory owned by userID.
func (s *Store) CreateMemory(ctx context.Context, userID int64, kind, content, source string) (*MemoryRow, error) {
	now := ms(s.now())
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO memories(user_id, kind, content, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, kind, content, source, now, now)
	if err != nil {
		return nil, fmt.Errorf("create memory: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create memory: %w", err)
	}
	return &MemoryRow{ID: id, UserID: userID, Kind: kind, Content: content, Source: source,
		CreatedAt: fromMS(now), UpdatedAt: fromMS(now)}, nil
}

// GetMemory returns the memory only if it belongs to userID; otherwise
// ErrNotFound.
func (s *Store) GetMemory(ctx context.Context, userID, id int64) (*MemoryRow, error) {
	x, err := scanMemory(s.db.QueryRowContext(ctx,
		`SELECT `+memoryCols+` FROM memories m WHERE m.id = ? AND m.user_id = ?`, id, userID))
	if err != nil {
		return nil, fmt.Errorf("get memory %d: %w", id, err)
	}
	return x, nil
}

// UpdateMemoryContent replaces the content of the caller's memory.
func (s *Store) UpdateMemoryContent(ctx context.Context, userID, id int64, content string) (*MemoryRow, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET content = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		content, ms(s.now()), id, userID)
	if err != nil {
		return nil, fmt.Errorf("update memory %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("update memory %d: %w", id, ErrNotFound)
	}
	return s.GetMemory(ctx, userID, id)
}

// DeleteMemory removes the caller's memory.
func (s *Store) DeleteMemory(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete memory %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete memory %d: %w", id, ErrNotFound)
	}
	return nil
}

// ListMemories returns the user's memories, newest first. kind "" means all
// kinds. limit <= 0 means no limit.
func (s *Store) ListMemories(ctx context.Context, userID int64, kind string, limit, offset int) ([]*MemoryRow, error) {
	q := `SELECT ` + memoryCols + ` FROM memories m WHERE m.user_id = ?`
	args := []any{userID}
	if kind != "" {
		q += ` AND m.kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY m.updated_at DESC, m.id DESC LIMIT ? OFFSET ?`
	args = append(args, limitArg(limit), max(offset, 0))
	return s.queryMemories(ctx, "list memories", q, args...)
}

// SearchMemoriesFTS runs an FTS5 MATCH expression over the user's memories,
// best match (lowest bm25) first. match is passed to FTS5 verbatim, so the
// caller must build it safely; a malformed expression returns an error.
func (s *Store) SearchMemoriesFTS(ctx context.Context, userID int64, kind, match string, limit, offset int) ([]*MemoryRow, error) {
	q := `SELECT ` + memoryCols + ` FROM memories_fts JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ? AND m.user_id = ?`
	args := []any{match, userID}
	if kind != "" {
		q += ` AND m.kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY bm25(memories_fts), m.id DESC LIMIT ? OFFSET ?`
	args = append(args, limitArg(limit), max(offset, 0))
	return s.queryMemories(ctx, "search memories", q, args...)
}

// SearchMemoriesLike returns the user's memories whose content contains
// every term (case-insensitive for ASCII), newest first. It is the fallback
// when a query cannot be expressed in FTS5 syntax.
func (s *Store) SearchMemoriesLike(ctx context.Context, userID int64, kind string, terms []string, limit, offset int) ([]*MemoryRow, error) {
	q := `SELECT ` + memoryCols + ` FROM memories m WHERE m.user_id = ?`
	args := []any{userID}
	if kind != "" {
		q += ` AND m.kind = ?`
		args = append(args, kind)
	}
	for _, t := range terms {
		q += ` AND m.content LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(t)+"%")
	}
	q += ` ORDER BY m.updated_at DESC, m.id DESC LIMIT ? OFFSET ?`
	args = append(args, limitArg(limit), max(offset, 0))
	return s.queryMemories(ctx, "search memories (like)", q, args...)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// limitArg maps "no limit" (<= 0) to SQLite's -1.
func limitArg(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}
