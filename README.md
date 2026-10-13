# Bosun

An open-source personal agent that gets real work done, but only through
integrations you've explicitly granted, so it can never do more than you allowed.

See [GOALS.md](GOALS.md) for the why and [ARCHITECTURE.md](ARCHITECTURE.md) for
the how. The HTTP API is documented in [docs/API.md](docs/API.md).

## Status

MVP: the daemon runs a tool-using agent loop over the HTTP API, with per-user
policy, approvals, an audit log, and an encrypted credential vault. The only
built-in integration so far is `websearch` (`websearch.fetch` for public web
pages; `websearch.search` needs a SearXNG instance), per-user memory, the MCP
bridge (third-party tools from external MCP servers), and the web UI.
Calendar, email, notes, and routines are not built yet.

## Quick start

Requires Go 1.26+.

```sh
make build                    # builds bin/bosund and bin/bosun
./bin/bosun setup             # writes ~/.bosun/bosun.yaml, creates the data dir and the first admin
```

`bosun setup` prompts for LAN access, the listen address, data directory, admin
username and password; `--lan`, `--listen`, `--data-dir` and `--admin` skip
those prompts (see [LAN](#using-bosun-from-other-devices-lan)). Use
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
- HTTP bearer tokens come from the credential vault: the shared credential
  for integration `mcp` with the configured kind. An admin stores it with
  `bosun credentials set mcp bearer:github` (see [Credentials](#credentials))
  and restarts `bosund`.
- MCP servers are third-party code running with your user's privileges; only
  configure servers you trust.

### Credentials

Integration secrets (API keys, tokens) live in the daemon's encrypted vault.
Credentials are per user, except for shared integrations (`mcp`, `websearch`:
one household-wide backend), which use a single shared credential that only an
admin can set.

```sh
pass show github-token | ./bin/bosun credentials set mcp bearer:github   # secret from stdin
./bin/bosun credentials set mcp bearer:notion                             # hidden prompt
./bin/bosun credentials list                    # metadata only, never secrets
./bin/bosun credentials remove mcp bearer:notion
```

The secret is read from stdin or a hidden prompt, never from a flag, so it
does not end up in shell history or the process table. For shared
integrations `--shared` is implied (and said so); `--shared` on a per-user
integration is an error. `--field key=value` adds non-secret extra values and
`--expires` an RFC 3339 expiry. The kinds an integration accepts are the ones it
declares; for MCP, the `credential:` value of a server (`bearer` or
`bearer:<suffix>`). Every change is audited without the secret.

**Master key.** Credentials are encrypted (AES-256-GCM) under a master key
chosen by `daemon.master_key` in `bosun.yaml`:

- `auto` (default): the OS keychain if available (macOS Keychain via
  `security`; on Linux the Secret Service via `secret-tool`), else
  `<data_dir>/master.key` (mode 0600).
- `keychain`: the OS keychain, or fail to start.
- `file`: `<data_dir>/master.key` only.

`bosund` logs which one it uses at startup. When the keychain is used and a
`master.key` file already exists, its key is imported into the keychain and
the file is **left in place** (with a log notice): delete it yourself once the
daemon has started with the keychain, since anyone who can read it can decrypt
the vault. Do not switch to `file` after a key was generated in the keychain:
the file would hold a different key.

### Using Bosun from other devices (LAN)

By default `bosund` listens on `127.0.0.1` only. To reach it from your phone or
another computer, enable LAN mode, which always uses HTTPS:

```sh
./bin/bosun setup --lan    # or answer "y" to "Allow other devices…" in an interactive setup
```

This sets `daemon.listen: 0.0.0.0:7777`, `daemon.tls.enabled: true`, and
generates a self-signed ECDSA P-256 certificate (valid 10 years) in
`<data_dir>/tls/` (`cert.pem`, `key.pem` with mode 0600) for this machine's
hostname, `localhost`, and its current IPv4 addresses. To use your own
certificate instead, set `daemon.tls.cert_file` and `daemon.tls.key_file`. If
the machine's IP changes, `bosun setup --lan --regenerate-cert` makes a new one.

`bosun setup` and `bosund` (at every start) print the certificate's SHA-256
fingerprint:

```
TLS certificate sha256_fingerprint=3A:7F:…:C2 names=myhost,localhost,127.0.0.1,::1,192.168.1.20
```

On the other device, check that the fingerprint matches what the daemon
printed (read it on the daemon's machine, not over the network you are
verifying), then pin it:

```sh
bosun --server https://myhost:7777 --insecure-fingerprint 3A:7F:…:C2 login
bosun chat "hello"         # later commands reuse the server and the pin
```

The pin accepts exactly that certificate and nothing else; there is no
"skip verification" option. If you trust the certificate another way, use
`--ca-file cert.pem` instead. The server and pin are saved in
`<data_dir>/cli-server.json` next to the CLI token. Browsers will warn about
the self-signed certificate; compare the fingerprint in the certificate viewer
before accepting it. On the daemon's own machine the CLI trusts
`daemon.tls.cert_file` automatically.

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
