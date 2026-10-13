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
and the web UI, plus `email` (IMAP/SMTP) and `calendar` (CalDAV: Google,
iCloud, Fastmail, Nextcloud, ...), both off by default.

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

### Email

The `email` integration reads, searches, drafts, sends, archives and flags
mail over IMAP and SMTP, so it works with Gmail, Fastmail, iCloud,
Outlook.com and self-hosted servers. Enable it in `bosun.yaml` and restart
`bosund`:

```yaml
integrations:
  email:
    enabled: true
    poll_interval: 2m   # check INBOXes for new mail (email.received events); "off" disables
```

Accounts are per user and stored in the credential vault. Each user links
their own, in one of two ways:

**App password (any provider).** Create an app password at your provider
(Gmail: needs 2-Step Verification, then Google Account > Security > App
passwords; iCloud: appleid.apple.com; Fastmail: Settings > Privacy & Security;
Outlook.com: Microsoft account > Security), then:

```sh
./bin/bosun credentials set email password \
    --field host=imap.fastmail.com --field user=me@fastmail.com
# prompts for the secret: the app password
```

Optional fields: `smtp_host` (default: `host` with `imap.` replaced by
`smtp.`), `imap_port` (default 993, TLS; 143 uses STARTTLS), `smtp_port`
(default 465 TLS, falling back to 587 with STARTTLS), `email` (From address
when the login is not an address), `name` (From display name), `save_sent`
(`true`/`false`: file a copy of sent mail in Sent; default true, false for
Gmail, which does it itself). Credentials are only ever sent over TLS.

**Google sign-in (Gmail).** `bosun connect google --scopes imap` (add other
bundles as needed, e.g. `--scopes imap,calendar`) links the account with
OAuth; email then uses XOAUTH2 against `imap.gmail.com` / `smtp.gmail.com`.
If both are present, the Google link wins.

- Tools: `email.list`, `email.search` (`from:`, `to:`, `subject:"..."`,
  `since:2026-01-31`, `before:`, `unread`, `flagged`, other words match the
  text), `email.read`, `email.folders` (capability `email.read`, read risk);
  `email.draft` (`email.draft`, write: saves to Drafts, sends nothing);
  `email.archive`, `email.mark` (`email.organize`, write); `email.send`
  (`email.send`, **destructive**: irreversible, so it asks by default).
  Replies (`in_reply_to: <uid>`) get proper `In-Reply-To`/`References`
  headers and default to the sender and "Re: subject".
- Everything containing mail (bodies, subjects, senders, folder names) is
  treated as untrusted data: email is the easiest way for a stranger to put
  instructions in front of the assistant. HTML is converted to text, invisible
  characters are removed, attachments are listed by name and size only, and
  bodies are capped at 50,000 characters. `email.read` does not mark mail as
  read.
- New mail: the poller emits `email.received` (`uid`, `folder`, `from`,
  `subject`, `snippet`, `date`) for each new INBOX message, so a routine with
  `trigger: { event: email.received }` runs for that user. Mail already in the
  INBOX when an account is first seen is not announced, restarts do not
  re-announce, and at most 20 events are emitted per poll.

### Calendar

The `calendar` integration reads and edits each user's own calendars over
CalDAV, so it works with Google Calendar, iCloud, Fastmail, Nextcloud,
Radicale, Baïkal and other CalDAV servers. Enable it in `bosun.yaml` and
restart `bosund`:

```yaml
integrations:
  calendar:
    enabled: true     # backend: caldav (the only one, and the default)
```

Then each user links their own account (accounts are per user and stored in
the vault):

- **Google:** `bosun connect google --scopes caldav` (combinable, e.g.
  `--scopes imap,caldav`). CalDAV needs the full
  `https://www.googleapis.com/auth/calendar` scope, which the `caldav` bundle
  requests.
- **iCloud:** create an app-specific password at appleid.apple.com (Sign-In
  and Security → App-Specific Passwords), then
  `bosun credentials set calendar password --field url=https://caldav.icloud.com --field user=you@icloud.com`
  and paste it at the prompt. Your normal Apple ID password will not work.
- **Fastmail:** an app password with CalDAV access, `--field url=https://caldav.fastmail.com`
  and your Fastmail address as `user`.
- **Nextcloud / Radicale / Baïkal / others:** the server root (discovered via
  `/.well-known/caldav`) or your principal/calendar URL, e.g.
  `--field url=https://cloud.example.com/remote.php/dav --field user=dave`
  (use an app password where the server offers them).
- Optional `--field calendar=Work` picks the default calendar for new events
  (otherwise the first writable one).

Google is used if linked, otherwise the password account. Credentials are only
sent over `https://` (plain `http://` is refused except to localhost), and
only to the configured host's domain.

- Tools: `calendar.calendars`, `calendar.events` (from/to as RFC 3339, a
  local time, a date, or `today`/`tomorrow`/`this week`/`next week`/`this
  month`, in `daemon.timezone` unless a `timezone` is given; recurrences
  expanded, at most 200), `calendar.get`, `calendar.freebusy` (merged busy
  time across writable calendars), all `calendar.read`; `calendar.create` and
  `calendar.update` (`calendar.write`, asks by default; updates use ETags and
  fail with a conflict if the event changed meanwhile); `calendar.delete`
  (`calendar.delete`, destructive).
