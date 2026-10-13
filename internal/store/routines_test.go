package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigration0002 checks that routine_runs rows written under schema 1 are
// carried over with their statuses mapped and their owner filled in.
func TestMigration0002(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	init, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`,
		string(init),
		`INSERT INTO schema_migrations VALUES (1, '0001_init.sql', 0)`,
		`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES (7, 'alice', 'x', 'member', 0, 0)`,
		`INSERT INTO routines(id, user_id, name, trigger_kind, trigger_spec, prompt, created_at, updated_at)
		 VALUES (3, 7, 'brief', 'cron', '@daily', 'p', 0, 0)`,
		`INSERT INTO routine_runs(id, routine_id, started_at, finished_at, status) VALUES
		 (1, 3, 10, 20, 'succeeded'), (2, 3, 30, 40, 'failed'), (3, 3, 50, 60, 'abandoned'), (4, 3, 70, NULL, 'waiting_approval')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if v, err := s.Migrate(ctx); err != nil || v < 2 {
		t.Fatalf("migrate = %d, %v", v, err)
	}
	runs, err := s.ListRoutineRuns(ctx, 7, 3, 0)
	if err != nil || len(runs) != 4 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	want := map[int64]string{1: "done", 2: "error", 3: "canceled", 4: "waiting_approval"}
	for _, r := range runs {
		if r.Status != want[r.ID] || r.UserID != 7 || r.Source != "schedule" {
			t.Errorf("run %d = %+v, want status %s", r.ID, r, want[r.ID])
		}
	}
	rt, err := s.GetRoutine(ctx, 7, 3)
	if err != nil || !rt.LastRunAt.IsZero() || len(rt.Deliver) != 0 {
		t.Errorf("routine = %+v, %v", rt, err)
	}
}
