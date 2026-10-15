-- 0005_policy_scopes: site-scoped policy rules and approval scopes.
--
-- policy_rules gains a scope column: '' is the capability-wide rule (the
-- only kind before this migration); a non-empty scope (a normalized site
-- such as 'opentable.com') limits the rule to tool calls whose scope
-- argument matches. A rule is unique per (user_id, capability, scope).
-- SQLite cannot change a UNIQUE constraint in place, so the table is
-- rebuilt; no other table references policy_rules.
--
-- approvals gains the scope of the call being approved, so clients can
-- offer "approve and always allow on <scope>".

CREATE TABLE policy_rules_new (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    capability TEXT    NOT NULL,
    scope      TEXT    NOT NULL DEFAULT '',
    decision   TEXT    NOT NULL CHECK (decision IN ('allow', 'ask', 'deny')),
    condition  TEXT,   -- JSON, reserved for argument conditions (unused in v1)
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, capability, scope)
);
INSERT INTO policy_rules_new(id, user_id, capability, scope, decision, condition, created_at, updated_at)
    SELECT id, user_id, capability, '', decision, condition, created_at, updated_at FROM policy_rules;
DROP TABLE policy_rules;
ALTER TABLE policy_rules_new RENAME TO policy_rules;

ALTER TABLE approvals ADD COLUMN scope TEXT NOT NULL DEFAULT '';
