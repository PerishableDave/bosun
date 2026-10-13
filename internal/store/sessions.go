package store

import (
	"context"
	"fmt"
	"time"
)

// Session is a sessions row. Only the hash of the bearer token is stored.
type Session struct {
	ID         int64
	UserID     int64
	TokenHash  []byte
	Label      string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt time.Time
}

const sessionCols = `id, user_id, token_hash, label, created_at, expires_at, last_used_at`

func scanSession(r interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	var c, e, l int64
	if err := r.Scan(&x.ID, &x.UserID, &x.TokenHash, &x.Label, &c, &e, &l); err != nil {
		return nil, notFound(err)
	}
	x.CreatedAt, x.ExpiresAt, x.LastUsedAt = fromMS(c), fromMS(e), fromMS(l)
	return &x, nil
}

// CreateSession stores a new session for userID.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash []byte, label string, expiresAt time.Time) (*Session, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(user_id, token_hash, label, created_at, expires_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, tokenHash, label, ms(now), ms(expiresAt), ms(now))
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create session: %w", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &Session{ID: id, UserID: userID, TokenHash: tokenHash, Label: label,
		CreatedAt: fromMS(ms(now)), ExpiresAt: fromMS(ms(expiresAt)), LastUsedAt: fromMS(ms(now))}, nil
}

// GetSessionByTokenHash looks up a session by token hash. Expiry is not
// checked here; callers decide.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (*Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE token_hash = ?`, tokenHash))
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return x, nil
}

// ListSessions returns a user's sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// TouchSession records that a session was just used.
func (s *Store) TouchSession(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id = ?`, ms(s.now()), id); err != nil {
		return fmt.Errorf("touch session %d: %w", id, err)
	}
	return nil
}

// DeleteSession removes a session by ID.
func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session %d: %w", id, err)
	}
	return nil
}

// DeleteUserSessions removes all of a user's sessions (e.g. on password change).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions for user %d: %w", userID, err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that expired before now and returns
// how many were removed.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ms(s.now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
