-- 0002_routines: columns the routine scheduler needs.
--
-- routines gains last_run_at. routine_runs is rebuilt (SQLite cannot change a
-- CHECK constraint in place) to add the owning user (runs are listed per
-- user without a join), what started the run, and the final reply, and to
-- use the scheduler's status vocabulary. Existing rows are carried over with
-- their statuses mapped.

ALTER TABLE routines ADD COLUMN last_run_at INTEGER;

CREATE INDEX routines_due ON routines(enabled, trigger_kind, next_run_at);

CREATE TABLE routine_runs_new (
    id              INTEGER PRIMARY KEY,
    routine_id      INTEGER NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source          TEXT    NOT NULL DEFAULT 'schedule' CHECK (source IN ('schedule', 'event', 'manual')),
    event           TEXT    NOT NULL DEFAULT '',
    scheduled_for   INTEGER,
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    status          TEXT    NOT NULL CHECK (status IN ('running', 'waiting_approval', 'done', 'error', 'canceled')),
    conversation_id TEXT    REFERENCES conversations(id) ON DELETE SET NULL,
    error           TEXT    NOT NULL DEFAULT '',
    reply           TEXT    NOT NULL DEFAULT ''
);

INSERT INTO routine_runs_new (id, routine_id, user_id, scheduled_for, started_at, finished_at, status, conversation_id, error)
SELECT rr.id, rr.routine_id, r.user_id, rr.scheduled_for, rr.started_at, rr.finished_at,
       CASE rr.status
           WHEN 'succeeded' THEN 'done'
           WHEN 'failed' THEN 'error'
           WHEN 'abandoned' THEN 'canceled'
           ELSE rr.status
       END,
       rr.conversation_id, rr.error
FROM routine_runs rr JOIN routines r ON r.id = rr.routine_id;

DROP TABLE routine_runs;
ALTER TABLE routine_runs_new RENAME TO routine_runs;
CREATE INDEX routine_runs_routine ON routine_runs(routine_id, started_at DESC);
CREATE INDEX routine_runs_status ON routine_runs(status);
