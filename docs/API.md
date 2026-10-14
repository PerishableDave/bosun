# Bosun HTTP API (v1)

`bosund` serves a JSON + Server-Sent Events API, by default on
`http://127.0.0.1:7777`. LAN binding requires TLS (`bosun setup --lan`
generates a self-signed certificate; `bosund` logs its SHA-256 fingerprint at
startup so clients can pin it). The `bosun` CLI and the web UI are clients of
this API.

All endpoints below are implemented. An endpoint whose backing component is
not configured on a given server (only possible when embedding `api.Server`
without it, never in `bosund`) answers `501 not_implemented`.

## Conventions

- **Auth.** Send `Authorization: Bearer <token>`. Tokens come from
  `POST /v1/login`, start with `bsn_`, and are opaque. Only their SHA-256 hash
  is stored server-side. Missing/invalid/expired tokens get `401` with a
  `WWW-Authenticate: Bearer` header. Tokens are never accepted in URLs.
- **Bodies.** Requests are a single JSON object (`Content-Type:
  application/json`), max 1 MiB. Unknown fields are rejected with `400`.
- **Timestamps** are RFC 3339 strings in UTC.
- **IDs.** Users and memories are integers. Conversations (`conv_…`) and approvals
  (`appr_…`) are opaque strings.
- **Isolation.** Every resource is scoped to the authenticated user. A
  resource owned by another user is reported as `404`, with one exception:
  answering another user's approval (`POST /v1/approvals/{id}`) is `403`.
  Approval IDs are 128-bit random values, so this reveals nothing guessable.

### Errors

All errors use one envelope:

```json
{ "error": { "code": "unauthorized", "message": "invalid or expired token" } }
```

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `bad_request` | Malformed JSON, unknown field, or invalid value |
| 401 | `unauthorized` | Missing/invalid/expired bearer token |
| 401 | `invalid_credentials` | Login failed (username or password wrong; deliberately indistinguishable) |
| 403 | `forbidden` | Answering an approval that belongs to another user, a non-admin calling an admin-only endpoint, or a member managing a shared credential |
| 404 | `not_found` | No such endpoint or resource (or not yours) |
| 409 | `conflict` | State precondition failed, e.g. approval already resolved or expired, or a turn already running |
| 404 | `invalid_invite` | Invite code unknown, expired or already used (deliberately indistinguishable) |
| 409 | `last_admin` | The change would leave no enabled admin |
| 429 | `rate_limited` | Too many attempts; see `Retry-After` |
| 500 | `internal` | Server error (details are logged, never returned) |
| 501 | `not_implemented` | The endpoint's component is not configured on this server |
| 503 | `unavailable` | The daemon is shutting down (e.g. starting a routine run during shutdown) |

### User object

```json
{ "id": 1, "username": "dave", "role": "admin", "created_at": "2026-10-12T22:00:00Z" }
```

`role` is `"admin"` or `"member"`.

---

## Endpoints

### `GET /v1/health` (implemented, no auth)

```json
200 { "status": "ok", "version": "dev" }
```

### `POST /v1/login` (implemented, no auth)

Request:

```json
{ "username": "dave", "password": "…", "label": "cli" }
```

`label` is optional (≤ 100 chars) and describes the client session.

Response `200`:

```json
{
  "token": "bsn_…",
  "expires_at": "2026-11-11T22:00:00Z",
  "user": { "id": 1, "username": "dave", "role": "admin", "created_at": "…" }
}
```

Errors: `400 bad_request` (missing fields), `401 invalid_credentials`.

### `POST /v1/logout` (implemented)

Revokes the bearer token used for the request. Response `204 No Content`.

### `GET /v1/me` (implemented)

Response `200`: the [user object](#user-object).

### `GET /v1/events` (implemented, SSE)

A long-lived per-user event stream. Every connected client of a user receives
that user's events (so an approval can be answered from any client; the first
answer wins). Response headers: `Content-Type: text/event-stream`,
`Cache-Control: no-cache`.

The stream starts with a `: connected` comment and sends `: ping` comments
every 15 s as keep-alives. Events not delivered (client disconnected, or a
client too slow to keep up) are dropped; clients resynchronise with the REST
endpoints (e.g. `GET /v1/approvals`) after reconnecting.

