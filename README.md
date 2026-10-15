# Bosun

[![CI](https://github.com/perishabledave/bosun/actions/workflows/ci.yml/badge.svg)](https://github.com/perishabledave/bosun/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Bosun is an open-source personal agent that gets real work done, but only
through integrations you've explicitly granted, so it can never do more than
you allowed. It reads and drafts your mail, manages your calendar, keeps notes,
searches the web and runs routines on a schedule, all through a small set of
scoped tools. Every tool has a capability you can allow, deny or require
approval for, every action lands in an audit log, and everything runs on your
own machine or home server with the model of your choice, including fully
local models via Ollama.

> **Status: early, pre-1.0.** Expect breaking changes (config, API, storage)
> before 1.0; [CHANGELOG.md](CHANGELOG.md) calls them out. Tested on macOS and
> Linux (amd64 and arm64).

## Features

- **Permissions you can read.** Every tool maps to a capability
  (`email.read`, `email.send`, `calendar.write`, ...) with a risk level.
  Reads are allowed by default; writes and irreversible actions ask first.
  Change any of it per capability: `allow`, `ask` or `deny`.
- **Approvals anywhere:** inline in `bosun chat`, in the web UI, or with
  Approve/Deny buttons on Telegram.
- **Mail and calendars over open protocols:** IMAP/SMTP and CalDAV (Gmail,
  iCloud, Fastmail, Outlook.com, Yahoo, Nextcloud, self-hosted), read-only ICS
  feeds, and optional Google/Microsoft OAuth. App-password wizards test the
  login before saving it.
- **Notes, web search and fetch,** and any [MCP](https://modelcontextprotocol.io)
  server's tools, each under the same policy and approval gate.
- **A code sandbox:** the assistant can run short Python, Node or shell
  programs in a throwaway container with no network and no access to your
  files.
- **A contained web browser** (opt-in): a private headless Chromium per user
  that can read pages, fill forms and click through sites you allow, while
  anything that books, buys, sends or saves waits for your approval with a
  screenshot of exactly what it will press.
- **Any model:** Anthropic, any OpenAI-compatible API, or local models through
  Ollama. Switching is a config change.
- **Memory** you can list, edit and delete; **routines** that run prompts on a
  schedule or when mail arrives; **hooks** around every model and tool call.
- **Local-first and private:** one SQLite database, an encrypted credential
  vault (master key in the OS keychain where available), an audit log of every
  action and model request, no telemetry.
- **Household-ready:** separate users with their own accounts, memory,
  permissions and routines; invite codes; LAN mode with certificate pinning, or
  Tailscale/Caddy in front.
- **Easy to run:** two static binaries, `bosun setup`, `bosun service install`,
  `bosun doctor`, backups and restores; or a Docker Compose stack.

## Install

**On your own computer** (macOS or Linux), with Homebrew or the install script,
which verifies the download's checksum:

```sh
brew install perishabledave/tap/bosun
# or
curl -fsSL https://raw.githubusercontent.com/perishabledave/bosun/main/install.sh | sh
# add `-s -- --user` to install into ~/.local/bin without sudo
```

Binaries are also on the [releases page](https://github.com/perishabledave/bosun/releases).

**On a homelab server** for a household: the multi-arch Docker image
`ghcr.io/perishabledave/bosun` and a Compose file with optional Ollama,
SearXNG and Caddy in [deploy/docker](deploy/docker).

**[docs/DEPLOY.md](docs/DEPLOY.md)** walks through both end to end: running
as a background service, where files live, upgrades, backups, exposing Bosun
safely via Tailscale or a reverse proxy, resource needs, and a security
checklist. The [Quick start](#quick-start) below is the short version.

## How it compares

Many personal agents get their power from broad access to the machine they run
on: a shell, the file system, a browser they drive. Bosun takes the opposite
trade. It has no shell, no general file access and no browser automation; it
can act only through integrations that declare their capabilities up front,
and each capability is allowed, denied or gated by your approval. That makes it
less able to improvise, and much easier to reason about what it can do to you.
Integrations run in-process but are kept apart by tests: they cannot reach the
database, the vault or each other, and every tool call goes through one
dispatcher that checks policy and writes the audit log. Content from mail, web
pages and other people's calendar invites is treated as untrusted data, never
as instructions. Bosun is local-first (your data, keys and logs stay on your
hardware, with fully local operation via Ollama) and prefers open protocols
(IMAP, SMTP, CalDAV, ICS, MCP) over provider-specific APIs, so it works with
whatever mail and calendar you already use.

## Documentation

- [GOALS.md](GOALS.md): what Bosun is for, its principles, and non-goals
- [ARCHITECTURE.md](ARCHITECTURE.md): how it works, including the security model
- [docs/DEPLOY.md](docs/DEPLOY.md): installing and running it, personal or homelab
- [docs/INTEGRATIONS.md](docs/INTEGRATIONS.md): integration roadmap
- [docs/API.md](docs/API.md): the HTTP API
- [SECURITY.md](SECURITY.md): reporting vulnerabilities, scope, threat model
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup and project rules
- [CHANGELOG.md](CHANGELOG.md)

## Quick start

After installing, `bosun setup` creates your config, data directory and admin
user. To build from source instead (Go 1.26+):

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

Pick a model in `bosun.yaml`. The default is `anthropic/claude-sonnet-5-5`;
for a local model, set `llm.default: ollama/<model>` (Ollama at
`http://localhost:11434`, no key needed). An `openai` provider (any
OpenAI-compatible endpoint; set `base_url`) is also supported.

```sh
./bin/bosun service install   # run bosund in the background at login (or run ./bin/bosund in a terminal)
```

Then:

```sh
./bin/bosun login                                 # stores a session token in <data_dir>/cli-token
./bin/bosun credentials set llm anthropic --shared  # paste the API key at the hidden prompt (admin; stored encrypted)
./bin/bosun doctor                                # checks everything, including that the key works
./bin/bosun tools                          # what the assistant can do, and your policy for each
./bin/bosun chat "hello"                   # one turn; `bosun chat` alone opens a session
```

**LLM API keys.** Store each provider's key in the vault with `bosun
credentials set llm <provider> --shared` (admin only, encrypted like every
other credential). A key stored or rotated there takes effect within 30
seconds, without a restart. As a fallback, a provider's `api_key_env` names an
environment variable to read when the vault has no key (the default config
uses `ANTHROPIC_API_KEY`; set it before starting `bosund`, or in
`<data_dir>/env` for a service). `bosun doctor` shows where each provider's
key comes from.

### Commands

| Command | What it does |
| --- | --- |
| `bosun setup` | create `bosun.yaml`, the data directory and the first admin |
| `bosun service install\|status\|logs\|restart\|stop\|start\|uninstall` | run `bosund` in the background ([details](#running-as-a-service)) |
| `bosun login` / `bosun status` | log in; check that the daemon is running |
| `bosun chat [message]` | talk to your assistant (interactive, or one message) |
| `bosun tools`, `bosun policy` | what the assistant can do; your allow/ask/deny rules |
| `bosun approvals` | list and answer pending approvals |
| `bosun browser status\|stop\|screenshot` | what the assistant's browser is doing; stop it |
| `bosun connect email\|calendar\|google\|microsoft` | link accounts |
| `bosun credentials` | manage secrets in the encrypted vault |
| `bosun memory`, `bosun routines` | what it remembers; scheduled and event-driven prompts |
| `bosun channels`, `bosun mcp`, `bosun hooks` | Telegram links; MCP server status; registered hooks |
| `bosun users`, `bosun accept-invite`, `bosun passwd` | household users and passwords |
| `bosun doctor`, `bosun version`, `bosun config check` | health checks and versions |
| `bosun backup`, `bosun restore` | back up and restore config, database and keys |
| `bosun completion bash\|zsh\|fish` | shell completion |

Every command has `--help`.

## Using Bosun

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

Some tools act on a site (the browser's). Their rules can be limited to one
site, which beats the capability-wide rule:

```sh
./bin/bosun policy set browser.act allow --scope opentable.com   # only there
./bin/bosun policy unset browser.act --scope opentable.com
```

Approvals for such tools also offer **Approve and always allow on <site>**
(web UI, Telegram, `a` in `bosun chat`, or
`bosun approvals <id> approve --always`), which creates that rule for you.

### Web UI

`bosund` also serves a small browser UI at its listen address (by default
<http://127.0.0.1:7777/>): sign in with your Bosun username and password, chat
with streamed replies and tool calls, approve or deny pending approvals (also
those raised by routines in other conversations), set allow/ask/deny per tool and see your linked accounts (connecting them is done with `bosun connect`),
and browse, add, edit and delete your memories. It is plain HTML/JS embedded in
the binary (no build step, nothing loaded from the network) and follows your
light/dark setting. The token lives in `sessionStorage`, so closing the tab
signs you out. Set `daemon.web_ui: false` in `bosun.yaml` to serve the API only.

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
`bearer:<suffix>`); for `llm`, a configured provider name (`anthropic`,
`openai`, ...). Every change is audited without the secret.

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
the vault (`bosun doctor` warns while it is there). Do not switch to `file` after a key was generated in the keychain:
the file would hold a different key.

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
Email address: you@icloud.com
Provider: iCloud Mail (IMAP imap.mail.me.com:993, SMTP smtp.mail.me.com:587 STARTTLS)

iCloud Mail needs an app password (your normal password will not work). To create one:
  1. Open https://appleid.apple.com and sign in (two-factor authentication must be on).
  2. Go to Sign-In and Security → App-Specific Passwords and click + (Generate).
  3. Name it "Bosun" and copy the password (format xxxx-xxxx-xxxx-xxxx).

App password for you@icloud.com:            ← hidden
Testing the connection (IMAP login and SMTP authentication)...
Connection OK.
Saved. Bosun can now read and send mail as you@icloud.com (by default, sending asks you first).

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
pass show fastmail-bosun | ./bin/bosun connect email --user you@fastmail.com --password-stdin
./bin/bosun connect email --user me@example.org --password-stdin \
    --host imap.example.org --smtp-host smtp.example.org --smtp-port 587 < pw.txt
```

`--provider gmail|icloud|fastmail|outlook|yahoo|other` overrides the preset
picked from the address, and `--imap-port` / `--smtp-port` the ports.

<details><summary>Storing the credential by hand</summary>

The wizard stores a `password` credential, which you can also write directly:

```sh
./bin/bosun credentials set email password \
    --field host=imap.fastmail.com --field user=you@fastmail.com
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

### Files

The `files` integration gives the assistant access to **only the folders you
list**, as the OS user running `bosund`. It is off by default (`bosun setup`
leaves it alone); enable it and restart:

```yaml
integrations:
  files:
    enabled: true
    roots:
      - path: ~/Documents/Shared   # ~ is expanded for the daemon user
        name: shared               # how the model refers to it: shared/report.md
        writable: true
      - path: /srv/media
        name: media
        writable: false
    max_read: 2MiB                 # per files.read call, and the largest file searched
    extensions: []                 # empty = any; else e.g. [.md, .txt, .csv, .json, .pdf]
    include_hidden: false          # true allows dotfiles such as .git and .env
```

- Tools: `files.roots`, `files.list`, `files.search`, `files.read`,
  `files.stat` (read risk, allowed by default); `files.write`, `files.append`,
  `files.move`, `files.mkdir` (write risk, ask by default); `files.delete`
  (destructive, asks by default; files only, never folders). Capabilities are
  `files.read`, `files.write` and `files.delete`.
- Every path is `<root name>/<relative path>`. Absolute paths, `..`, hidden
  entries, over-long names and symlinks that resolve outside the root are
  refused; recursive list and search never follow symlinks. Errors never show
  host paths. Writes need `writable: true`, are atomic (temp file then rename)
  and refuse to replace an existing file unless the call says `overwrite: true`.
  Moves stay inside one root. Files over 16 MiB are not written.
- `files.read` returns UTF-8 text (invalid bytes replaced), capped at
  `max_read` per call with an offset to continue. Binary files and images
  return only type, size and (for images) dimensions. PDFs are not parsed by
  Bosun; the read says so and points at the sandbox integration.
- **Household caveat:** there is one filesystem for the whole household, not
  per-user roots. Everyone who can talk to the assistant can use every root,
  and file contents are treated as untrusted data because another household
  member (or anything syncing into those folders) may have written them. Point
  roots only at folders you are happy to share with all users.

### Browser

The `browser` integration gives your assistant its own web browser: a private,
headless Chromium per user, with its own profile (sign-ins you make there
stay there), separate from your normal browser. Off by default:

```yaml
integrations:
  browser:
    enabled: true
    # chrome_path: auto-detected: $CHROME_PATH, Google Chrome or Chromium in
    #   the usual places (/Applications/Google Chrome.app, /usr/bin/chromium, ...)
    # headless: true
    # step_budget: 60          # browser steps per task
    # task_timeout: 10m
    # idle_timeout: 10m        # stop an unused browser after this long
    # screenshot_every_step: true
    # sensitive_sites: [mybank.example]   # added to the built-in list
    # allow_private: false     # let it reach your LAN (off: public sites only)
    # no_sandbox: false        # needed in most containers (see docs/DEPLOY.md)
```

**What it can do.** Open pages and read them (as text, with numbered element
refs), find elements, click links and buttons, type into fields, choose from
drop-downs, scroll, go back, take screenshots, fill a saved login, and press
the button that completes something (book, buy, send, save) once you approve.
The **Browser** page of the web UI shows its current view live, with a Stop
button; `bosun browser stop` does the same.

| Tool | Capability | Risk | Scoped to the site |
|---|---|---|---|
| `browser.open`, `browser.read`, `browser.find`, `browser.screenshot` | `browser.read` | read (allowed by default) | `open`: its URL's site |
| `browser.click`, `browser.type`, `browser.select`, `browser.scroll`, `browser.back` | `browser.act` | write (asks by default) | yes |
| `browser.login` | `browser.login` | write | yes |
| `browser.submit` | `browser.submit` | destructive (asks) | yes |

**Granting a site.** Acting on pages asks by default. The first time the
assistant clicks around a site you trust, answer **Approve and always allow on
opentable.com** and later clicks, typing and scrolling there run without
asking; other sites still ask. You can also set it up front:
`bosun policy set browser.act allow --scope opentable.com`.

**What always asks.** Anything that changes your account goes through
`browser.submit`: `browser.click` refuses submit buttons and anything labelled
buy, pay, order, checkout, confirm, book, reserve, send, post, publish, delete,
remove, cancel, unsubscribe, save, update, change, apply, submit, sign up,
register or transfer, and pressing Enter will not submit a form that posts.
The approval shows a screenshot with the button highlighted, its label, and
the values of the form it submits (passwords masked); if the page changes
before you answer, nothing is pressed. You *can* allow `browser.submit` on a
site, but don't: page content is untrusted, and a page (or an email the
assistant read earlier) can try to talk the assistant into submitting
something you did not ask for. That approval is what stops it. On **sensitive
sites** (banks, brokerages, PayPal, Google and Apple accounts, password
managers, the IRS, plus your `sensitive_sites`) every browser action asks,
whatever your rules say.

**Logins.** Save a site's login in the vault and the assistant can fill it in
without ever seeing it:

```sh
bosun credentials set browser login:opentable.com --field username=you@example.com
# prompts for the password
```

`browser.login` only fills it on an https page of that site (or a
subdomain), and never submits: signing in is a `browser.submit` you approve.

**What it never does.** Download files, upload files, fill payment details,
visit non-web URLs (`file:`, `chrome:`), or reach your private network (unless
`allow_private`). To pay, or to finish any step you would rather do yourself,
deny the request and complete it on the site in your own browser. Each step's
screenshot is kept under `<data_dir>/integrations/browser/steps/` (last 200
per conversation) and referenced from the audit log.

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

### Sandbox (run code)

The `sandbox` integration lets the assistant write and run short Python,
Node.js or shell programs: crunch a CSV you pasted, convert a file, check a
calculation, generate a chart. Each run happens in a fresh, throwaway Docker
container that cannot reach the network or your files. It is not a shell on
your machine: the code never runs on the host. `bosun setup` leaves it off; it
needs Docker (Docker Desktop on macOS/Windows, Docker Engine on Linux)
reachable by the user `bosund` runs as. Enable it and restart `bosund`:

```yaml
integrations:
  sandbox:
    enabled: true
    backend: docker          # "none" disables it
    images:                  # defaults shown; one pinned image per language
      python: python:3.12-alpine
      node: node:22-alpine
      shell: alpine:3.22
    timeout: 60s             # per run, at most 300s
    memory: 512m             # swap disabled
    work_size: 64m           # the /work tmpfs (counts toward memory)
    max_concurrent: 2        # further runs fail fast with "busy"
    pull: auto               # pull a missing image on first use; "never" to forbid
```

- One tool, `sandbox.run` {`language`: `python`|`node`|`shell`, `code`,
  `files` (name → text, each ≤ 1 MiB, ≤ 8 MiB total, appear as `./in/<name>`),
  `args`, `stdin`}. It returns the exit code, stdout and stderr (64 KiB each),
  and any files the program wrote to `./out/` (up to 20 files, 8 MiB; text
  files are shown, binary files are returned base64 in the structured result).
  Output is treated as untrusted data, since the code may have processed a
  fetched page or an email.
- Capability `sandbox.run`, risk **write**, so every run asks for approval by
  default and you see the code first. Because of the containment below,
  allowing it unattended is reasonable once you are comfortable:
  `bosun policy set sandbox.run allow`. Short of a container-escape bug in
  the kernel or Docker, the worst a run can do is use one CPU and `memory`
  of RAM for up to `timeout`, and return wrong output.
- Containment, per run: `docker run --rm --network none --read-only` with a
  tmpfs at `/work` as the only writable place, no host paths mounted (code and
  inputs go in on stdin as a tar, results come back on stdout),
  `--user 65534:65534` (nobody), `--cap-drop ALL`,
  `--security-opt no-new-privileges`, `--memory 512m --memory-swap 512m`,
  `--cpus 1`, `--pids-limit 256`, and a hard timeout after which the container
  is force-removed. Containers are labelled `bosun.sandbox=1`; anything left
  behind by a crash is removed when `bosund` starts or stops.
- **First use pulls the image** (`pull: auto`): about 20 MB for Alpine
  Python and 50 MB for Node; the first call may take a while, and if it
  outlasts the tool call the pull continues in the background so the next
  call works. Pre-pull to avoid the wait:
  `docker pull python:3.12-alpine node:22-alpine alpine:3.22`. With
  `pull: never` a missing image is an error. `bosun doctor` and the
  integrations page show whether Docker is reachable and which images are
  present.
- **Custom images.** Only each language's standard library is available
  (there is no network, so `pip install` cannot work inside a run). For extra
  packages, build an image and point `images.<language>` at it. Any image
  works if it has `sh` and `tar` (Alpine/BusyBox or Debian-based both do) and
  the interpreter (`python3`, `node`, `sh`) on `PATH`; it runs as uid 65534
  with a read-only root, so install packages system-wide at build time:

  ```dockerfile
  # docker build -t local/bosun-python-data:1 .
  FROM python:3.12-slim
  RUN pip install --no-cache-dir pandas matplotlib
  ```

  ```yaml
  integrations:
    sandbox:
      images: { python: local/bosun-python-data:1 }
      pull: never        # a local-only tag cannot be pulled
  ```

- Limits to know: the dispatcher bounds every tool call at 60 seconds, so a
  run's effective limit is the smaller of `timeout` and what is left of that
  (a run times out a few seconds early rather than being cut off). Docker
  access is root-equivalent on the host for whoever has it: that is `bosund`,
  never the model, which only chooses the code. In the Docker Compose setup
  the sandbox needs the host's Docker socket; see
  [docs/DEPLOY.md](docs/DEPLOY.md#code-sandbox-in-docker-compose) and prefer a
  personal-machine install if you want it.

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

### Audit log

Every tool call (with its policy decision and outcome), approval, policy
change, and model request (provider, model, token counts) is appended to the
`audit_log` table in `<data_dir>/bosun.db` (SQLite). Message content and tool
arguments are redacted there unless `audit.log_content: true`.

```sh
sqlite3 ~/.bosun/bosun.db "select ts, kind, tool, decision, error from audit_log order by id desc limit 20"
```

## Running Bosun

See also [docs/DEPLOY.md](docs/DEPLOY.md) for complete walkthroughs.

### Running as a service

`bosun service` keeps `bosund` running in the background and starts it at login:

```sh
bosun service install    # macOS: ~/Library/LaunchAgents/com.github.perishabledave.bosun.plist (launchd user agent)
                         # Linux: ~/.config/systemd/user/bosund.service (systemd --user)
bosun service status     # running/pid, plus the daemon's health and version
bosun service logs       # follows <data_dir>/logs/bosund.log (macOS) or journalctl --user -u bosund
bosun service restart    # e.g. after editing bosun.yaml
bosun service stop | start | uninstall
```

`install` finds `bosund` next to `bosun` (or on `$PATH`; `--bosund` overrides)
and runs it with the absolute `--config` path. On macOS the agent has
`RunAtLoad` and `KeepAlive`, and logs to `<data_dir>/logs/bosund.log`. On
Linux the user unit uses `Restart=on-failure`; run `sudo loginctl
enable-linger $USER` once so it keeps running after you log out and starts at
boot. `--system` writes `/etc/systemd/system/bosund.service` running as a
dedicated `bosun` user instead (run it with sudo; `bosun` prints the
`useradd` command and never runs sudo itself); `--print` shows the unit
without installing it.

**Secrets never go in unit files.** `bosund` reads `<data_dir>/env` at
startup: optional `KEY=VALUE` lines (blank lines and `#` comments allowed)
added to its environment, without overriding variables already set. The file
must be mode 0600 and owned by the user running `bosund`, or the daemon
refuses to start. Prefer the vault for LLM keys (`bosun credentials set llm
anthropic`); use `env` for the rest (`api_key_env`, `client_secret_env`, a
Telegram `token_env`).

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

### Behind a reverse proxy (Tailscale Serve, Caddy)

Instead of LAN mode's self-signed certificate, you can put `bosund` behind a
proxy that terminates TLS and keep it on plain HTTP locally:

```yaml
daemon:
  listen: 127.0.0.1:7777       # the proxy runs on the same machine
  behind_proxy: true           # allows a non-loopback listen without tls; never with tls.enabled
  trusted_proxies: [127.0.0.1] # CIDRs or IPs whose X-Forwarded-* headers are believed
```

With `behind_proxy`, `X-Forwarded-For`, `X-Forwarded-Proto` and
`X-Forwarded-Host` are used (for the client address in logs, and for the OAuth
redirect URL `bosun connect` registers) **only** on connections from
`trusted_proxies`; from anyone else, and whenever `behind_proxy` is off, they
are ignored. Without `trusted_proxies`, loopback and the private ranges are
trusted and `bosund` warns at startup. `bosun doctor` flags forwarded headers
arriving from untrusted addresses and a non-loopback listen behind a proxy.

Tailscale Serve (HTTPS on your tailnet, with a real certificate):

```sh
tailscale serve --bg --https=443 http://127.0.0.1:7777
bosun --server https://<machine>.<tailnet>.ts.net login
```

Caddy (`Caddyfile`; Caddy sets `X-Forwarded-*` itself):

```
bosun.example.com {
	reverse_proxy 127.0.0.1:7777
}
```

Server-Sent Events (chat streaming) need the proxy not to buffer responses;
both of these stream by default. See [docs/DEPLOY.md](docs/DEPLOY.md) for
packaged installs.

### Health checks, backups and versions

```sh
bosun doctor             # ok / warn / fail per check; exits non-zero on any failure
bosun version            # CLI and daemon versions
bosun config check       # load and validate a config file (--config) without a daemon
```

`bosun doctor` checks that the config loads, the data dir is 0700, the vault
master key is readable, the daemon answers (and runs the same version), each
LLM provider's key source and a cheap reachability probe (Anthropic and
OpenAI list models; Ollama `/api/tags`), every integration's and channel's
status, the TLS certificate's expiry and fingerprint, the routine scheduler,
free disk space, and the reverse-proxy settings. Logged in as an admin, the
daemon runs its side (`GET /v1/doctor`), so it also works against a remote
daemon; with the daemon down, the local checks still run.

```sh
bosun backup --out bosun-backup.tar.gz    # safe while bosund runs
bosun restore bosun-backup.tar.gz         # stop bosund first
```

A backup (mode 0600) holds `bosun.yaml`, a consistent snapshot of the database
(`VACUUM INTO`, not a copy of the live WAL files), and the data dir's files:
`master.key` when the vault key is file-based, `env`, `integrations/` state
and `tls/`. The CLI session token and logs are left out. **If the master key is
in the OS keychain, it is not included** (you get a warning): credentials in
the backup then decrypt only where the keychain holds the same key.
`--include-keychain-key` exports it into the archive; anyone holding that
archive can read every stored credential, so keep it like a password.

`restore` reads the archive's manifest first and refuses a database newer than
the binary's migrations (upgrade bosun first). It refuses to run while the
daemon answers, and refuses a non-empty data dir or an existing config unless
`--force`, which moves them aside (`<dir>.before-restore-<time>`) rather than
deleting them. `--data-dir` restores somewhere else and rewrites the config to
match. Log in again afterwards.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for the setup and the rules the tests
enforce.

```sh
make build              # bin/bosund and bin/bosun
make test               # go test ./...
make lint               # gofmt, go vet, integration import guard, dispatcher choke-point test
make check              # lint + go test -race (what CI runs)
make docker             # build the container image from source (bosun:dev)
make release-snapshot   # all release archives, checksums, formula and images into dist/, nothing published
```

Releases are cut by pushing a `v*` tag; `.github/workflows/release.yml` runs
the tests, then GoReleaser (`.goreleaser.yaml`).

Dependencies are kept minimal: `modernc.org/sqlite` (pure Go, no cgo),
`gopkg.in/yaml.v3`, `golang.org/x/crypto` (argon2id), `golang.org/x/term`
(password prompts), and `github.com/spf13/cobra` for the CLI. Their licenses
are listed in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).

## License

MIT; see [LICENSE](LICENSE). Security reports: [SECURITY.md](SECURITY.md).
