# Bosun

An open-source personal agent that gets real work done, but only through
integrations you've explicitly granted, so it can never do more than you allowed.

See [GOALS.md](GOALS.md) for the why and [ARCHITECTURE.md](ARCHITECTURE.md) for
the how. The HTTP API is documented in [docs/API.md](docs/API.md).

## Status

MVP: the daemon runs a tool-using agent loop over the HTTP API, with per-user
policy, approvals, an audit log, hooks, and an encrypted credential vault.
Built-in integrations so far are `websearch` (`websearch.fetch` for public web
pages; `websearch.search` needs a SearXNG instance) and `notes` (Markdown
files, off by default), plus per-user memory, the MCP bridge (third-party
tools from external MCP servers), routines (scheduled and event-driven runs),
and the web UI. Calendar and email are not built yet.

## Quick start

Requires Go 1.26+.

```sh
make build                    # builds bin/bosund and bin/bosun
./bin/bosun setup             # writes ~/.bosun/bosun.yaml, creates the data dir and the first admin
```

`bosun setup` prompts for the listen address, data directory, admin username
and password; `--listen`, `--data-dir` and `--admin` skip those prompts. Use
`--config <path>` (or `BOSUN_CONFIG`) on every command to keep the config
elsewhere.

Pick a model in `bosun.yaml`. The default is `anthropic/claude-sonnet-5-5`,
which reads the key from `ANTHROPIC_API_KEY`; for a local model, set
`llm.default: ollama/<model>` (Ollama at `http://localhost:11434`). An
`openai` provider (any OpenAI-compatible endpoint, `base_url` + `api_key_env`)
is also supported.

```sh
export ANTHROPIC_API_KEY=...  # if using Anthropic
./bin/bosund                  # start the daemon (Ctrl-C / SIGTERM to stop)
```

In another terminal:

```sh
./bin/bosun login                 # stores a session token in <data_dir>/cli-token
./bin/bosun tools                 # what the assistant can do, and your policy for each
./bin/bosun chat "hello"          # one turn; `bosun chat` alone opens a session
```

### Web UI

