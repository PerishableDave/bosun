# Integration roadmap

What Bosun needs to feel like a personal agent (Muse, Instinct, OpenClaw),
and the order we build it in. Research as of October 2026.

## What the comparable products ship

| Product | How you reach it | Integrations that matter |
|---|---|---|
| Meta Muse | web, iOS/Android, WhatsApp | Google Workspace (Gmail, Calendar, Docs, Drive), Outlook, Notion, Dropbox, Spotify, Plaid, 40+ native connectors, Zapier for the rest; per-connector read-only vs. full permissions |
| Instinct | **text it**: iMessage, WhatsApp, phone; group chats | Gmail, Calendar, Drive, Docs, Sheets, Tasks; 1Password for logins; Stripe for payments; a dedicated email address for the agent; location |
| OpenClaw | CLI, chat apps | email, calendar, GitHub, web search/scrape, Google Workspace, browser automation, voice |

Three things recur in every one of them: **email**, **calendar**, and a
**messaging channel** you can text from your phone. Google account sign-in is
what makes the first two work for most people. The usual security advice for
these (read-only scopes first, confirm before sending) is Bosun's default
policy model.

Sources: [Zapier on Muse](https://zapier.com/blog/muse-ai/),
[TechCrunch on Muse](https://techcrunch.com/2026/09/08/meta-debuts-its-muse-ai-agent-will-consumers-trust-it/),
[TechCrunch on Instinct group chats](https://techcrunch.com/2026/10/05/instinct-brings-its-ai-agent-to-group-chats-even-for-friends-without-an-account/),
[Sacra on Instinct](https://sacra.com/c/instinct/),
[Composio: top OpenClaw skills](https://composio.dev/content/top-openclaw-skills),
[mcpmanager: most popular MCP servers](https://mcpmanager.ai/blog/most-popular-mcp-servers/),
[strac: MCP servers and safety](https://www.strac.io/blog/best-mcp-servers).

## Bosun's angle

Open protocols first, so the privacy-minded setups work without a Google
account: IMAP/SMTP for mail, CalDAV for calendars, CardDAV for contacts. Google
(and later Microsoft) OAuth layered on top for people who live in Workspace.
Every integration is per-user, credentials live in the vault, and anything
containing other people's text (mail bodies, event descriptions) is marked
untrusted.

## Wave 1 (in progress)

| Integration | Protocol | Tools | Notes |
|---|---|---|---|
| `oauth` | OAuth2 + PKCE, Google and Microsoft | `bosun connect google` | Vault resolver: any integration can ask for kind `oauth2:google` and get a fresh token |
| `email` | IMAP4rev1 + SMTP, XOAUTH2 for Gmail | list, search, read, draft, send (destructive), archive, mark, folders | Poller emits `email.received` events for routines |
| `calendar` | CalDAV + iCalendar, Google CalDAV with OAuth | calendars, events, get, create, update, delete, freebusy | RRULE expansion for the common cases |
| `channels/telegram` | Bot API, long polling | chat, inline Approve/Deny, routine deliveries | First channel; the `channels` framework is designed for Slack/Discord/iMessage next |

## Wave 2 (planned)

- **Tasks and reminders:** Google Tasks via OAuth; a local `tasks` integration
  for people without Google; "remind me at…" as sugar over routines.
- **Contacts:** CardDAV (iCloud, Google, Fastmail, Nextcloud) so "email Ana"
  resolves without the user pasting an address.
- **Microsoft 365:** Outlook mail and calendar via Graph (OAuth is already
  provider-agnostic).
- **More channels:** Slack, Discord, iMessage (macOS, via the local Messages
  database, read-only unless explicitly enabled), WhatsApp via the Cloud API.
- **Files:** Google Drive read-only and a local `files` integration scoped to
  explicitly allowed folders.
- **Location and weather:** small read-only tools that make routine briefings
  useful ("leave by 8:10, it's raining").
- **GitHub, Notion, Linear, Todoist:** via the MCP bridge; no first-party code
  unless the MCP servers prove inadequate.

## Deliberately not planned

- Browser automation as a general fallback (OpenClaw-style). It is the main
  way these agents end up with more access than the user intended.
- Payments. Bosun can draft, book, and prepare; a human pays.
- A dedicated agent email address that can create accounts on the user's
  behalf. Useful, but it is an account-creation capability and belongs behind
  a very explicit opt-in if it ever ships.
