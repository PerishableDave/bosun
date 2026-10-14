package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// BackupTo writes a consistent snapshot of the database to path with
// VACUUM INTO (safe while bosund is running: it reads one transaction's view
// and never copies a half-written WAL). path must not exist. The snapshot
// is a self-contained database in rollback-journal mode.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup %s: already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup %s: %w", path, err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

// LatestSchemaVersion returns the highest migration version embedded in
// this binary: a database (or backup) with a higher version was written by
// a newer bosun.
func LatestSchemaVersion() (int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, nil
	}
	return ms[len(ms)-1].version, nil
}

// CountRoutines returns the number of routines and how many are enabled,
// across all users (for bosun doctor).
func (s *Store) CountRoutines(ctx context.Context) (total, enabled int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(enabled != 0), 0) FROM routines`).Scan(&total, &enabled)
	if err != nil {
		return 0, 0, fmt.Errorf("count routines: %w", err)
	}
	return total, enabled, nil
}
