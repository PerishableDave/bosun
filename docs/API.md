# Bosun HTTP API (v1)

`bosund` serves a JSON + Server-Sent Events API, by default on
`http://127.0.0.1:7777`. LAN binding requires TLS. The `bosun` CLI and the web
UI are clients of this API.

Status legend: **implemented** endpoints work today; **contract** endpoints
currently return `501 Not Implemented` but their request/response shapes below
are fixed and other components must implement them as written.

## Conventions

- **Auth.** Send `Authorization: Bearer <token>`. Tokens come from
  `POST /v1/login`, start with `bsn_`, and are opaque. Only their SHA-256 hash
  is stored server-side. Missing/invalid/expired tokens get `401` with a
  `WWW-Authenticate: Bearer` header. Tokens are never accepted in URLs.
- **Bodies.** Requests are a single JSON object (`Content-Type:
  application/json`), max 1 MiB. Unknown fields are rejected with `400`.
- **Timestamps** are RFC 3339 strings in UTC.
- **IDs.** Users are integers. Conversations (`conv_…`) and approvals
  (`appr_…`) are opaque strings.
- **Isolation.** Every resource is scoped to the authenticated user. A
  resource owned by another user is reported as `404`, never `403`.

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
| 404 | `not_found` | No such endpoint or resource (or not yours) |
| 409 | `conflict` | State precondition failed, e.g. approval already resolved or expired |
| 500 | `internal` | Server error (details are logged, never returned) |
| 501 | `not_implemented` | Contract endpoint not built yet |

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

On this stream you will typically see `approval_required` and
`approval_resolved` (see [SSE events](#sse-events)); any event type may be
published to it.

### `POST /v1/conversations` (contract)

Request (all fields optional):

```json
{ "title": "Morning planning" }
```

Response `201`:

```json
{ "id": "conv_3f2a…", "title": "Morning planning", "created_at": "2026-10-12T22:00:00Z" }
```

### `POST /v1/conversations/{id}/messages` (contract, SSE response)

Sends a user message and streams the agent's turn.

Request:

```json
{ "content": "Summarize today's calendar" }
```

`content` is required. Response `200` with `Content-Type: text/event-stream`;
the body is a sequence of [SSE events](#sse-events): any number of `text`,
`tool_call`, `tool_result`, `approval_required`, `approval_resolved`, ending
with exactly one `done` or `error`, after which the server closes the stream.

Errors before streaming starts use normal JSON errors: `400`, `401`,
`404` (conversation not found or not yours), `409` (a turn is already running
in this conversation).

If the client disconnects mid-turn the turn is canceled; any pending approval
for it is marked `canceled`.

### `GET /v1/approvals` (contract)

Lists the caller's pending, unexpired approvals, oldest first.

Response `200`:

```json
{
  "approvals": [
    {
      "id": "appr_9c1e…",
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
`approved` | `denied` | `expired` | `canceled`.

### `POST /v1/approvals/{id}` (contract)

Request:

```json
{ "decision": "approve" }
```

`decision` is `"approve"` or `"deny"` (anything else → `400`).

Response `200`: the updated approval object (as above, with `status`,
`resolved_at`, and `resolved_by` set). An `approval_resolved` event is
published to all of the user's clients.

Errors: `404` (no such approval, or not yours), `409 conflict` (already
resolved by another client, expired, or canceled).

---

## SSE events

Each event is framed as:

```
event: <type>
data: <single-line JSON>

```

| `event` | `data` | Meaning |
|---|---|---|
| `text` | `{"delta": "Here is"}` | A chunk of assistant text; concatenate deltas |
| `tool_call` | `{"id": "toolu_01", "name": "calendar.list", "args": {…}}` | The model called a tool |
| `tool_result` | `{"id": "toolu_01", "summary": "3 events today"}` | A tool call finished (summary only; full results stay server-side) |
| `approval_required` | `{"approval_id": "appr_…", "tool": "email.send", "args": {…}, "risk": "write"}` | A tool call is blocked waiting for the user; answer with `POST /v1/approvals/{id}` |
| `approval_resolved` | `{"approval_id": "appr_…", "decision": "approve"}` | An approval was answered; `decision` is `approve`, `deny`, or `expired` |
| `done` | `{}` | The turn finished successfully |
| `error` | `{"message": "model request failed"}` | The turn failed; terminal |

`tool_call.id` and `tool_result.id` match each other. A denied or timed-out
tool call still produces a `tool_result` whose summary says so.

Go types for every payload live in `internal/api/events`
(`TextData`, `ToolCallData`, `ToolResultData`, `ApprovalRequiredData`,
`ApprovalResolvedData`, `DoneData`, `ErrorData`). Server code publishes with
`events.Bus.Publish(userID, events.Event{Type: events.TypeText, Data: …})` and
writes per-request streams with `events.NewSSEWriter`.
