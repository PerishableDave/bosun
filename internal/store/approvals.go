package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Approval statuses as stored.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
	ApprovalCanceled = "canceled"
)

// Approval is an approvals row.
type Approval struct {
	ID             string
	UserID         int64
	ConversationID string
	ToolCallID     string
	Tool           string
	Capability     string
	Risk           string
	Scope          string // the call's normalized scope ("" if unscoped)
	Args           json.RawMessage
	Status         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ResolvedAt     time.Time // zero while pending
	ResolvedBy     int64     // 0 if not resolved by a user (e.g. expired)
}

const approvalCols = `id, user_id, conversation_id, tool_call_id, tool, capability, risk, args, status,
	created_at, expires_at, resolved_at, resolved_by, scope`

func scanApproval(r interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var conv sql.NullString
	var args string
	var c, e int64
	var resAt, resBy sql.NullInt64
	if err := r.Scan(&a.ID, &a.UserID, &conv, &a.ToolCallID, &a.Tool, &a.Capability, &a.Risk, &args, &a.Status,
		&c, &e, &resAt, &resBy, &a.Scope); err != nil {
		return nil, notFound(err)
	}
	a.ConversationID, a.Args = conv.String, json.RawMessage(args)
	a.CreatedAt, a.ExpiresAt, a.ResolvedAt, a.ResolvedBy = fromMS(c), fromMS(e), fromMS(resAt.Int64), resBy.Int64
	return &a, nil
}

// CreateApproval inserts a pending approval. If a.ID is empty one is
// generated. Status, CreatedAt are set by the store.
func (s *Store) CreateApproval(ctx context.Context, a Approval) (*Approval, error) {
	if a.ID == "" {
		a.ID = NewID("appr_")
	}
	if len(a.Args) == 0 {
		a.Args = json.RawMessage("{}")
	}
	if !json.Valid(a.Args) {
		return nil, fmt.Errorf("create approval: args are not valid JSON")
	}
	a.Status = ApprovalPending
	a.CreatedAt = fromMS(ms(s.now()))
	a.ExpiresAt = fromMS(ms(a.ExpiresAt))
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals(id, user_id, conversation_id, tool_call_id, tool, capability, risk, args, status, created_at, expires_at, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, nullStr(a.ConversationID), a.ToolCallID, a.Tool, a.Capability, a.Risk, string(a.Args),
		a.Status, ms(a.CreatedAt), ms(a.ExpiresAt), a.Scope)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create approval: %w", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create approval: %w", err)
	}
	return &a, nil
}

// GetApproval returns an approval by ID (no ownership check; callers that
// serve users must compare UserID).
func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get approval %s: %w", id, err)
	}
	return a, nil
}

// ListPendingApprovals returns a user's pending, unexpired approvals, oldest first.
func (s *Store) ListPendingApprovals(ctx context.Context, userID int64) ([]*Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalCols+` FROM approvals WHERE user_id = ? AND status = 'pending' AND expires_at > ? ORDER BY created_at, id`,
		userID, ms(s.now()))
	if err != nil {
		return nil, fmt.Errorf("list pending approvals: %w", err)
	}
	defer rows.Close()
	var out []*Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending approvals: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveApproval moves a pending approval to status (approved, denied, or
// canceled), recording resolvedBy (0 for system). It is atomic: only the first
// resolution wins; later calls, or calls after expiry, return ErrConflict.
func (s *Store) ResolveApproval(ctx context.Context, id, status string, resolvedBy int64) (*Approval, error) {
	switch status {
	case ApprovalApproved, ApprovalDenied, ApprovalCanceled:
	default:
		return nil, fmt.Errorf("resolve approval %s: invalid status %q", id, status)
	}
	now := ms(s.now())
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = ?, resolved_at = ?, resolved_by = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ?`,
		status, now, nullInt(resolvedBy), id, now)
	if err != nil {
		return nil, fmt.Errorf("resolve approval %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetApproval(ctx, id); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("resolve approval %s: %w", id, ErrConflict)
	}
	return s.GetApproval(ctx, id)
}

// ExpireApprovals marks pending approvals past their deadline as expired and
// returns them.
func (s *Store) ExpireApprovals(ctx context.Context) ([]*Approval, error) {
	now := ms(s.now())
	rows, err := s.db.QueryContext(ctx, `
		UPDATE approvals SET status = 'expired', resolved_at = ?
		WHERE status = 'pending' AND expires_at <= ?
		RETURNING `+approvalCols, now, now)
	if err != nil {
		return nil, fmt.Errorf("expire approvals: %w", err)
	}
	defer rows.Close()
	var out []*Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("expire approvals: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
