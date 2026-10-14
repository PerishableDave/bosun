-- 0004_invites: household user management.
--
-- invites holds one-time codes that let a new (or reset) member choose their
-- own password. Only the SHA-256 of a code is stored; a row is deleted when
-- it is redeemed, and expired rows are purged opportunistically.
--
-- users gains last_login_at (set on password login and invite redemption).
--
-- deleted_users records the IDs of removed users so that CreateUser never
-- hands a deleted user's ID to someone new: users.id is a plain INTEGER
-- PRIMARY KEY (SQLite would reuse the highest ID), and in-memory state
-- (event subscriptions, caches) and the audit log are keyed by user ID.

ALTER TABLE users ADD COLUMN last_login_at INTEGER;

CREATE TABLE invites (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  BLOB    NOT NULL UNIQUE,
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX invites_user ON invites(user_id);

CREATE TABLE deleted_users (
    id         INTEGER PRIMARY KEY,
    deleted_at INTEGER NOT NULL
);
