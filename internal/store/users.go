package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLastAdmin is returned when a change would leave no enabled admin.
var ErrLastAdmin = errors.New("store: would remove the last enabled admin")

// User is a users row.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string // "admin" | "member"
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  time.Time // zero if never
}

const userCols = `id, username, password_hash, role, disabled, created_at, updated_at, last_login_at`

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, updated int64
	var last sql.NullInt64
	if err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &created, &updated, &last); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromMS(created), fromMS(updated), fromMS(last.Int64)
	return &u, nil
}

// CreateUser inserts a user. Returns ErrConflict if the username is taken
// (case-insensitive). The new ID is above every existing and deleted user's
// ID, so a removed user's ID is never handed to someone new.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role, created_at, updated_at)
		 VALUES (max(ifnull((SELECT max(id) FROM users), 0), ifnull((SELECT max(id) FROM deleted_users), 0)) + 1, ?, ?, ?, ?, ?)`,
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

// TouchUserLogin records a successful login.
func (s *Store) TouchUserLogin(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, ms(s.now()), id); err != nil {
		return fmt.Errorf("touch user login %d: %w", id, err)
	}
	return nil
}

// otherEnabledAdmins counts enabled admins other than id.
func otherEnabledAdmins(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id != ?`, id).Scan(&n)
	return n, err
}

// UpdateUserAccess changes a user's role and/or disabled flag (nil leaves a
// field unchanged). It returns ErrLastAdmin if the change would leave no
// enabled admin, and ErrNotFound for an unknown user. Disabling a user also
// revokes their sessions and pending invites.
func (s *Store) UpdateUserAccess(ctx context.Context, id int64, role *string, disabled *bool) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update user %d: %w", id, err)
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("update user %d: %w", id, err)
	}
	newRole, newDisabled := u.Role, u.Disabled
	if role != nil {
		newRole = *role
	}
	if disabled != nil {
		newDisabled = *disabled
	}
	if u.Role == "admin" && !u.Disabled && (newRole != "admin" || newDisabled) {
		n, err := otherEnabledAdmins(ctx, tx, id)
		if err != nil {
			return nil, fmt.Errorf("update user %d: %w", id, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("update user %d: %w", id, ErrLastAdmin)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?`,
		newRole, newDisabled, ms(s.now()), id); err != nil {
		return nil, fmt.Errorf("update user %d: %w", id, err)
	}
	if newDisabled && !u.Disabled {
		for _, q := range []string{`DELETE FROM sessions WHERE user_id = ?`, `DELETE FROM invites WHERE user_id = ?`} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return nil, fmt.Errorf("update user %d: %w", id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update user %d: %w", id, err)
	}
	return s.GetUser(ctx, id)
}

// userDataDeletes removes everything a user owns, children first. Most of
// these rows would also go by ON DELETE CASCADE; deleting explicitly keeps
// the result independent of the foreign_keys pragma and fires the
// memories_fts triggers. The audit log is deliberately kept (append-only;
// entries outlive users). Each statement takes the user ID for every "?".
var userDataDeletes = []string{
	`DELETE FROM approvals WHERE user_id = ?`,
	`DELETE FROM routine_runs WHERE user_id = ? OR routine_id IN (SELECT id FROM routines WHERE user_id = ?)`,
	`DELETE FROM channel_links WHERE user_id = ?`,
	`DELETE FROM messages WHERE conversation_id IN (SELECT id FROM conversations WHERE user_id = ?)`,
	`DELETE FROM conversations WHERE user_id = ?`,
	`DELETE FROM routines WHERE user_id = ?`,
	`DELETE FROM memories WHERE user_id = ?`,
	`DELETE FROM policy_rules WHERE user_id = ?`,
	`DELETE FROM credentials WHERE user_id = ?`,
	`DELETE FROM sessions WHERE user_id = ?`,
	`DELETE FROM invites WHERE user_id = ?`,
	// References from other users' rows are cleared (ON DELETE SET NULL).
	`UPDATE approvals SET resolved_by = NULL WHERE resolved_by = ?`,
	`UPDATE hooks SET created_by = NULL WHERE created_by = ?`,
	`UPDATE invites SET created_by = NULL WHERE created_by = ?`,
	`DELETE FROM users WHERE id = ?`,
}

// DeleteUser removes a user and all their data in one transaction. It
// returns ErrLastAdmin if the user is the last enabled admin and ErrNotFound
// for an unknown user. The ID is recorded in deleted_users so it is never
// reused.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	if u.Role == "admin" && !u.Disabled {
		n, err := otherEnabledAdmins(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("delete user %d: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("delete user %d: %w", id, ErrLastAdmin)
		}
	}
	for _, q := range userDataDeletes {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("delete user %d: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO deleted_users(id, deleted_at) VALUES (?, ?)`, id, ms(s.now())); err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	return nil
}