- Event text is untrusted (anyone can send you an invitation), so the model
  is told never to follow instructions in it.
- Recurrence support: DAILY/WEEKLY/MONTHLY/YEARLY with INTERVAL, COUNT, UNTIL,
  BYDAY (incl. "2nd Tuesday", "last Friday"), BYMONTHDAY, BYMONTH, BYSETPOS,
  RDATE, EXDATE and moved/cancelled instances. Rarer rules (hourly, BYHOUR,
  BYWEEKNO, BYYEARDAY) show only the first occurrence, flagged.

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
- HTTP bearer tokens come from the credential vault: the shared credential
  for integration `mcp` with the configured kind. An admin stores it with
  `bosun credentials set mcp bearer:github` (see [Credentials](#credentials))
  and restarts `bosund`.
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

### Connect your Google account

Integrations that work with your mail, calendar or tasks use an account you
link once with `bosun connect`. A browser opens, you sign in and approve, and
from then on `bosund` keeps a fresh access token for you (refreshed
automatically; the tokens are encrypted in the vault like any credential).

```sh
./bin/bosun connect google                          # default bundles: email,calendar
./bin/bosun connect google --scopes email,calendar,tasks
./bin/bosun connect google --no-browser             # headless: just print the URL
./bin/bosun connect list                            # provider, account, scopes; never tokens
./bin/bosun connect remove google                   # revoke at Google and delete
```

Scope bundles for Google:

| Bundle | Scopes | Why |
| --- | --- | --- |
| `email` | `gmail.readonly`, `gmail.send`, `gmail.modify` | read, send, and archive/label (archiving and labelling need `gmail.modify`) |
| `calendar` | `calendar.events`, `calendar.readonly` | read and edit events |
| `tasks` | `tasks` | Google Tasks |
| `drive` | `drive.readonly` | read Drive files |
| `imap` | `https://mail.google.com/` | Gmail over IMAP/SMTP (XOAUTH2 accepts only this full scope) |
| `caldav` | `calendar` | Google's CalDAV endpoint (needs the full calendar scope) |

Connecting again replaces the stored account for that provider (one account
per provider per user). If you revoke Bosun's access in your Google account,
integrations report that you need to run `bosun connect google` again.

**One-time admin setup (Google Cloud OAuth client):**

1. In the [Google Cloud console](https://console.cloud.google.com/), create
   (or pick) a project.
2. *APIs & Services → Library*: enable the APIs you will use: **Gmail API**,
   **Google Calendar API** (and **CalDAV API** for CalDAV), **Google Tasks
   API**, **Google Drive API**.
3. *APIs & Services → OAuth consent screen*: user type *External* (or
   *Internal* for Workspace), add the scopes above, and while the app is in
   *Testing* add every household member as a test user. (Refresh tokens of
   apps in *Testing* expire after 7 days; publish the app, unverified is fine
   for personal use, to avoid re-connecting weekly.)
4. *APIs & Services → Credentials → Create credentials → OAuth client ID*,
   application type **Desktop app**. Desktop clients accept a loopback
   redirect on any port, so `http://127.0.0.1:<port>/oauth/callback` works
   without registering it.
5. Configure `bosun.yaml` and give `bosund` the client secret through the
   environment (or the vault, see below), then restart `bosund`:

```yaml
oauth:
  google:    { client_id: "1234-abc.apps.googleusercontent.com", client_secret_env: GOOGLE_OAUTH_CLIENT_SECRET }
  microsoft: { client_id: "00000000-0000-0000-0000-000000000000" }   # public client, PKCE only
```

Instead of an environment variable, an admin can keep the secret in the vault:
`./bin/bosun credentials set oauth client_secret:google` (shared; hidden
prompt). Google treats even Desktop app secrets as confidential-ish, so do not
commit them.

**Redirect URIs.** The provider redirects the browser to `bosund` itself, at
`/oauth/callback` on the address the CLI used:

- `http://127.0.0.1:<port>/oauth/callback` for the default loopback daemon
  (works with a Google *Desktop app* client as is);
- `https://<host>:<port>/oauth/callback` when you use Bosun from other devices
  over TLS. Google only allows that with a *Web application* client whose
  authorized redirect URIs list it (and Google requires a real domain name,
  not a bare LAN IP); the simpler path is to run `bosun connect` on the
  machine running `bosund`, through the loopback address.

**Microsoft** (personal, work and school accounts, `common` tenant, v2
endpoints): register an app in *Microsoft Entra ID → App registrations*, add
the *Mobile and desktop applications* platform with redirect URI
`http://127.0.0.1/oauth/callback` (Microsoft ignores the port of loopback
redirect URIs; if the portal refuses `127.0.0.1`, add it in the app manifest;
for LAN use also add `https://<host>:<port>/oauth/callback`), and enable
*Allow public client flows*. No secret is needed. Bundles: `email`
(`Mail.ReadWrite`, `Mail.Send`), `calendar` (`Calendars.ReadWrite`), `tasks`
(`Tasks.ReadWrite`), `drive` (`Files.Read`), all on Microsoft Graph.

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
