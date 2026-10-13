package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PolicyRule is a policy_rules row: an explicit decision for one user and
// capability.
type PolicyRule struct {
	ID         int64
	UserID     int64
	Capability string
	Decision   string          // "allow" | "ask" | "deny"
	Condition  json.RawMessage // reserved; nil in v1
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const ruleCols = `id, user_id, capability, decision, condition, created_at, updated_at`

func scanRule(r interface{ Scan(...any) error }) (*PolicyRule, error) {
	var x PolicyRule
	var cond sql.NullString
	var c, u int64
	if err := r.Scan(&x.ID, &x.UserID, &x.Capability, &x.Decision, &cond, &c, &u); err != nil {
		return nil, notFound(err)
	}
	if cond.Valid {
		x.Condition = json.RawMessage(cond.String)
	}
	x.CreatedAt, x.UpdatedAt = fromMS(c), fromMS(u)
	return &x, nil
}

// UpsertPolicyRule creates or replaces the rule for (userID, capability).
func (s *Store) UpsertPolicyRule(ctx context.Context, userID int64, capability, decision string, condition json.RawMessage) (*PolicyRule, error) {
	now := ms(s.now())
	var cond sql.NullString
	if len(condition) > 0 {
		cond = sql.NullString{String: string(condition), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policy_rules(user_id, capability, decision, condition, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, capability) DO UPDATE SET
			decision = excluded.decision, condition = excluded.condition, updated_at = excluded.updated_at`,
		userID, capability, decision, cond, now, now)
	if err != nil {
		return nil, fmt.Errorf("upsert policy rule %s: %w", capability, err)
	}
	return s.GetPolicyRule(ctx, userID, capability)
}

// GetPolicyRule returns the rule for (userID, capability) or ErrNotFound.
func (s *Store) GetPolicyRule(ctx context.Context, userID int64, capability string) (*PolicyRule, error) {
	x, err := scanRule(s.db.QueryRowContext(ctx,
		`SELECT `+ruleCols+` FROM policy_rules WHERE user_id = ? AND capability = ?`, userID, capability))
	if err != nil {
		return nil, fmt.Errorf("get policy rule %s: %w", capability, err)
	}
	return x, nil
}

// ListPolicyRules returns all of a user's rules ordered by capability.
func (s *Store) ListPolicyRules(ctx context.Context, userID int64) ([]*PolicyRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleCols+` FROM policy_rules WHERE user_id = ? ORDER BY capability`, userID)
	if err != nil {
		return nil, fmt.Errorf("list policy rules: %w", err)
	}
	defer rows.Close()
	var out []*PolicyRule
	for rows.Next() {
		x, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("list policy rules: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeletePolicyRule removes the rule for (userID, capability).
func (s *Store) DeletePolicyRule(ctx context.Context, userID int64, capability string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM policy_rules WHERE user_id = ? AND capability = ?`, userID, capability)
	if err != nil {
		return fmt.Errorf("delete policy rule %s: %w", capability, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete policy rule %s: %w", capability, ErrNotFound)
	}
	return nil
}
