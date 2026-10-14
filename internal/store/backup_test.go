package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupTo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(ctx, filepath.Join(dir, "bosun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, err := st.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := LatestSchemaVersion(); err != nil || latest != v {
		t.Fatalf("latest = %d, %v; migrated to %d", latest, err, v)
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO users (username, password_hash, role, created_at, updated_at) VALUES ('snap', 'x', 'admin', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "snap.db")
	if err := st.BackupTo(ctx, out); err != nil {
		t.Fatal(err)
	}
	if err := st.BackupTo(ctx, out); err == nil {
		t.Fatal("overwrote an existing backup")
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot: %v %v", fi, err)
	}
	snap, err := Open(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if sv, err := snap.SchemaVersion(ctx); err != nil || sv != v {
		t.Fatalf("snapshot schema = %d, %v", sv, err)
	}
	var n int
	if err := snap.DB().QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username = 'snap'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot rows = %d, %v", n, err)
	}
	total, enabled, err := st.CountRoutines(ctx)
	if err != nil || total != 0 || enabled != 0 {
		t.Fatalf("routines = %d %d %v", total, enabled, err)
	}
}
