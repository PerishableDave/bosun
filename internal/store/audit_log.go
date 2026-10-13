package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuditEntry is an audit_log row. The table is append-only.
type AuditEntry struct {
	ID            int64
	Time          time.Time
	UserID        int64 // 0 for system
	Kind          string
	Integration   string
	Tool          string
	Capability    string
	Risk          string
	Decision      string
	ApprovalID    string
	ApproverID    int64
	Args          json.RawMessage
	ResultSummary string
	Error         string
	Notes         []string
	Detail        json.RawMessage
}

const auditCols = `id, ts, user_id, kind, integration, tool, capability, risk, decision, approval_id, approver_id,
	args, result_summary, error, notes, detail`

// AppendAudit appends an entry and returns it with ID set. If e.Time is zero
// the current time is used.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) (*AuditEntry, error) {
	if e.Kind == "" {
		return nil, fmt.Errorf("append audit: kind is required")
	}
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	e.Time = fromMS(ms(e.Time))
	notes := e.Notes
	if notes == nil {
		notes = []string{}
	}
	nb, err := json.Marshal(notes)
	if err != nil {
		return nil, fmt.Errorf("append audit: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log(ts, user_id, kind, integration, tool, capability, risk, decision, approval_id, approver_id,
			args, result_summary, error, notes, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(e.Time), nullInt(e.UserID), e.Kind, e.Integration, e.Tool, e.Capability, e.Risk, e.Decision, e.ApprovalID,
		nullInt(e.ApproverID), rawOrNull(e.Args), e.ResultSummary, e.Error, string(nb), rawOrNull(e.Detail))
	if err != nil {
		return nil, fmt.Errorf("append audit: %w", err)
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return nil, fmt.Errorf("append audit: %w", err)
	}
	return &e, nil
}

func rawOrNull(b json.RawMessage) sql.NullString {
	if len(b) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// AuditFilter narrows ListAudit. Zero fields are ignored.
type AuditFilter struct {
	UserID  int64
	Kind    string
	Since   time.Time
	Until   time.Time
	AfterID int64 // for pagination: entries with ID > AfterID
	Limit   int   // default 100, max 1000
}

// ListAudit returns entries matching f, oldest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	var where []string
	var args []any
	if f.UserID != 0 {
		where, args = append(where, "user_id = ?"), append(args, f.UserID)
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, ms(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "ts < ?"), append(args, ms(f.Until))
	}
	if f.AfterID != 0 {
		where, args = append(where, "id > ?"), append(args, f.AfterID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT ` + auditCols + ` FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var uid, approver sql.NullInt64
		var a, detail sql.NullString
		var notes string
		if err := rows.Scan(&e.ID, &ts, &uid, &e.Kind, &e.Integration, &e.Tool, &e.Capability, &e.Risk, &e.Decision,
			&e.ApprovalID, &approver, &a, &e.ResultSummary, &e.Error, &notes, &detail); err != nil {
			return nil, fmt.Errorf("list audit: %w", err)
		}
		e.Time, e.UserID, e.ApproverID = fromMS(ts), uid.Int64, approver.Int64
		if a.Valid {
			e.Args = json.RawMessage(a.String)
		}
		if detail.Valid {
			e.Detail = json.RawMessage(detail.String)
		}
		if err := json.Unmarshal([]byte(notes), &e.Notes); err != nil {
			return nil, fmt.Errorf("list audit: decode notes: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
