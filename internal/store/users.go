package store

import (
	"context"
	"fmt"
	"time"
)

// User is a users row.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string // "admin" | "member"
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const userCols = `id, username, password_hash, role, disabled, created_at, updated_at`

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, updated int64
	if err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt, u.UpdatedAt = fromMS(created), fromMS(updated)
	return &u, nil
}

// CreateUser inserts a user. Returns ErrConflict if the username is taken
// (case-insensitive).
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		username, passwordHash, role, ms(now), ms(now))
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create user %q: %w", username, ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	return s.GetUser(ctx, id)
}

// GetUser returns the user with the given ID.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, nil
}

// GetUserByUsername returns the user with the given username (case-insensitive).
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if err != nil {
		return nil, fmt.Errorf("get user %q: %w", username, err)
	}
	return u, nil
}

// ListUsers returns all users ordered by ID.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// UpdateUserPassword replaces a user's password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	return s.updateUser(ctx, id, `password_hash = ?`, passwordHash)
}

// SetUserRole changes a user's role.
func (s *Store) SetUserRole(ctx context.Context, id int64, role string) error {
	return s.updateUser(ctx, id, `role = ?`, role)
}

// SetUserDisabled enables or disables a user.
func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabled bool) error {
	return s.updateUser(ctx, id, `disabled = ?`, disabled)
}

func (s *Store) updateUser(ctx context.Context, id int64, set string, v any) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET `+set+`, updated_at = ? WHERE id = ?`, v, ms(s.now()), id)
	if err != nil {
		return fmt.Errorf("update user %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update user %d: %w", id, ErrNotFound)
	}
	return nil
}