Every event of every turn the user runs is published to this stream (each
payload carries its `conversation_id`), so it is where a second client sees
`approval_required` / `approval_resolved` for a turn started elsewhere. See
[SSE events](#sse-events).

### `POST /v1/conversations`

Request (all fields optional; `title` is trimmed and cut to 200 bytes):

```json
{ "title": "Morning planning" }
```

Response `201`:

```json
{ "id": "conv_3f2a…", "title": "Morning planning", "created_at": "2026-10-12T22:00:00Z", "updated_at": "2026-10-12T22:00:00Z" }
```

### `GET /v1/conversations`

The caller's conversations, most recently updated first.

Response `200`:

```json
{ "conversations": [ { "id": "conv_3f2a…", "title": "Morning planning", "created_at": "…", "updated_at": "…" } ] }
```

### `GET /v1/conversations/{id}`

One conversation with its full stored history. `content` is the array of
content blocks exactly as stored (`text`, `tool_use`, `tool_result`);
`role` is `user`, `assistant`, or `tool`.

Response `200`:

```json
{
  "id": "conv_3f2a…", "title": "", "created_at": "…", "updated_at": "…",
  "messages": [
    { "role": "user", "content": [ { "type": "text", "text": "What's on example.com?" } ], "created_at": "…" },
    { "role": "assistant", "content": [ { "type": "tool_use", "id": "toolu_01", "name": "websearch.fetch", "input": { "url": "https://example.com" } } ], "created_at": "…" },
    { "role": "tool", "content": [ { "type": "tool_result", "tool_use_id": "toolu_01", "content": "<untrusted-data …" } ], "created_at": "…" },
    { "role": "assistant", "content": [ { "type": "text", "text": "It is a placeholder page." } ], "created_at": "…" }
  ]
}
```

Errors: `404` (no such conversation, or not yours).

### `POST /v1/conversations/{id}/messages` (SSE response)

Sends a user message and streams the agent's turn.

Request:

```json
{ "content": "Summarize today's calendar" }
```

`content` is required and must not be blank. Response `200` with
`Content-Type: text/event-stream`; the body is the sequence of
[SSE events](#sse-events) for this conversation only: any number of `text`,
`tool_call`, `tool_result`, `approval_required`, `approval_resolved`, ending
with exactly one `done` or `error`, after which the server closes the stream.
`: ping` comments are sent as keep-alives while the turn waits (e.g. on an
approval).

The turn is: store the message, call the model (default model from
`llm.default`) with the conversation history and the registered tools, run
each tool call through policy/approval/hooks, feed the results back, and
repeat until the model ends its turn or 20 model calls have been made (then
the turn ends with `error`). Every message, including tool calls and
results, is stored in the conversation.

Errors before streaming starts use normal JSON errors: `400`, `401`,
`404` (conversation not found or not yours), `409` (a turn is already running
in this conversation).

If the client disconnects mid-turn the turn is canceled; any pending approval
for it is marked `canceled`.

### `GET /v1/approvals`

Lists the caller's pending, unexpired approvals, oldest first.

Response `200`:

```json
{
  "approvals": [
    {
      "id": "appr_9c1e…",
      "user_id": 1,
      "conversation_id": "conv_3f2a…",
      "tool_call_id": "toolu_01",
      "tool": "email.send",
      "capability": "email.send",
      "risk": "write",
      "args": { "to": "alice@example.com", "subject": "Re: lunch" },
      "status": "pending",
      "created_at": "2026-10-12T22:00:00Z",
      "expires_at": "2026-10-12T22:15:00Z"
    }
  ]
}
```

`risk` is `read` | `write` | `destructive`. `status` is `pending` |
`approved` | `denied` | `expired` | `canceled`. `conversation_id` and
`tool_call_id` are omitted when empty.

### `POST /v1/approvals/{id}`

Request:

```json
{ "decision": "approve" }
```

`decision` is `"approve"` or `"deny"` (anything else → `400`).

Response `200`: the updated approval object (as above, with `status`,
`resolved_at`, and `resolved_by` set). An `approval_resolved` event is
published to all of the user's clients, and the waiting tool call proceeds
(approve) or is reported to the model as denied (deny).

Errors: `403 forbidden` (the approval belongs to another user), `404` (no
such approval), `409 conflict` (already resolved by another client, expired,
or canceled).

### `GET /v1/policy`

The caller's effective permission configuration: the per-risk defaults (from
`policy.defaults` in `bosun.yaml`) and the caller's explicit rules, ordered by
capability. An explicit rule beats the risk default; capabilities no
registered tool declares are always denied.

Response `200`:

```json
{
  "defaults": { "read": "allow", "write": "ask", "destructive": "ask" },
  "rules": [ { "capability": "websearch.fetch", "decision": "ask", "updated_at": "2026-10-12T22:00:00Z" } ]
}
```

### `PUT /v1/policy/{capability}`

Creates or replaces the caller's rule for one capability. Takes effect on the
next tool call. Recorded in the audit log (`kind = config`).

Request:

```json
{ "decision": "allow" }
```

`decision` is `"allow"`, `"ask"`, or `"deny"`.

Response `200`: the rule (`{"capability": …, "decision": …, "updated_at": …}`).

Errors: `400` (bad decision, malformed capability, or a capability that no
registered tool declares; see `GET /v1/tools`).

### `DELETE /v1/policy/{capability}`

Removes the caller's rule so the risk default applies again. Recorded in the
audit log. Response `204 No Content`.

Errors: `404` (you have no rule for that capability).

### `GET /v1/tools`

The tools the agent can call, sorted by name, with the decision policy would
make for the caller right now.

Response `200`:

```json
{
  "tools": [
    {
      "name": "websearch.fetch",
      "integration": "websearch",
      "description": "Fetch a public web page over http(s) and return its readable text (truncated). …",
      "capability": "websearch.fetch",
      "risk": "read",
      "decision": "allow"
    }
  ]
}
```

### `GET /v1/integrations`

Every integration the daemon initialised (including ones that currently offer
no tools, e.g. an MCP bridge whose servers all failed), sorted by name, with
runtime health.

Response `200`:

```json
{
  "integrations": [
    {
      "name": "mcp",
      "version": "0.1.0",
      "scope": "shared",
      "status": "degraded",
      "message": "notion failed",
      "tool_count": 26,
      "details": [
        {"name": "github", "transport": "stdio", "state": "ready", "tools": 26,
         "server_name": "github-mcp-server", "server_version": "0.6.0", "protocol_version": "2025-06-18"},
        {"name": "notion", "transport": "http", "state": "failed", "tools": 0,
         "error": "initialize: mcp server notion: HTTP 401: unauthorized"}
      ]
    },
    {"name": "websearch", "version": "0.1.0", "scope": "shared", "status": "ok", "tool_count": 2}
  ]
}
```

- `status` is `ok`, `degraded` or `down`. Integrations that do not report
  health are always `ok`.
- `details` is integration-specific and only returned to admins (it can contain
  commands, URLs and error text). For `mcp` it is one entry per configured
  server; `state` is `ready`, `failed` (never connected), `exited` (the stdio
  process went away; calls fail until bosund restarts) or `closed`.

### `POST /v1/integrations/{name}/test`

Tries a credential against the integration's backend **without storing it**,
so a wizard can check an app password before saving it with
`PUT /v1/credentials/{integration}/{kind}`. Only integrations that implement
the optional `integration.CredentialTester` interface support it (`email`,
`calendar`).

Request (the same shape as a credential, plus its kind):

```json
{"kind": "password", "secret": "<app password>",
 "fields": {"host": "imap.fastmail.com", "user": "me@fastmail.com"}}
```

Response `200`, whether or not the credential works:

```json
{"ok": true}
{"ok": false, "error": "wrong password or login refused: imap.gmail.com rejected the login for dave@gmail.com (server said: [AUTHENTICATIONFAILED] Invalid credentials). Gmail only accepts an app password here, ..."}
```

- `email`, kind `password`: logs in over IMAP, then authenticates to the
  SMTP submission server (EHLO, STARTTLS where used, AUTH, QUIT); nothing is
  sent. Errors distinguish a refused login (with a provider hint: Gmail's
  2-Step Verification requirement, Microsoft's basic-auth retirement), an
  unreachable server, and SMTP refusing a login IMAP accepted.
- `calendar`, kind `password`: CalDAV discovery must find at least one
  calendar. Kinds `ics` / `ics:<name>`: the secret is the feed URL; it must be
  `https://` (or `webcal://`), resolve to a public address, be at most 5 MB
  and parse as iCalendar.
- The secret is never logged or audited (the log line has user, integration,
  kind and outcome only) and is replaced by `[redacted]` if it appears in the
  error text.
- Rate limit: 5 tests per minute per user (`429 rate_limited` with
  `Retry-After`), so the endpoint cannot be used to guess passwords.
- Errors: `404 not_found` (integration not enabled), `400 bad_request`
  (integration cannot test credentials, undeclared kind, missing secret,
  invalid fields).

### `GET /v1/hooks` (admin only)

The registered hooks, keyed by hook point, each list in run order. Every
runnable point is present (possibly empty). Members get `403 forbidden`.

Response `200`:

```json
{
  "hooks": {
    "pre_model_call": [
      {"name": "builtin.redact_secrets", "type": "go"},
      {"name": "script-1-bosun-scrub", "type": "script"}
    ],
    "pre_tool_call": [],
    "post_tool_call": [],
    "on_approval": [{"name": "notify", "type": "script"}]
  }
}
```

- `type` is `go` (compiled in) or `script` (an admin-configured executable under
  `hooks.script_hooks` in `bosun.yaml`; listed only when
  `hooks.script_hooks.enabled` is true). There is no API to add, change, or
  enable hooks: they are configuration only.
- A failing `pre_tool_call` hook stops the tool call; a failing
  `post_tool_call` hook withholds the result; a failing `pre_model_call` hook
  fails the turn (the stream ends with an `error` event whose message starts
  `blocked by pre_model_call hook`). Script hook failures are recorded in the
  audit log with `kind = hook`.

### `GET /v1/memory`

The caller's memories: facts, preferences, summaries, and notes the agent
recalls into its system prompt. Some are created automatically after each
successful turn (`source` is `"conv:<conversation id>"`), others by the user
(`source` is `"user"`).

Query parameters (all optional):

| Param | Meaning |
|---|---|
| `q` | Full-text search; every word must match. Results are ordered by relevance (FTS5 bm25). Words are matched literally: quotes, `*`, `AND`/`OR`/`NOT`, `NEAR(…)` and other FTS syntax are not interpreted. Without `q`, results are ordered most recently updated first. |
| `kind` | `fact`, `preference`, `summary`, or `note` |
| `limit` | 1–500, default 50 |
| `offset` | ≥ 0, default 0 |

Response `200`:

```json
{
  "memories": [
    {
      "id": 7,
      "kind": "note",
      "content": "Alice's boat is called Sea Breeze.",
      "source": "conv:conv_3f…",
      "created_at": "2026-10-12T22:00:00Z",
      "updated_at": "2026-10-12T22:00:00Z"
    }
  ]
}
```

Errors: `400` (bad `kind`, `limit`, or `offset`).

### `POST /v1/memory`

Adds a memory for the caller.

Request:

```json
{ "kind": "preference", "content": "Prefers metric units." }
```

`kind` is `fact`, `preference`, `summary`, or `note`. `content` is trimmed and
must be non-empty and at most 4000 bytes.

Response `201`: the memory object (`source` is `"user"`).

Errors: `400` (bad kind or content).

### `PUT /v1/memory/{id}`

Replaces the content of one of the caller's memories (the kind is kept).

Request:

```json
{ "content": "Prefers imperial units." }
```

Response `200`: the updated memory object.

Errors: `400` (empty or oversized content), `404` (no such memory, or it
belongs to another user).

### `DELETE /v1/memory/{id}`

Deletes one of the caller's memories. Response `204 No Content`.

Errors: `404` (no such memory, or it belongs to another user).

### Routines

A routine is a named prompt the agent runs **as its owner** on a trigger:
a cron schedule or an event. Runs go through exactly the same policy as
interactive turns; a tool call that needs approval produces the usual
`approval_required` event and the run waits for the answer (or the approval
timeout). Each run gets a new conversation titled `routine: <name>`, readable
with `GET /v1/conversations/{id}`; when it ends the owner's clients receive a
`routine_finished` event.

Routine IDs and run IDs are integers. All routine endpoints are scoped to the
caller; another user's routine is `404`.

Routine object:

```json
{
  "id": 4,
  "name": "morning-brief",
  "prompt": "Summarize today's calendar.",
  "trigger": { "cron": "0 7 * * 1-5" },
  "model": "anthropic/claude-haiku-5-5",
  "deliver": ["web", "cli"],
  "enabled": true,
  "next_run_at": "2026-10-13T05:00:00Z",
  "last_run_at": "2026-10-12T05:00:00Z",
  "created_at": "2026-10-01T09:00:00Z",
  "updated_at": "2026-10-01T09:00:00Z"
}
```

- `trigger` has exactly one of `cron` or `event`.
  - `cron`: five fields `minute hour day-of-month month day-of-week` with
    `*`, lists (`1,15`), ranges (`1-5`), steps (`*/15`, `8-18/2`), month and
    weekday names (`JAN`, `MON-FRI`; 0 and 7 are Sunday), or one of
    `@hourly`, `@daily` (`@midnight`), `@weekly`, `@monthly`, `@yearly`
    (`@annually`). When both day fields are restricted, either may match.
    Schedules are evaluated in the daemon's `daemon.timezone` (default: the
    system zone). On a DST spring-forward, a time inside the skipped hour
    fires once at the end of the gap; on fall-back, a repeated time fires
    once. If the daemon was down when a run was due, the routine runs once
    at startup (not once per missed time).
  - `event`: dotted lower-case name such as `email.received`. The event's
    data is appended to the prompt as a delimited untrusted-data block, and
    memory extraction is skipped for such runs. Built-in events:
    `email.received` (the `email` integration's poller, once per new INBOX
    message, for the mailbox owner) with data
    `{"uid", "folder", "from", "subject", "snippet", "date"}`.
- `model` (optional) overrides `llm.default` as `<provider>/<model>`; an
  unknown provider fails the run with `model not available: …`.
- `deliver`: `web` and/or `cli` (default both). In v1 both mean the reply is
  stored in the run and its conversation and announced with
  `routine_finished`.
- `next_run_at` is present only for enabled cron routines; `last_run_at` only
  after a run.

Run object:

```json
{
  "id": 9,
  "routine_id": 4,
  "status": "done",
  "source": "schedule",
  "conversation_id": "conv_3f…",
  "scheduled_for": "2026-10-12T05:00:00Z",
  "started_at": "2026-10-12T05:00:01Z",
  "finished_at": "2026-10-12T05:00:09Z",
  "reply": "You have two meetings today…"
}
```

`status` is `running`, `waiting_approval` (blocked on an approval),
`done`, `error` (with `error`), or `canceled` (the daemon shut down
mid-run). `source` is `schedule`, `event` (with `event`), or `manual`.
`reply` is the run's final assistant text.

#### `GET /v1/routines`

Response `200`: `{"routines": [<routine>, …]}` ordered by name.

#### `POST /v1/routines`

```json
{
  "name": "morning-brief",
  "prompt": "Summarize today's calendar.",
  "trigger": { "cron": "0 7 * * 1-5" },
  "model": "",
  "deliver": ["web"],
  "enabled": true
}
```

`name` (≤ 100 bytes, unique per user), `prompt` (≤ 16 KiB) and `trigger` are
required; `enabled` defaults to `true`. Response `201`: the routine.

Errors: `400` (invalid field; a cron error says which field is wrong, e.g.
`trigger: cron: hour field "25": value 25 out of range 0-23`), `409` (name
already used).

#### `GET /v1/routines/{id}`

Response `200`: the routine. Errors: `404`.

#### `PUT /v1/routines/{id}`

Partial update: omitted fields are kept. `trigger` replaces the whole trigger;
`"model": ""` clears the override; `"enabled": false` disables. The next run
time is recomputed from now, so re-enabling never fires for times that passed
while disabled.

```json
{ "trigger": { "cron": "@hourly" }, "enabled": false }
```

Response `200`: the routine. Errors: `400`, `404`, `409` (name taken).

#### `DELETE /v1/routines/{id}`

Deletes the routine and its run history (its conversations are kept).
Response `204`. Errors: `404`.

#### `POST /v1/routines/{id}/run`

Starts a run now, even if the routine is disabled. The run proceeds in the
background. Response `202`: the run (status `running`); watch for
`routine_finished` with its `run_id`, or poll the runs list.

Errors: `404`, `409` (a run of this routine is already in progress), `503`.

#### `GET /v1/routines/{id}/runs`

Query: `limit` 1–200, default 20. Response `200`: `{"runs": [<run>, …]}`,
newest first. Errors: `400` (bad limit), `404`.

### `GET /v1/credentials`

Metadata for the credentials in the vault that the caller can use: their own
(`scope: "user"`) and the household-wide shared ones (`scope: "shared"`,
visible to every user so members can see whether a shared integration is
configured). Secrets and field values are never returned. Optional query:
`integration=<name>`.

```json
{ "credentials": [
  { "integration": "email", "kind": "oauth2:google", "scope": "user",
    "expiry": "2027-01-31T00:00:00Z",
    "created_at": "2026-10-12T22:00:00Z", "updated_at": "2026-10-12T22:00:00Z" },
  { "integration": "mcp", "kind": "bearer:github", "scope": "shared",
    "created_at": "2026-10-12T22:00:00Z", "updated_at": "2026-10-12T22:00:00Z" }
] }
```

`expiry` is omitted when the credential does not expire. Sorted by
integration, kind, then user before shared.

### `PUT /v1/credentials/{integration}/{kind}`

Creates or replaces a credential, encrypted with the daemon master key.

```json
{ "secret": "ghp_…", "fields": { "username": "dave" }, "expiry": "2027-01-31T00:00:00Z" }
```

- `secret` (required, at most 64 KiB) is the API key, token or password.
- `fields` (optional, at most 32) are extra values the integration needs;
  names match `^[a-z][a-z0-9_]*$`, values at most 8 KiB.
- `expiry` (optional, RFC 3339) must be in the future.
- `integration` must be an initialised integration (`GET /v1/integrations`);
  otherwise `404`.
- `kind` must be one the integration declares in its manifest, or
  `<declared>:<suffix>` for a declared kind without a suffix (an integration
  declaring `bearer` accepts `bearer:github`). The MCP bridge declares the
  kinds named by its servers' `credential:` settings, e.g. `bearer:github`.
  Otherwise `400`.
- **Scope** (`?scope=user|shared`): per-user integrations store the caller's
  own credential; `scope=shared` is rejected for them (`400`), because they
  never read shared credentials. Shared integrations (`scope: "shared"` in
  `GET /v1/integrations`, e.g. `mcp`, `websearch`) only use the shared
  credential, so the scope defaults to `shared` for them and `scope=user` is
  rejected (`400`). Shared credentials can only be written by an admin
  (`403 forbidden` otherwise).

Response `200`: the credential's metadata, as in the list. Each write is
recorded in the audit log as kind `config` with the integration, kind, scope
and field *names* in `detail`, never the secret or field values.

Integrations that connect at startup (MCP servers) use a new credential after
`bosund` restarts.

### `DELETE /v1/credentials/{integration}/{kind}`

Deletes a credential, with the same scope rules as `PUT` (shared credentials
are admin-only). The integration does not have to be enabled any more, so
stale credentials can be removed; for an unknown integration the scope is
taken from `?scope=` (default `user`). Response `204 No Content`; audited like
`PUT`.

Errors: `403` (member deleting a shared credential), `404` (no such
credential).

### OAuth account linking

`bosun connect <provider>` links a user's Google or Microsoft account. The
tokens are stored in the vault as the caller's credential
`("oauth", "oauth2:<provider>")` and every integration that asks its
credential source for kind `oauth2:<provider>` receives a fresh access token
(see ARCHITECTURE.md, "Users, auth, and credentials"). Providers are
configured by the admin under `oauth:` in `bosun.yaml`; an unconfigured
provider answers `400`.

#### `POST /v1/oauth/{provider}/start`

```json
{ "scopes": ["email", "calendar"] }
```

`scopes` are scope bundle names (google: `email`, `calendar`, `tasks`,
`drive`, `imap`, `caldav`; microsoft: `email`, `calendar`, `tasks`, `drive`);
an entry starting with `https://` is passed through as a literal scope; empty
selects the provider's defaults (`email`, `calendar`). Response `200`:

```json
{
  "provider": "google",
  "state": "6Qx…",
  "auth_url": "https://accounts.google.com/o/oauth2/v2/auth?…&code_challenge_method=S256&…",
  "redirect_url": "http://127.0.0.1:7777/oauth/callback",
  "scopes": ["openid", "email", "https://www.googleapis.com/auth/gmail.readonly", "…"],
  "expires_at": "2026-10-13T10:10:00Z"
}
```

The daemon keeps a single-use state (user, provider, PKCE S256 verifier,
scopes) for 10 minutes; it is held in memory, so a daemon restart cancels
pending authorizations. `redirect_url` is `/oauth/callback` on the host and
scheme the request came in on, so it must be registered with the provider:
`http://127.0.0.1:<port>/oauth/callback` for a loopback daemon and
`https://<host>:<port>/oauth/callback` for a LAN/TLS one. At most 10 pending
authorizations per user (`429`).

Errors: `400` (unknown bundle, provider not configured or missing its client
secret), `404` (unknown provider).

#### `GET /v1/oauth/{provider}/status?state=<state>`

Polled by the CLI. Response `200`:

```json
{ "status": "complete",
  "account": { "provider": "google", "email": "dave@example.com", "scopes": ["…"],
               "expiry": "…", "linked_at": "…", "updated_at": "…" } }
```

`status` is `pending`, `complete`, `error` (with `error`, e.g. the user denied
consent or the code exchange failed) or `expired` (with `error`). `404` for a
state that is unknown, purged, or belongs to another user.

#### `GET /oauth/callback?code=…&state=…` (no auth, outside `/v1`)

The provider's redirect target, served by `bosund`. It consumes the state
(replays fail), exchanges the code with the PKCE verifier, reads the account
email from the provider's userinfo endpoint, stores the tokens for the user who
started the flow, audits the link (kind `config`, provider, email and scopes;
never tokens), and renders a small HTML page ("You can close this tab").
`400` for an invalid, expired or reused state; `502` if the provider refused
the code or consent was denied.

#### `GET /v1/oauth`

The caller's linked accounts (never tokens) and the known providers:

```json
{
  "accounts": [{ "provider": "google", "email": "dave@example.com", "scopes": ["…"],
                 "expiry": "…", "linked_at": "…", "updated_at": "…" }],
  "providers": [{ "name": "google", "configured": true,
                  "bundles": { "email": ["…"], "calendar": ["…"] },
                  "default_bundles": ["email", "calendar"] }]
}
```

`expiry` is the current access token's; it is refreshed on use.

#### `DELETE /v1/oauth/{provider}`

Unlinks: calls the provider's revocation endpoint with the refresh token (best
effort; Google has one, Microsoft does not) and deletes the stored tokens.
`204`; `404` if nothing is linked. Audited (kind `config`).

