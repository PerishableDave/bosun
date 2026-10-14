package store

import (
	"context"
	"fmt"
	"time"
)

// Invite is an invites row. Only the hash of the one-time code is stored.
type Invite struct {
	ID        int64
	UserID    int64
	CodeHash  []byte
	CreatedBy int64 // 0 if unknown or since deleted
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ReplaceInvite deletes any invites for userID and stores a new one.
func (s *Store) ReplaceInvite(ctx context.Context, userID, createdBy int64, codeHash []byte, expiresAt time.Time) (*Invite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE user_id = ? OR expires_at <= ?`, userID, ms(now)); err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO invites(user_id, code_hash, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, codeHash, nullInt(createdBy), ms(now), ms(expiresAt))
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create invite: %w", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	return &Invite{ID: id, UserID: userID, CodeHash: codeHash, CreatedBy: createdBy,
		CreatedAt: fromMS(ms(now)), ExpiresAt: fromMS(ms(expiresAt))}, nil
}

// GetLiveInvite returns the unexpired invite with the given code hash, or
// ErrNotFound.
func (s *Store) GetLiveInvite(ctx context.Context, codeHash []byte) (*Invite, error) {
	var inv Invite
	var createdBy *int64
	var c, e int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, code_hash, created_by, created_at, expires_at FROM invites WHERE code_hash = ? AND expires_at > ?`,
		codeHash, ms(s.now())).Scan(&inv.ID, &inv.UserID, &inv.CodeHash, &createdBy, &c, &e)
	if err != nil {
		return nil, fmt.Errorf("get invite: %w", notFound(err))
	}
	if createdBy != nil {
		inv.CreatedBy = *createdBy
	}
	inv.CreatedAt, inv.ExpiresAt = fromMS(c), fromMS(e)
	return &inv, nil
}

// RedeemInvite consumes the unexpired invite with codeHash in one
// transaction: it deletes every invite of that user, sets their password
// hash, revokes their sessions and returns their ID. It returns ErrNotFound
// if the code is unknown, expired or already used (so a code works once
// even under concurrent use).
func (s *Store) RedeemInvite(ctx context.Context, codeHash []byte, passwordHash string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("redeem invite: %w", err)
	}
	defer tx.Rollback()
	now := ms(s.now())
	var userID int64
	if err := tx.QueryRowContext(ctx, `DELETE FROM invites WHERE code_hash = ? AND expires_at > ? RETURNING user_id`,
		codeHash, now).Scan(&userID); err != nil {
		return 0, fmt.Errorf("redeem invite: %w", notFound(err))
	}
	for _, q := range []string{
		`DELETE FROM invites WHERE user_id = ?`,
		`DELETE FROM sessions WHERE user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return 0, fmt.Errorf("redeem invite: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, now, userID); err != nil {
		return 0, fmt.Errorf("redeem invite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("redeem invite: %w", err)
	}
	return userID, nil
}

// PendingInvites returns the expiry of each user's unexpired invite.
func (s *Store) PendingInvites(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, max(expires_at) FROM invites WHERE expires_at > ? GROUP BY user_id`, ms(s.now()))
	if err != nil {
		return nil, fmt.Errorf("pending invites: %w", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id, e int64
		if err := rows.Scan(&id, &e); err != nil {
			return nil, fmt.Errorf("pending invites: %w", err)
		}
		out[id] = fromMS(e)
	}
	return out, rows.Err()
}

// DeleteUserSessionsExcept removes all of a user's sessions except the one
// with keepTokenHash (e.g. after a password change from that session).
func (s *Store) DeleteUserSessionsExcept(ctx context.Context, userID int64, keepTokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keepTokenHash); err != nil {
		return fmt.Errorf("delete other sessions for user %d: %w", userID, err)
	}
	return nil
}
