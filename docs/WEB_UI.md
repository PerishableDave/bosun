# Web UI: product spec

The web UI is how most household members will use Bosun day to day. It must
feel like a modern chat product, not an admin panel, while keeping Bosun's
distinctive parts (approvals, tool visibility, audit) visible and quick.

Target: a competent chat client on desktop and phone, with zero build step,
plain HTML/CSS/JS, no CDN, light and dark.

## Principles

1. **The conversation list is a first-class object.** Users create many
   conversations; they must be able to find, name, pin, archive and delete
   them without thinking.
2. **Streaming must feel alive.** A reply shows up word by word, tool calls
   appear as they happen, approvals interrupt inline, and the user can stop
   a turn at any time.
3. **Keyboard first, touch friendly.** Every common action has a shortcut;
   every target is finger-sized on a phone.
4. **Never lose the user's text.** Drafts survive navigation and reloads.
5. **Show what Bosun did, compactly.** Tool calls and approvals are visible
   but folded by default; details one click away.

## Conversations

| Feature | Behavior |
|---|---|
| Auto-title | A new conversation is titled from the first user message (truncated to ~40 chars) immediately; after the first reply, a cheap model call (`memory.extract_model`) produces a ≤6-word title asynchronously and the sidebar updates live. A title the user set by hand is never overwritten. |
| Rename | Pencil icon on hover, or double-click the title in the header; inline edit; Enter saves, Esc cancels. |
| Delete | Trash icon → confirm dialog naming the conversation → hard delete (messages too; pending approvals in it are canceled). Toast "Deleted". |
| Pin | Pinned conversations sit in a "Pinned" group at the top. |
| Archive | Removes from the main list; "Archived" filter shows them; unarchive restores. |
| Grouping | Pinned, Today, Yesterday, Previous 7 days, Older. |
| Search | Search box filters by title and message text (server-side `q`); Cmd/Ctrl+K focuses it. |
| Source badges | Conversations created by routines or Telegram carry a small badge (`routine`, `telegram`). |
| Activity dot | A conversation that received a reply while not open (routine finished) shows an unread dot until opened. |
| New chat | Button and Cmd/Ctrl+Shift+O. Opening the app with no conversations shows an empty state with three example prompts. |
| Pagination | The list loads 50 at a time with "Load more". |
| Export | Conversation menu → "Export as Markdown" downloads a `.md`. |

## Chat view

- **Composer:** auto-growing textarea, Enter sends, Shift+Enter newline;
  disabled while a turn runs, with a **Stop** button that cancels the turn
  (`POST /v1/conversations/{id}/stop`). Draft text is kept per conversation
  in `localStorage` and restored.
- **Messages:** user right-aligned, assistant left; timestamps on hover;
  **Copy** button on assistant messages; a streaming caret while text
  arrives; auto-scroll that stops when the user scrolls up, with a
  "↓ Jump to latest" pill.
- **Markdown:** headings, bold/italic, inline code, fenced code with a copy
  button and language label, ordered/unordered lists, links (open in a new
  tab, `rel="noopener"`), tables, blockquotes, horizontal rules. Escape
  everything first; never inject raw model HTML.
- **Tool calls:** a compact folded row per call (`⚙ email.list · 0.8s`),
  expandable to show arguments and the result summary; errors in red.
  Consecutive tool calls fold into one group ("3 tool calls").
- **Approvals:** inline card with Approve / Deny, the "always allow on
  <site>" option when offered, the screenshot preview, and the resolved
  state afterwards (who/when). Pending approvals from other conversations
  still show in the Approvals tab and the badge.
- **Errors:** turn errors appear as a message with a **Retry** that resends
  the last user message.
- **Routine conversations** show a header note "Run by routine <name>".

## App shell

- Sidebar: collapsible on desktop (Cmd/Ctrl+B), a drawer on phones; the
  current conversation title in the header; the user's name/role and a menu
  (Settings, Log out).
- Tabs stay: Chat, Approvals (badge), Tools, Memory, Accounts, Browser,
  Users (admin), plus **Settings** (change password, theme auto/light/dark,
  keyboard shortcut list).
- Toasts for errors and confirmations; a global "disconnected, retrying"
  banner when the event stream drops.
- Accessibility: visible focus rings, `aria-label` on icon buttons, live
  region for streaming text, respects `prefers-reduced-motion`.
- Performance: history renders incrementally for long conversations; the
  event stream reconnects with backoff.

## API additions (contract)

- `GET /v1/conversations?archived=false|true|all&q=&limit=50&before=<cursor>`
  → items with `id, title, title_source (auto|user), pinned, archived,
  source (chat|routine|telegram), routine_name?, message_count,
  last_message_at, last_message_preview (≤120 chars), unread, created_at,
  updated_at`, plus `next_cursor`.
- `PATCH /v1/conversations/{id}` `{title?, pinned?, archived?}`; setting a
  title marks `title_source=user`.
- `DELETE /v1/conversations/{id}` → 204; cascades messages; cancels pending
  approvals of that conversation.
- `POST /v1/conversations/{id}/stop` → 202; cancels the running turn (the
  stream ends with `error: canceled` or a `done` with `stopped: true`).
- `POST /v1/conversations/{id}/read` → 204; clears `unread`.
- `GET /v1/conversations/{id}/export?format=markdown` → `text/markdown`
  attachment.
- SSE event `conversation_updated {id, title, title_source, updated_at,
  unread}` on the user's stream whenever a title, pin, archive or unread
  state changes, so every open tab stays in sync.
- Migration adds `pinned`, `archived`, `title_source`, `unread` to
  `conversations`.

## Out of scope for this pass

Message editing and branching, per-message regeneration, file attachments,
voice, a mobile app. Each is a candidate for the next pass.
