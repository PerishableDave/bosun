package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func mustUser(t *testing.T, s *Store, name string) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "hash", "member")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func TestMigrateIdempotentAndTables(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	v1, err := s.SchemaVersion(ctx)
	if err != nil || v1 < 1 {
		t.Fatalf("SchemaVersion = %d, %v", v1, err)
	}
	v2, err := s.Migrate(ctx)
	if err != nil || v2 != v1 {
		t.Fatalf("second Migrate = %d, %v; want %d", v2, err, v1)
	}
	for _, tbl := range []string{"users", "sessions", "credentials", "policy_rules", "conversations", "messages",
		"memories", "memories_fts", "routines", "routine_runs", "approvals", "audit_log", "hooks"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = ?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d, err=%v)", tbl, n, err)
		}
	}
	var mode string
	s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, "Dave", "h1", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "dave", "h2", "member"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate (case-insensitive) username: err = %v, want ErrConflict", err)
	}
	got, err := s.GetUserByUsername(ctx, "DAVE")
	if err != nil || got.ID != u.ID || got.Role != "admin" {
		t.Fatalf("GetUserByUsername = %+v, %v", got, err)
	}
	if _, err := s.GetUser(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUser missing: %v", err)
	}
	if _, err := s.CreateUser(ctx, "x", "h", "root"); err == nil {
		t.Error("invalid role accepted")
	}
	if err := s.UpdateUserPassword(ctx, u.ID, "h3"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUser(ctx, u.ID)
	if got.PasswordHash != "h3" {
		t.Errorf("password not updated")
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Errorf("CountUsers = %d", n)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	sess, err := s.CreateSession(ctx, u.ID, []byte("hash1"), "cli", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, u.ID, []byte("hash1"), "", now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate token hash: %v", err)
	}
	got, err := s.GetSessionByTokenHash(ctx, []byte("hash1"))
	if err != nil || got.ID != sess.ID || got.UserID != u.ID {
		t.Fatalf("GetSessionByTokenHash = %+v, %v", got, err)
	}
	s.CreateSession(ctx, u.ID, []byte("old"), "", now.Add(-time.Minute))
	if n, err := s.DeleteExpiredSessions(ctx); err != nil || n != 1 {
		t.Errorf("DeleteExpiredSessions = %d, %v", n, err)
	}
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionByTokenHash(ctx, []byte("hash1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestConversationsAndMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := mustUser(t, s, "a"), mustUser(t, s, "b")
	c, err := s.CreateConversation(ctx, a.ID, "hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(ctx, b.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user's conversation visible: %v", err)
	}
	if _, err := s.AppendMessage(ctx, c.ID, "user", json.RawMessage(`[{"type":"text","text":"hi"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, c.ID, "assistant", json.RawMessage(`[{"type":"text","text":"yo"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, c.ID, "system", json.RawMessage(`[]`)); err == nil {
		t.Error("invalid role accepted")
	}
	if _, err := s.AppendMessage(ctx, c.ID, "user", json.RawMessage(`{bad`)); err == nil {
		t.Error("invalid JSON accepted")
	}
	msgs, err := s.ListMessages(ctx, c.ID)
	if err != nil || len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("ListMessages = %+v, %v", msgs, err)
	}
	list, err := s.ListConversations(ctx, a.ID, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListConversations = %v, %v", list, err)
	}
	if list, _ := s.ListConversations(ctx, b.ID, 10); len(list) != 0 {
		t.Errorf("user b sees %d conversations", len(list))
	}
	if err := s.DeleteConversation(ctx, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.ListMessages(ctx, c.ID); len(msgs) != 0 {
		t.Errorf("messages not cascaded")
	}
}

func TestPolicyRules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	if _, err := s.UpsertPolicyRule(ctx, u.ID, "email.send", "ask", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.UpsertPolicyRule(ctx, u.ID, "email.send", "deny", nil)
	if err != nil || r.Decision != "deny" {
		t.Fatalf("upsert = %+v, %v", r, err)
	}
	if _, err := s.UpsertPolicyRule(ctx, u.ID, "x.y", "maybe", nil); err == nil {
		t.Error("invalid decision accepted")
	}
	s.UpsertPolicyRule(ctx, u.ID, "calendar.read", "allow", nil)
	rules, err := s.ListPolicyRules(ctx, u.ID)
	if err != nil || len(rules) != 2 || rules[0].Capability != "calendar.read" {
		t.Fatalf("ListPolicyRules = %+v, %v", rules, err)
	}
	if err := s.DeletePolicyRule(ctx, u.ID, "email.send"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPolicyRule(ctx, u.ID, "email.send"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestApprovals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	a, err := s.CreateApproval(ctx, Approval{UserID: u.ID, Tool: "email.send", Capability: "email.send",
		Risk: "write", Args: json.RawMessage(`{"to":"x"}`), ExpiresAt: now.Add(15 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPendingApprovals(ctx, u.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != a.ID {
		t.Fatalf("ListPendingApprovals = %+v, %v", pending, err)
	}
	r, err := s.ResolveApproval(ctx, a.ID, ApprovalApproved, u.ID)
	if err != nil || r.Status != ApprovalApproved || r.ResolvedBy != u.ID || r.ResolvedAt.IsZero() {
		t.Fatalf("ResolveApproval = %+v, %v", r, err)
	}
	if _, err := s.ResolveApproval(ctx, a.ID, ApprovalDenied, u.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("second resolve: %v, want ErrConflict (first answer wins)", err)
	}
	if _, err := s.ResolveApproval(ctx, "appr_missing", ApprovalDenied, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}

	b, _ := s.CreateApproval(ctx, Approval{UserID: u.ID, Tool: "t", Capability: "c", Risk: "destructive",
		ExpiresAt: now.Add(time.Minute)})
	s.SetClock(func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := s.ResolveApproval(ctx, b.ID, ApprovalApproved, u.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("resolve after deadline: %v, want ErrConflict", err)
	}
	expired, err := s.ExpireApprovals(ctx)
	if err != nil || len(expired) != 1 || expired[0].ID != b.ID || expired[0].Status != ApprovalExpired {
		t.Fatalf("ExpireApprovals = %+v, %v", expired, err)
	}
}

func TestAuditAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	e, err := s.AppendAudit(ctx, AuditEntry{UserID: u.ID, Kind: "tool_call", Tool: "email.send",
		Capability: "email.send", Decision: "ask", Args: json.RawMessage(`{"a":1}`), Notes: []string{"hook: ok"}})
	if err != nil {
		t.Fatal(err)
	}
	s.AppendAudit(ctx, AuditEntry{Kind: "llm_request", Detail: json.RawMessage(`{"model":"m"}`)})
	all, err := s.ListAudit(ctx, AuditFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAudit = %v, %v", all, err)
	}
	mine, _ := s.ListAudit(ctx, AuditFilter{UserID: u.ID})
	if len(mine) != 1 || mine[0].ID != e.ID || mine[0].Notes[0] != "hook: ok" || string(mine[0].Args) != `{"a":1}` {
		t.Fatalf("filtered = %+v", mine)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE audit_log SET tool = 'x'`); err == nil {
		t.Error("audit_log UPDATE allowed")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log`); err == nil {
		t.Error("audit_log DELETE allowed")
	}
}

func TestMemoriesFTS(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memories(user_id, kind, content, created_at, updated_at) VALUES (?, 'fact', 'likes espresso', 1, 1)`, u.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'espresso'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts match = %d, %v", n, err)
	}
}
