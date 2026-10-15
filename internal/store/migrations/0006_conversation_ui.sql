-- 0006_conversation_ui: conversation list state for the web UI.
--
-- pinned / archived / unread are per-conversation flags the owner sets
-- (unread is also set when a routine reply lands). title_source records who
-- chose the title: 'auto' (Bosun may retitle it) or 'user' (never touched
-- again). source is where the conversation came from: 'chat' (the API),
-- 'routine', or 'telegram'. last_message_at is the time of the newest
-- message (NULL while there is none) and orders the list.

ALTER TABLE conversations ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversations ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversations ADD COLUMN title_source TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE conversations ADD COLUMN unread INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversations ADD COLUMN source TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE conversations ADD COLUMN last_message_at INTEGER;

UPDATE conversations SET source = 'routine' WHERE routine_id IS NOT NULL OR title LIKE 'routine: %';
UPDATE conversations SET source = 'telegram' WHERE source = 'chat' AND title = 'telegram';
-- Titles given at creation by a client are the user's choice.
UPDATE conversations SET title_source = 'user' WHERE source = 'chat' AND title <> '';
UPDATE conversations SET last_message_at =
    (SELECT max(created_at) FROM messages WHERE messages.conversation_id = conversations.id);

CREATE INDEX conversations_activity ON conversations(
    user_id, archived, pinned DESC, COALESCE(last_message_at, created_at) DESC, id DESC);
