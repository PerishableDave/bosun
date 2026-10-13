# Bosun HTTP API (v1)

`bosund` serves a JSON + Server-Sent Events API, by default on
`http://127.0.0.1:7777`. LAN binding requires TLS. The `bosun` CLI and the web
UI are clients of this API.

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
| 403 | `forbidden` | Answering an approval that belongs to another user |
| 404 | `not_found` | No such endpoint or resource (or not yours) |
| 409 | `conflict` | State precondition failed, e.g. approval already resolved or expired, or a turn already running |
| 500 | `internal` | Server error (details are logged, never returned) |
| 501 | `not_implemented` | The endpoint's component is not configured on this server |

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

`tool_call.id` and `tool_result.id` match each other. A denied, timed-out, or
failed tool call still produces a `tool_result`, whose summary starts with
`error: ` and says why; the model receives the same outcome as an error tool
result and the turn continues.

Go types for every payload live in `internal/api/events`
(`TextData`, `ToolCallData`, `ToolResultData`, `ApprovalRequiredData`,
`ApprovalResolvedData`, `DoneData`, `ErrorData`); `events.ConversationOf`
returns an event's conversation. Server code publishes with
`events.Bus.Publish(userID, events.Event{Type: events.TypeText, Data: …})` and
writes per-request streams with `events.NewSSEWriter`.