### Channels

Chat-app frontends (v1: Telegram, see the README). A chat acts as a user only
after it is *linked*: the user requests a one-time code here and sends
`/link <code>` from inside the chat app.

#### `GET /v1/channels`

The channels enabled on this server and the caller's links.

```json
{ "channels": ["telegram"],
  "links": [
    { "channel": "telegram", "external_id": "123456789", "display_name": "Dave (@dave)",
      "conversation_id": "conv_…", "created_at": "2026-10-13T10:00:00Z" }
  ] }
```

`external_id` is the channel's identity for the chat (for Telegram, the
private chat ID). `conversation_id` is the conversation the chat talks to
(omitted until its first message).

#### `POST /v1/channels/{channel}/link`

Issues a link code for the caller. No body. Response `201`:

```json
{ "channel": "telegram", "code": "K7QX2M", "expires_at": "2026-10-13T10:10:00Z",
  "instructions": "Send /link K7QX2M to @my_bosun_bot on Telegram." }
```

Codes are 6 characters (`A-Z` and `2-9` without look-alikes), valid for 10
minutes, single-use, and held in memory (a daemon restart voids them). A new
code replaces the caller's previous one for that channel. Redeeming one from
an identity already linked to another user fails. Links and unlinks are
audited (kind `config`, integration `channels`). Errors: `404` (channel not
enabled).