`bosund` also serves a small browser UI at its listen address (by default
<http://127.0.0.1:7777/>): sign in with your Bosun username and password, chat
with streamed replies and tool calls, approve or deny pending approvals (also
those raised by routines in other conversations), set allow/ask/deny per tool,
and browse, add, edit and delete your memories. It is plain HTML/JS embedded in
the binary (no build step, nothing loaded from the network) and follows your
light/dark setting. The token lives in `sessionStorage`, so closing the tab
signs you out. Set `daemon.web_ui: false` in `bosun.yaml` to serve the API only.

### Permissions

Every tool has a capability (e.g. `websearch.fetch`) and a risk level. Unless
you set a rule, the risk default from `policy.defaults` applies (read: allow,
write: ask, destructive: ask). "ask" pauses the tool call until you answer;
`bosun chat` prompts inline, and `bosun approvals list` /
`bosun approvals <id> approve|deny` work from any terminal.

```sh
./bin/bosun policy set websearch.fetch ask    # always ask before fetching pages
./bin/bosun policy list
./bin/bosun policy unset websearch.fetch      # back to the risk default
```

### Memory

After each turn a cheap model pass proposes things worth remembering about you
(facts, preferences, and anything you ask it to "remember that …"); relevant
memories are added to the system prompt of later turns. Memories are private to
each user and fully editable:

```sh
./bin/bosun memory list                       # newest first; --kind note, --search "dentist"
./bin/bosun memory add preference "Prefers metric units."
./bin/bosun memory edit 7 "Boat is called Wave Runner."
./bin/bosun memory forget 7
```

Configure it under `memory:` in `bosun.yaml` (all optional):
`enabled: true`, `extract_model: ""` (empty = `llm.default`; a small, cheap
model is a good choice), `recall_limit: 8`.

### Notes

The `notes` integration lets the assistant list, search, read, write, append
to, and delete Markdown/text notes (`.md`, `.markdown`, `.txt`) in a folder.
`bosun setup` leaves it off; enable it in `bosun.yaml` and restart `bosund`:

```yaml
integrations:
  notes:
    enabled: true
    dir: ~/Notes        # default: <data_dir>/integrations/notes
    per_user: true      # default: each user's notes live in ~/Notes/<username>/
```

- Capabilities: `notes.read` (list/search/read; read risk), `notes.write`
  (write/append; write risk, asks by default), `notes.delete` (destructive).
- With `per_user: true` users never see each other's notes. With
  `per_user: false` everyone shares `dir`, and note contents are treated as
  untrusted data (another household member could have written instructions
  into a note).
- Paths are relative to the notes folder. Absolute paths, `..`, hidden
  files/folders (`.git`, `.obsidian`), other extensions, and symlinks that
  point outside the folder are refused. Reads return at most 200 KB.

### Hooks

Hooks run at fixed points of every request: `pre_model_call` (before each
model request; may rewrite the system prompt and messages), `pre_tool_call`
(may rewrite arguments or cancel), `post_tool_call` (may rewrite the result)
and `on_approval` (notification after an approval is answered, expires, or is
canceled). `bosun hooks list` (admin) shows what is registered.

One built-in hook is on by default: `builtin.redact_secrets` masks strings
that look like API keys or tokens (`sk-ant-…`, `ghp_…`, `xoxb-…`, `AKIA…`,
JWTs, runs of 32+ hex digits) in what is sent to the model. The stored
conversation keeps the original. Turn it off with `hooks.redact_secrets: false`.

**Script hooks** run an executable you provide. They are the one place Bosun
runs an external program on your behalf, so they are off by default and only
configurable in `bosun.yaml` (no API), and `bosund` logs each one at WARN
level on startup:

```yaml
hooks:
  script_hooks:
    enabled: false                 # must be true for any script to run
    scripts:
      - point: pre_tool_call       # pre_tool_call | post_tool_call | pre_model_call | on_approval
        command: ["/usr/local/bin/bosun-guard", "--strict"]   # absolute path; no shell
        timeout: 5s                # default 5s, max 60s
        match: "email.*"           # optional glob on tool or capability (tool points only)
        name: guard                # optional; default script-<n>-<command>
```

The script gets one JSON object on stdin (`point`, `user_id`, `tool`,
`integration`, `capability`, `risk`, `tool_call_id`, `args`; plus `result`
`{content, untrusted}` for post_tool_call, `model`/`system`/`messages` for
pre_model_call, `approval`/`decision` for on_approval) and must print one JSON
object on stdout:

| Point | Output |
|---|---|
| `pre_tool_call` | `{"action": "continue" \| "cancel", "args": {...}, "reason": "..."}` (`args` optional; rewritten args are validated and checked by policy) |
| `post_tool_call` | `{"result": "replacement content"}` or `{}` |
| `pre_model_call` | `{"system": "...", "messages": [...]}`, both optional, or `{}` |
| `on_approval` | ignored |

A non-zero exit, a timeout, or invalid JSON **fails closed**: the tool call is
not run, its result is withheld, or the turn fails; on_approval failures are
only logged. Every failure is recorded in the audit log (`kind = hook`).
Scripts get only `PATH` and `HOME` in their environment (no API keys), run in
bosund's working directory, and run with bosund's privileges.

### MCP servers

Any [Model Context Protocol](https://modelcontextprotocol.io) server can add
tools without rebuilding Bosun. List servers under `integrations.mcp` in
`bosun.yaml` and restart `bosund`:

```yaml
integrations:
  mcp:
    servers:
      - name: github                 # [a-z0-9_-], becomes the prefix mcp.github.*
        command: ["npx", "-y", "@modelcontextprotocol/server-github"]
        env: { GITHUB_PERSONAL_ACCESS_TOKEN: GITHUB_TOKEN }  # child var <- daemon var
        cwd: ""                      # optional working directory
        timeout: 60s                 # optional, per tool call
      - name: notion
        url: https://mcp.example.com/mcp   # streamable HTTP
        credential: bearer           # optional: vault credential kind "bearer" (or "bearer:<suffix>")
```

- Each remote tool becomes `mcp.<server>.<tool>` (tool names are lowercased
  and sanitized to `[a-z0-9_-]`), with a capability of the same name you can
  set policy on like any other: `bosun policy set mcp.github.create_issue allow`.
- Risk is **write** (asks for approval by default) unless the server marks the
  tool `readOnlyHint` (read) or `destructiveHint` (destructive). Results are
  always treated as untrusted data.
- Stdio servers run with a minimal environment: `PATH`, `HOME`, `LANG`, and
  only the variables you map under `env`. The daemon's other secrets (LLM API
  keys, …) are not passed on.
- A server that fails to start is skipped, not fatal. `bosun mcp status` shows
  each server's state, tool count and error (details are admin-only). A stdio
  server that exits is not restarted until `bosund` restarts.
- HTTP bearer tokens come from the credential vault (shared credential for
  integration `mcp`, the configured kind). There is no CLI for writing vault
  credentials yet.
- MCP servers are third-party code running with your user's privileges; only
  configure servers you trust.

### Routines

A routine runs a prompt as you on a schedule (cron, in `daemon.timezone`,
default the system zone) or when an event happens, with the same permissions
as your chats: anything that needs approval waits for you.

```sh
./bin/bosun routines add --name morning-brief --cron "0 7 * * 1-5" \
    --prompt "Summarize today's calendar and flag anything urgent."
./bin/bosun routines list
./bin/bosun routines run 1 --wait             # run now and print the reply
./bin/bosun routines runs 1                   # history: status, reply, conversation
./bin/bosun routines edit 1 --disable
```

Each run gets its own conversation (`routine: <name>`). Routines can also be
seeded from `bosun.yaml` (created once per owner and name; later edits made
with the CLI are kept):

```yaml
routines:
  - name: morning-brief
    owner: dave
    trigger: { cron: "0 7 * * 1-5" }
    prompt: "Summarize today's calendar."
    model: anthropic/claude-haiku-5-5   # optional override
```

### Audit log

Every tool call (with its policy decision and outcome), approval, policy
change, and model request (provider, model, token counts) is appended to the
`audit_log` table in `<data_dir>/bosun.db` (SQLite). Message content and tool
arguments are redacted there unless `audit.log_content: true`.

```sh
sqlite3 ~/.bosun/bosun.db "select ts, kind, tool, decision, error from audit_log order by id desc limit 20"
```

## Development

```sh
make test   # go test ./...
make vet    # go vet ./...
make lint   # gofmt + vet + integration import guard
```

Dependencies are kept minimal: `modernc.org/sqlite` (pure Go, no cgo),
`gopkg.in/yaml.v3`, `golang.org/x/crypto` (argon2id), `golang.org/x/term`
(password prompts), and `github.com/spf13/cobra` for the CLI.

## License

TBD.
