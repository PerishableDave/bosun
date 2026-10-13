package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Credential is a credentials row. Nonce and Ciphertext are opaque to the
// store; encryption and decryption live in internal/vault.
type Credential struct {
	ID          int64
	UserID      *int64 // nil for a shared (admin-level) credential
	Integration string
	Kind        string
	Nonce       []byte
	Ciphertext  []byte
	ExpiresAt   time.Time // zero if it does not expire
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const credentialCols = `id, user_id, integration, kind, nonce, ciphertext, expires_at, created_at, updated_at`

func scanCredential(r interface{ Scan(...any) error }) (*Credential, error) {
	var c Credential
	var uid, exp sql.NullInt64
	var created, updated int64
	if err := r.Scan(&c.ID, &uid, &c.Integration, &c.Kind, &c.Nonce, &c.Ciphertext, &exp, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	if uid.Valid {
		v := uid.Int64
		c.UserID = &v
	}
	c.ExpiresAt, c.CreatedAt, c.UpdatedAt = fromMS(exp.Int64), fromMS(created), fromMS(updated)
	return &c, nil
}

// credUserKey maps a nil (shared) user to 0, matching the credentials_unique
// index expression ifnull(user_id, 0). Real user IDs are always >= 1.
func credUserKey(userID *int64) int64 {
	if userID == nil {
		return 0
	}
	return *userID
}

func credUserArg(userID *int64) sql.NullInt64 {
	if userID == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *userID, Valid: true}
}

// UpsertCredential creates or replaces the credential for (UserID,
// Integration, Kind). CreatedAt is preserved on replace.
func (s *Store) UpsertCredential(ctx context.Context, c Credential) (*Credential, error) {
	if c.Integration == "" || c.Kind == "" {
		return nil, fmt.Errorf("upsert credential: integration and kind are required")
	}
	if c.UserID != nil && *c.UserID <= 0 {
		return nil, fmt.Errorf("upsert credential: invalid user id %d", *c.UserID)
	}
	if len(c.Nonce) == 0 || len(c.Ciphertext) == 0 {
		return nil, fmt.Errorf("upsert credential: nonce and ciphertext are required")
	}
	now := ms(s.now())
	var exp sql.NullInt64
	if !c.ExpiresAt.IsZero() {
		exp = sql.NullInt64{Int64: ms(c.ExpiresAt), Valid: true}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("upsert credential: begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE credentials SET nonce = ?, ciphertext = ?, expires_at = ?, updated_at = ?
		WHERE ifnull(user_id, 0) = ? AND integration = ? AND kind = ?`,
		c.Nonce, c.Ciphertext, exp, now, credUserKey(c.UserID), c.Integration, c.Kind)
	if err != nil {
		return nil, fmt.Errorf("upsert credential: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credentials(user_id, integration, kind, nonce, ciphertext, expires_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			credUserArg(c.UserID), c.Integration, c.Kind, c.Nonce, c.Ciphertext, exp, now, now); err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("upsert credential: %w", ErrConflict)
			}
			return nil, fmt.Errorf("upsert credential: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("upsert credential: commit: %w", err)
	}
	return s.GetCredential(ctx, c.UserID, c.Integration, c.Kind)
}

// GetCredential returns the credential for (userID, integration, kind), where
// a nil userID selects the shared row. Returns ErrNotFound if absent.
func (s *Store) GetCredential(ctx context.Context, userID *int64, integration, kind string) (*Credential, error) {
	c, err := scanCredential(s.db.QueryRowContext(ctx,
		`SELECT `+credentialCols+` FROM credentials WHERE ifnull(user_id, 0) = ? AND integration = ? AND kind = ?`,
		credUserKey(userID), integration, kind))
	if err != nil {
		return nil, fmt.Errorf("get credential %s/%s: %w", integration, kind, err)
	}
	return c, nil
}

// DeleteCredential removes the credential for (userID, integration, kind).
// Returns ErrNotFound if absent.
func (s *Store) DeleteCredential(ctx context.Context, userID *int64, integration, kind string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM credentials WHERE ifnull(user_id, 0) = ? AND integration = ? AND kind = ?`,
		credUserKey(userID), integration, kind)
	if err != nil {
		return fmt.Errorf("delete credential %s/%s: %w", integration, kind, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete credential %s/%s: %w", integration, kind, ErrNotFound)
	}
	return nil
}

// ListCredentials returns the credentials owned by userID (nil = shared rows
// only), optionally filtered by integration ("" = all), ordered by
// integration and kind.
func (s *Store) ListCredentials(ctx context.Context, userID *int64, integration string) ([]*Credential, error) {
	q := `SELECT ` + credentialCols + ` FROM credentials WHERE ifnull(user_id, 0) = ?`
	args := []any{credUserKey(userID)}
	if integration != "" {
		q += ` AND integration = ?`
		args = append(args, integration)
	}
	q += ` ORDER BY integration, kind`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()
	var out []*Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
