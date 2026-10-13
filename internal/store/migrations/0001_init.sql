-- 0001_init: initial schema.
-- Timestamps are INTEGER unix milliseconds (UTC). Public-facing IDs
-- (conversations, approvals) are random TEXT; internal IDs are INTEGER.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'member')),
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   BLOB    NOT NULL UNIQUE,
    label        TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

-- Encrypted integration secrets, per user (user_id NULL = shared/admin-level).
CREATE TABLE credentials (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
    integration TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    nonce       BLOB    NOT NULL,
    ciphertext  BLOB    NOT NULL,
    expires_at  INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX credentials_unique ON credentials(ifnull(user_id, 0), integration, kind);

CREATE TABLE policy_rules (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    capability TEXT    NOT NULL,
    decision   TEXT    NOT NULL CHECK (decision IN ('allow', 'ask', 'deny')),
    condition  TEXT,   -- JSON, reserved for argument conditions (unused in v1)
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, capability)
);

CREATE TABLE conversations (
    id         TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      TEXT    NOT NULL DEFAULT '',
    routine_id INTEGER REFERENCES routines(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX conversations_user ON conversations(user_id, updated_at DESC);

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    conversation_id TEXT    NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    content         TEXT    NOT NULL, -- JSON array of content blocks
    created_at      INTEGER NOT NULL
);
CREATE INDEX messages_conversation ON messages(conversation_id, id);

CREATE TABLE memories (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('fact', 'preference', 'summary', 'note')),
    content    TEXT    NOT NULL,
    source     TEXT    NOT NULL DEFAULT '',
    embedding  BLOB,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX memories_user ON memories(user_id, kind);

CREATE VIRTUAL TABLE memories_fts USING fts5(content, content='memories', content_rowid='id');
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TABLE routines (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    trigger_kind TEXT    NOT NULL CHECK (trigger_kind IN ('cron', 'event')),
    trigger_spec TEXT    NOT NULL,
    prompt       TEXT    NOT NULL,
    model        TEXT    NOT NULL DEFAULT '',
    deliver      TEXT    NOT NULL DEFAULT '[]', -- JSON array
    enabled      INTEGER NOT NULL DEFAULT 1,
    next_run_at  INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (user_id, name)
);

CREATE TABLE routine_runs (
    id              INTEGER PRIMARY KEY,
    routine_id      INTEGER NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
    scheduled_for   INTEGER,
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    status          TEXT    NOT NULL CHECK (status IN ('running', 'waiting_approval', 'succeeded', 'failed', 'abandoned')),
    conversation_id TEXT    REFERENCES conversations(id) ON DELETE SET NULL,
    error           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX routine_runs_routine ON routine_runs(routine_id, started_at DESC);

CREATE TABLE approvals (
    id              TEXT    PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id TEXT    REFERENCES conversations(id) ON DELETE SET NULL,
    tool_call_id    TEXT    NOT NULL DEFAULT '',
    tool            TEXT    NOT NULL,
    capability      TEXT    NOT NULL,
    risk            TEXT    NOT NULL CHECK (risk IN ('read', 'write', 'destructive')),
    args            TEXT    NOT NULL DEFAULT '{}', -- JSON
    status          TEXT    NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'expired', 'canceled')),
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    resolved_at     INTEGER,
    resolved_by     INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX approvals_user_status ON approvals(user_id, status);

-- Append-only: UPDATE and DELETE are rejected by triggers.
CREATE TABLE audit_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             INTEGER NOT NULL,
    user_id        INTEGER,          -- no FK: entries outlive users
    kind           TEXT    NOT NULL,
    integration    TEXT    NOT NULL DEFAULT '',
    tool           TEXT    NOT NULL DEFAULT '',
    capability     TEXT    NOT NULL DEFAULT '',
    risk           TEXT    NOT NULL DEFAULT '',
    decision       TEXT    NOT NULL DEFAULT '',
    approval_id    TEXT    NOT NULL DEFAULT '',
    approver_id    INTEGER,
    args           TEXT,             -- JSON
    result_summary TEXT    NOT NULL DEFAULT '',
    error          TEXT    NOT NULL DEFAULT '',
    notes          TEXT    NOT NULL DEFAULT '[]', -- JSON array of strings
    detail         TEXT              -- JSON, kind-specific (e.g. provider/model/tokens)
);
CREATE INDEX audit_log_user_ts ON audit_log(user_id, ts);
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TABLE hooks (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    point      TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('script')),
    command    TEXT    NOT NULL, -- JSON array argv
    enabled    INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
