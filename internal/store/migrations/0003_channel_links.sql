-- 0003_channel_links: bindings between external chat identities (a
-- Telegram chat, later Slack/Discord/...) and Bosun users.
--
-- A link is created when a user redeems a one-time code from inside the chat
-- app. conversation_id is the long-lived conversation that chat talks to
-- (created lazily, replaced by /new). Deleting the user deletes the link.

CREATE TABLE channel_links (
    id              INTEGER PRIMARY KEY,
    channel         TEXT    NOT NULL,
    external_id     TEXT    NOT NULL,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name    TEXT    NOT NULL DEFAULT '',
    conversation_id TEXT    REFERENCES conversations(id) ON DELETE SET NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (channel, external_id)
);

CREATE INDEX channel_links_user ON channel_links(user_id, channel);
