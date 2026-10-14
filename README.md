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
and the web UI, plus `email` (IMAP/SMTP) and `calendar` (CalDAV: iCloud,
Fastmail, Google, Nextcloud, ...; read-only ICS feeds for Google Calendar,
Outlook.com and others), which `bosun setup` enables and links with app
passwords via `bosun connect email`.

## Quick start

Install the two binaries, `bosund` (the daemon) and `bosun` (the CLI), with
Homebrew (`brew install perishabledave/tap/bosun`) or the install script
(`curl -fsSL https://github.com/perishabledave/bosun/releases/latest/download/install.sh | sh`,
add `-s -- --user` to install into `~/.local/bin` without sudo; it verifies
the download's checksum), then run `bosun setup`. For a household server
there is a Docker image (`ghcr.io/perishabledave/bosun`, amd64/arm64) and a
Compose file with optional Ollama, SearXNG and Caddy in
[deploy/docker](deploy/docker). **[docs/DEPLOY.md](docs/DEPLOY.md)** walks
through both setups end to end: running as a background service, where files
live, upgrades, backups, exposing Bosun safely via Tailscale or a reverse
proxy, resource needs, and a security checklist.

To build from source instead (Go 1.26+):

```sh
make build                    # builds bin/bosund and bin/bosun
./bin/bosun setup             # writes ~/.bosun/bosun.yaml, creates the data dir and the first admin
```

The examples below use `./bin/bosun`; with an installed release, use `bosun`.

`bosun setup` prompts for LAN access, the listen address, data directory, admin
username and password; `--lan`, `--listen`, `--data-dir` and `--admin` skip
those prompts (see [LAN](#using-bosun-from-other-devices-lan)). It then offers
to connect your email (and calendar) with an app password, testing the login
before saving it (`--no-connect` skips that; see [Email](#email)). Use
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
those raised by routines in other conversations), set allow/ask/deny per tool and see your linked accounts (connecting them is done with `bosun connect`),
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
    per_user: true      # default: each user's notes live in ~/Notes/u<user id>/
```

- Capabilities: `notes.read` (list/search/read; read risk), `notes.write`
  (write/append; write risk, asks by default), `notes.delete` (destructive).
- With `per_user: true` users never see each other's notes. Folders are keyed
  by numeric user ID (`u1`, `u2`, ...), not username, so a new account that
  reuses a deleted user's name cannot inherit their notes. A pre-existing
  `<dir>/<username>/` folder is renamed to `<dir>/u<id>/` the first time that
  user's notes are used (logged). Removing a user never deletes their notes on
  disk; the folder stays as an orphan you can archive or delete by hand. With
  `per_user: false` everyone shares `dir`, and note contents are treated as
  untrusted data (another household member could have written instructions
  into a note).
- Paths are relative to the notes folder. Absolute paths, `..`, hidden
  files/folders (`.git`, `.obsidian`), other extensions, and symlinks that
  point outside the folder are refused. Reads return at most 200 KB.

### Email

The `email` integration reads, searches, drafts, sends, archives and flags
mail over IMAP and SMTP, so it works with Gmail, iCloud, Fastmail,
Outlook.com, Yahoo and self-hosted servers. `bosun setup` enables it (and
`calendar`) in the generated `bosun.yaml`; with no account linked its tools
just answer "not configured". For an older config, add it and restart
`bosund`:

```yaml
integrations:
  email:
    enabled: true
    poll_interval: 2m   # check INBOXes for new mail (email.received events); "off" disables
```

Accounts are per user and stored in the encrypted vault. The easy way to link
one is the wizard (`bosun setup` offers it right after creating the admin;
everyone else runs it after `bosun login`):

```sh
./bin/bosun connect email
```

```
Email address: dave@icloud.com
Provider: iCloud Mail (IMAP imap.mail.me.com:993, SMTP smtp.mail.me.com:587 STARTTLS)

iCloud Mail needs an app password (your normal password will not work). To create one:
  1. Open https://appleid.apple.com and sign in (two-factor authentication must be on).
  2. Go to Sign-In and Security → App-Specific Passwords and click + (Generate).
  3. Name it "Bosun" and copy the password (format xxxx-xxxx-xxxx-xxxx).

App password for dave@icloud.com:            ← hidden
Testing the connection (IMAP login and SMTP authentication)...
Connection OK.
Saved. Bosun can now read and send mail as dave@icloud.com (by default, sending asks you first).

iCloud Calendar works with the same app password. Set up your calendar too? [Y/n]:
Testing CalDAV at https://caldav.icloud.com...
Saved. Bosun can now read your calendars and add or change events (by default, changes ask you first).
```

It works out the servers from your address, shows where to create an **app
password** (a separate password just for Bosun, which you can revoke at any
time; no Google Cloud project or OAuth setup needed), **tests the login**
(IMAP and SMTP, through the daemon) and only then stores it. A wrong password
gets a clear message and another try; Gmail's hint explains the 2-Step
Verification requirement. Then it offers the calendar: with the same password
for iCloud and Fastmail, or as a read-only feed for Gmail, Outlook.com and
Yahoo (see [Calendar](#calendar)).

| Provider | Servers | App password |
| --- | --- | --- |
| Gmail | `imap.gmail.com`, `smtp.gmail.com:465` | needs 2-Step Verification, then <https://myaccount.google.com/apppasswords> |
| iCloud (`icloud.com`, `me.com`, `mac.com`) | `imap.mail.me.com`, `smtp.mail.me.com:587` | <https://appleid.apple.com> → Sign-In and Security → App-Specific Passwords |
| Fastmail | `imap.fastmail.com`, `smtp.fastmail.com:465` | Settings → Privacy & Security → Integrations → App passwords |
| Outlook.com / Hotmail / Live | `outlook.office365.com`, `smtp-mail.outlook.com:587` | Microsoft account → Security → App passwords (see the note below) |
| Yahoo | `imap.mail.yahoo.com`, `smtp.mail.yahoo.com:465` | Account Security → Generate app password |
| anything else | found by trying `imap.`/`smtp.`/`mail.<domain>`, or asked | your provider's app password, if it has them |

Note on Outlook.com: Microsoft is phasing out password ("basic")
authentication for IMAP/SMTP on personal accounts. Some accounts still accept
an app password; if the test fails with a correct one, that account needs
OAuth, which the email integration does not support for Microsoft yet.

**Which email provider?** If you are choosing, **Fastmail** or **iCloud**
give you mail *and* a read/write calendar with one app password and no OAuth
setup at all. Gmail works just as easily for mail; its calendar is read-only
through the private iCal link unless you set up OAuth.

For scripts (no questions; the password comes from stdin):

```sh
pass show fastmail-bosun | ./bin/bosun connect email --user me@fastmail.com --password-stdin
./bin/bosun connect email --user me@example.org --password-stdin \
    --host imap.example.org --smtp-host smtp.example.org --smtp-port 587 < pw.txt
```

`--provider gmail|icloud|fastmail|outlook|yahoo|other` overrides the preset
picked from the address, and `--imap-port` / `--smtp-port` the ports.

<details><summary>Storing the credential by hand</summary>

The wizard stores a `password` credential, which you can also write directly:

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
</details>

**Advanced: Google sign-in (Gmail over OAuth).** `bosun connect google
--scopes imap` (combinable, e.g. `--scopes imap,caldav`) links the account
with OAuth; email then uses XOAUTH2 against `imap.gmail.com` /
`smtp.gmail.com`. This needs your own Google Cloud OAuth client (see
[Connect your Google account](#connect-your-google-account)). If both are
present, the Google link wins.

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
CalDAV (iCloud, Fastmail, Google, Nextcloud, Radicale, Baïkal, ...) and
reads any calendar that has a private iCal (ICS) link. Like `email`, it is
enabled by `bosun setup`; for an older config:

```yaml
integrations:
  calendar:
    enabled: true     # backend: caldav (the default; ICS feeds need no setting)
```

Each user links their own calendars (stored in the vault), the easy way
first:

- **iCloud and Fastmail (read and write):** `bosun connect email` offers the
  calendar right after the mail account, with the same app password.
- **Google Calendar, Outlook.com, Yahoo, or any calendar with a private link
  (read-only):**

  ```sh
  ./bin/bosun connect calendar --ics                 # hidden prompt for the link
  ./bin/bosun connect calendar --ics --name family   # several feeds: family, work, ...
  ```

  Where to find the link: **Google Calendar** → Settings → your calendar →
  Integrate calendar → *Secret address in iCal format*; **Outlook.com** →
  Settings → Calendar → Shared calendars → *Publish a calendar* → ICS link;
  **iCloud** → share the calendar as a *Public Calendar* (`webcal://` links
  work); Yahoo → calendar → Share → ICS link. The link is a secret (anyone with
  it can read the calendar), so it is asked for hidden and stored in the
  vault; the daemon test-fetches it before saving. Each `--name` becomes a
  separate calendar (`ics:<name>` credential) and `calendar.events` merges
  them, labelled by name.
- **Advanced, Google Calendar read and write:** `bosun connect google --scopes
  caldav` (combinable, e.g. `--scopes imap,caldav`), which needs your own
  Google Cloud OAuth client ([setup](#connect-your-google-account)). CalDAV
  needs the full `https://www.googleapis.com/auth/calendar` scope, which the
  `caldav` bundle requests.
- **Nextcloud / Radicale / Baïkal / other CalDAV servers:**
  `bosun credentials set calendar password --field url=https://cloud.example.com/remote.php/dav --field user=dave`
  (the server root is discovered via `/.well-known/caldav`; paste an app
  password where the server offers them). The same works for iCloud
  (`url=https://caldav.icloud.com`) and Fastmail (`url=https://caldav.fastmail.com`).
  Optional `--field calendar=Work` picks the default calendar for new events
  (otherwise the first writable one).

If a user has several, Google OAuth is used first, then the CalDAV password
account, then the ICS feeds. CalDAV credentials are only sent over `https://`
(plain `http://` is refused except to localhost), and only to the configured
host's domain. ICS feeds are fetched over `https://` only (`webcal://` is
rewritten), never from private or loopback addresses, at most 5 MB, and
cached per user for five minutes; their calendars are read-only, so
`calendar.create`/`update`/`delete` explain how to link an editable account.

- Tools: `calendar.calendars`, `calendar.events` (from/to as RFC 3339, a
  local time, a date, or `today`/`tomorrow`/`this week`/`next week`/`this
  month`, in `daemon.timezone` unless a `timezone` is given; recurrences
  expanded, at most 200), `calendar.get`, `calendar.freebusy` (merged busy
  time across your own calendars: writable ones, or the ICS feeds), all
  `calendar.read`; `calendar.create` and `calendar.update` (`calendar.write`,
  asks by default; updates use ETags and fail with a conflict if the event
  changed meanwhile); `calendar.delete` (`calendar.delete`, destructive).
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

### Text your assistant on Telegram

`bosund` can run a Telegram bot so you can chat with your assistant, answer
approvals and get routine results from your phone. It long-polls Telegram
(`getUpdates`), so it needs no public URL, port forwarding or webhook.

1. In Telegram, talk to [@BotFather](https://t.me/BotFather): send `/newbot`,
   pick a name and a username, and copy the token it gives you.
2. Enable the channel in `bosun.yaml` and restart `bosund`:

   ```yaml
   channels:
     telegram:
       enabled: true
       allowed_usernames: [your_telegram_username]   # optional allowlist for linking
       # token_env: BOSUN_TELEGRAM_TOKEN             # alternative to the vault
   ```

3. Store the token (admin, kept encrypted in the vault; the bot picks it up
   within 30 seconds, no restart needed):

   ```sh
   ./bin/bosun credentials set channels telegram_bot_token --shared
   ```

4. Link your chat. On your computer:

   ```sh
   ./bin/bosun channels link telegram
   # Link code: K7QX2M
   # Send /link K7QX2M to @my_bosun_bot on Telegram.
   ```

   Open a private chat with your bot and send `/link K7QX2M`. Codes are
   single-use and expire after 10 minutes.

Then just send messages. Replies stream into one message; tool calls show as
`⚙ notes.read`; anything that needs approval arrives with **Approve** /
**Deny** buttons. Approvals raised elsewhere (the web UI, a routine waiting at
7am) are pushed to your linked chats too, and finished routines post their
reply. Bot commands: `/new` (fresh conversation), `/approvals`, `/whoami`,
`/unlink`, `/help`. Each chat talks to one long-lived conversation titled
`telegram`, also visible in the web UI and `bosun chat`.

```sh
./bin/bosun channels list                       # your linked chats
./bin/bosun channels unlink telegram 123456789  # cut one off
```

Safety and privacy:

- A chat acts as you only after it redeemed a code you generated. Messages
  from unlinked chats get one "how to link" reply per hour and never reach the
  agent; group chats are ignored and cannot be linked. Every update's sender
  must be the linked private chat. Five wrong codes from a chat lock it out of
  linking for an hour.
- A linked chat has your permissions, like the CLI: protect your Telegram
  account (enable two-step verification) and use `allowed_usernames`.
- Telegram bot chats are not end-to-end encrypted: your messages, the
  assistant's replies and approval details pass through Telegram's servers.
  The daemon logs chat IDs, never message text.

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

This is the advanced option. For mail and calendars, `bosun connect email`
(app passwords) and `bosun connect calendar --ics` (private calendar links)
need no OAuth setup at all; see [Email](#email) and [Calendar](#calendar).
OAuth is for Google Calendar *write* access over CalDAV, Gmail without an app
password, and Google/Microsoft APIs (Tasks, Drive, Graph).

Integrations that work with your mail, calendar or tasks can use an account you
link once with `bosun connect <provider>`. A browser opens, you sign in and approve, and
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

### Household users

`bosun setup` creates the first admin. The admin adds everyone else; each new
member gets a one-time invite code (valid 24 hours) and chooses their own
password, so nobody has to share one:

```sh
bosun users add alice                 # prints the code and the exact command for alice
bosun users add bob --role admin      # a second admin
```

Alice runs the printed command on her own computer (it includes `--server`
and the certificate pin when the daemon is on the LAN), or opens the web UI
and picks *Have an invite code?* on the sign-in page:

```sh
bosun accept-invite bsi_… --server https://myhost:7777 --insecure-fingerprint 3A:7F:…:C2
```

Each person has their own conversations, memory, credentials, permissions
and routines. `bosun passwd` changes your own password (your other sessions
are signed out). Admins manage users with `bosun users list`,
`disable`/`enable <name>` (signs them out, keeps their data),
`reset-password <name>` (new invite code), `set-role <name> admin|member`
and `remove <name>` (deletes the user and all their data; the audit log is
kept), or on the web UI's **Users** page. The last enabled admin cannot be
demoted, disabled or removed.

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
make docker             # build the container image from source (bosun:dev)
make release-snapshot   # all release archives, checksums, formula and images into dist/, nothing published
```

Releases are cut by pushing a `v*` tag; `.github/workflows/release.yml` runs
GoReleaser (`.goreleaser.yaml`).

Dependencies are kept minimal: `modernc.org/sqlite` (pure Go, no cgo),
`gopkg.in/yaml.v3`, `golang.org/x/crypto` (argon2id), `golang.org/x/term`
(password prompts), and `github.com/spf13/cobra` for the CLI.

## License

MIT; see [LICENSE](LICENSE).