#### `DELETE /v1/channels/{channel}/{external_id}`

Unlinks one of the caller's chats. Response `204`; `404` if the caller has no
such link. Deleting a user deletes their links.

The Telegram bot token is the shared credential `channels/telegram_bot_token`
(`PUT /v1/credentials/channels/telegram_bot_token`, admin only). The
`channels` integration appears in `GET /v1/integrations` while a channel is
enabled, with its health (`down` until a token is set, `ok` while polling).

### Household users

`bosun setup` creates the first admin. Admins add everyone else; a new member
gets a one-time **invite code** and chooses their own password, so the admin
never knows it. Every change below is audited (kind `auth`, `user_id` = the
acting user, `detail` = target user ID/username and what changed); passwords
and invite codes are never logged or stored in clear (codes are stored as
SHA-256 hashes).

User objects in this section extend the [user object](#user-object):

```json
{ "id": 2, "username": "alice", "role": "member", "created_at": "…",
  "disabled": false, "last_login_at": "…", "invite_expires_at": "…" }
```

`last_login_at` is absent until the first login (password or invite);
`invite_expires_at` is present while an unredeemed invite is outstanding.

#### `GET /v1/users` (admin only)

Response `200`: `{ "users": [ <user>, … ] }`, ordered by ID.

#### `POST /v1/users` (admin only)

```json
{ "username": "alice", "role": "member" }
```

`role` defaults to `member`. Response `201`:

```json
{ "user": <user>, "invite": { "code": "bsi_…", "expires_at": "…" } }
```

The code is valid for 24 hours and works once; it is returned only here. The
account cannot log in until the invite is accepted. With `"password": "…"` in
the request the password is set directly and `invite` is omitted. Errors:
`400` (bad username, role, or password), `409 conflict` (username taken,
case-insensitive).

#### `POST /v1/invites/{code}/accept` (no auth)

```json
{ "password": "…", "username": "alice", "label": "cli" }
```

`username` is optional; if given it must match the invite. Sets the password,
consumes the code (and any other invite of that user), revokes the user's
existing sessions, and logs in. Response `200`: as [`POST /v1/login`](#post-v1login-implemented-no-auth).
Errors: `400` (password too short, username mismatch), `404 invalid_invite`
(unknown, expired, used, or the user is disabled), `429 rate_limited` (10
attempts per minute per client address). The code is matched
case-insensitively after trimming spaces.

#### `PUT /v1/users/{id}` (admin only)

```json
{ "role": "admin", "disabled": true }
```

Either field may be omitted (not both). Disabling signs the user out
everywhere and cancels pending invites; their data is kept. Response `200`:
the user. Errors: `404`, `409 last_admin` when demoting or disabling the last
enabled admin (including yourself).

#### `DELETE /v1/users/{id}` (admin only)

Deletes the user and, in one transaction, everything they own: sessions,
invites, credentials, policy rules, conversations and messages, memories,
routines and their runs, approvals, and channel links. References from other
users' rows (an approval they resolved, a hook they created) become `null`.
The audit log is kept. A deleted user's ID is never reused. Response `204`.
Errors: `404`, `409 last_admin`.

#### `POST /v1/users/{id}/reset-password` (admin only)

Replaces the user's password with an unknown one, signs them out, and issues
a new invite (any earlier code stops working). Response `200`:
`{ "user": <user>, "invite": { "code": "bsi_…", "expires_at": "…" } }`.
Errors: `400` (user is disabled), `404`.

#### `PUT /v1/me/password`

Any user changes their own password:

```json
{ "current": "…", "new": "…" }
```

Response `204`. All of the user's other sessions are revoked; the one making
the request stays valid. Errors: `400` (new password too short),
`403 invalid_credentials` (wrong current password).

---

## SSE events

Each event is framed as:

```
event: <type>
data: <single-line JSON>

```

Every `data` object also carries `"conversation_id": "conv_…"` (omitted only
for events not tied to a conversation). It is left out of the table for
brevity.

| `event` | `data` | Meaning |
|---|---|---|
| `text` | `{"delta": "Here is"}` | A chunk of assistant text; concatenate deltas |
| `tool_call` | `{"id": "toolu_01", "name": "calendar.list", "args": {…}}` | The model called a tool |
| `tool_result` | `{"id": "toolu_01", "summary": "3 events today"}` | A tool call finished (summary only; full results stay server-side) |
| `approval_required` | `{"approval_id": "appr_…", "tool": "email.send", "args": {…}, "risk": "write"}` | A tool call is blocked waiting for the user; answer with `POST /v1/approvals/{id}` |
| `approval_resolved` | `{"approval_id": "appr_…", "decision": "approve"}` | An approval was answered; `decision` is `approve`, `deny`, `expired`, or `canceled` |
| `done` | `{}` | The turn finished successfully |
| `error` | `{"message": "model request failed: …"}` | The turn failed; terminal |
| `routine_finished` | `{"routine_id": 4, "run_id": 9, "status": "done"}` | A routine run ended (`status` is `done`, `error`, or `canceled`); `conversation_id` is the run's conversation. Sent on `GET /v1/events` only |

`tool_call.id` and `tool_result.id` match each other. A denied, timed-out, or
failed tool call still produces a `tool_result`, whose summary starts with
`error: ` and says why; the model receives the same outcome as an error tool
result and the turn continues.

Go types for every payload live in `internal/api/events`
(`TextData`, `ToolCallData`, `ToolResultData`, `ApprovalRequiredData`,
`ApprovalResolvedData`, `DoneData`, `ErrorData`, `RoutineFinishedData`); `events.ConversationOf`
returns an event's conversation. Server code publishes with
`events.Bus.Publish(userID, events.Event{Type: events.TypeText, Data: …})` and
writes per-request streams with `events.NewSSEWriter`.
