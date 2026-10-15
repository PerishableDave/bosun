# Changelog

All notable changes to Bosun are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0,
minor versions may contain breaking changes; they are called out under
**Changed** or **Removed**.

## [Unreleased]

Nothing yet.

## [0.3.0] - 2026-10-15

### Added

- **Web UI overhaul** (see `docs/WEB_UI.md`). Conversations are first-class:
  grouped sidebar (Pinned / Today / Yesterday / Previous 7 days / Older),
  search (Cmd/Ctrl+K), rename, pin, archive with undo, delete with confirm,
  Markdown export, source badges for routine and Telegram chats, an unread
  dot for routine replies, live updates across tabs. Chat: streaming caret,
  Stop, per-conversation drafts, Copy, "Jump to latest", Retry, folded tool
  calls with details, approval cards with previews and "always allow on
  <site>", a full Markdown renderer (tables, code with copy, lists, links).
  App shell: collapsible sidebar (Cmd/Ctrl+B), phone drawer, Settings
  (theme, password, shortcuts), toasts, reconnecting event stream, focus
  rings and reduced-motion support.
- **Conversation API**: `GET /v1/conversations` with `archived`, `q`,
  `limit`, `before` cursor and the full conversation object (`title_source`,
  `source`, `pinned`, `archived`, `unread`, `message_count`,
  `last_message_at`, `last_message_preview`); `PATCH`, `DELETE`,
  `POST .../stop`, `POST .../read`, `GET .../export?format=markdown|json`;
  SSE `conversation_updated` and `conversation_deleted`; `done` may carry
  `stopped: true`.
- **Automatic titles**: a provisional title from the first message, then a
  ≤6-word title from the extraction model; titles set by the user are never
  overwritten.
- CLI: `bosun conversations list|rename|pin|unpin|archive|unarchive|delete|export`.

### Changed

- `GET /v1/conversations` now returns unarchived conversations only, 50 per
  page, pinned first then newest activity; pass `archived=all` for the old
  behavior.

## [0.2.0] - 2026-10-14

### Added

- **Browser integration** (`integrations.browser`). A private headless
  Chromium per user, driven over the DevTools protocol (chromedp, through a
  pipe), with its own profile, a minimal environment and an SSRF-guarded
  proxy. Tools to open, read (text with element refs), find, click, type,
  select, scroll, go back, submit, fill a saved login and take screenshots.
  A submit gate refuses state-changing clicks; `browser.submit` is always
  asked by default and its approval shows a screenshot with the element
  highlighted and the form's values. Sensitive sites (banks, brokerages,
  identity providers) ask for every action. Web UI Browser page,
  `bosun browser status|stop|screenshot`, `/v1/browser` endpoints, and a
  `-browser` Docker image variant.
- **Site-scoped policy rules.** A rule can be limited to one site
  (`bosun policy set browser.act allow --scope opentable.com`,
  `PUT /v1/policy/{capability}?scope=`); scoped rule > capability rule >
  integration default > risk default. Approvals of scoped calls offer
  "Approve and always allow on <site>" in the web UI, the CLI and Telegram.
- **Integration default decisions.** An integration's manifest can declare a
  default decision per capability (`DefaultDecisions`), used instead of the
  risk default when the user has no rule. The browser declares
  `browser.read: ask`: a visit can carry data out in its URL, so the first
  visit to each site asks, and "always allow on <site>" makes it quiet.
  `browser.read`, `browser.find` and `browser.screenshot` are scoped to the
  current page's site (a tool's `ScopeFunc`).
- **Approval previews.** An integration can attach a screenshot to an
  approval (`GET /v1/approvals/{id}/preview`, shown in the web UI and sent
  as a photo on Telegram); previews are deleted when the approval resolves.
- **Sandbox integration** (`integrations.sandbox`). `sandbox.run` executes
  Python, Node or shell code in a throwaway Docker container: no network,
  read-only root, tmpfs workdir, non-root, no capabilities, memory and pid
  limits, hard timeout, fresh container per run; inputs and outputs travel as
  tar streams, never bind mounts. Asks by default.
- **Files integration** (`integrations.files`). Read, search, write, move and
  delete within admin-listed root folders only, with symlink and traversal
  guards, explicit `overwrite: true` for existing files, and destructive
  deletes.

### Changed

- Policy precedence is now scoped rule > capability rule > integration
  default > risk default > deny; an integration default can only tighten the
  admin's risk default, never loosen it.
- `GOALS.md` principle 1 reworded: browser, files and code execution exist
  only as scoped, contained integrations.

## [0.1.0] - 2026-10-13

First public release.

### Added

- **Daemon and CLI.** `bosund` (HTTP + SSE API, agent loop, scheduler) and
  `bosun` (CLI), two static binaries for macOS and Linux on amd64/arm64.
  SQLite storage (pure Go, no cgo) with embedded migrations.
- **Agent loop** with tool use and streaming over the HTTP API; LLM providers
  for Anthropic, any OpenAI-compatible endpoint, and Ollama. Provider API keys
  come from the encrypted vault (`bosun credentials set llm <provider>`),
  falling back to an environment variable, and rotate without a restart.
- **Permission model.** Every tool has a capability and a risk level
  (read / write / destructive); per-user `allow` / `ask` / `deny` rules with
  risk defaults (write and destructive ask). Approvals from the CLI, web UI or
  Telegram, with expiry. One dispatcher is the only path from the model to a
  tool, enforced by a test.
- **Audit log** of every tool call, decision, approval, policy change and
  model request (content redacted unless `audit.log_content: true`).
- **Prompt-injection mitigations.** Results from outside Bosun are flagged
  untrusted and wrapped in delimited data blocks; memory extraction is skipped
  on turns that read untrusted content.
- **Hooks**: `pre_model_call`, `pre_tool_call`, `post_tool_call`,
  `on_approval`; built-in secret redaction (on by default); opt-in script
  hooks that fail closed.
- **Integrations**: `websearch` (page fetch with an SSRF guard; search via
  SearXNG), `notes` (Markdown folder, per-user or shared), `email`
  (IMAP/SMTP; Gmail, iCloud, Fastmail, Outlook.com, Yahoo, self-hosted;
  `email.received` events), `calendar` (CalDAV read/write with ETag
  conflict detection and recurrence expansion; read-only ICS feeds).
  An import-guard test keeps integrations from reaching the store, auth,
  vault, dispatcher, each other, or `os/exec`.
- **MCP bridge**: tools from external Model Context Protocol servers (stdio
  and streamable HTTP) as `mcp.<server>.<tool>` capabilities, with
  annotation-based risk, untrusted results and a minimal child environment.
- **Account linking**: `bosun connect email` and `bosun connect calendar`
  wizards with provider presets, autodiscovery and app-password tests before
  saving; OAuth2 + PKCE for Google and Microsoft (`bosun connect google`).
- **Credential vault**: AES-256-GCM, master key in the macOS Keychain, Linux
  Secret Service, or a 0600 file; credentials API and CLI that never echo
  secrets.
- **Memory**: per-user memories with full-text recall and background
  extraction; list, add, edit and forget from the CLI, API and web UI.
- **Routines**: prompts run on a cron schedule or on events, with the owner's
  permissions; run history.
- **Telegram channel**: chat, Approve/Deny buttons and routine results via a
  long-polling bot; single-use link codes.
- **Web UI** embedded in `bosund`: chat, approvals, per-tool policy, linked
  accounts, memories, and user management for admins.
- **Household users**: roles, invite codes, `bosun users`, `bosun passwd`;
  deleting a user deletes all of their data except audit entries.
- **Networking**: loopback by default; LAN mode with a self-signed
  certificate and fingerprint pinning; `behind_proxy` mode for Tailscale Serve
  or Caddy, with `trusted_proxies`.
- **Operations**: `bosun setup`, `bosun service` (launchd agent
  `com.github.perishabledave.bosun`, systemd user or system unit `bosund`),
  `bosun doctor` (also `GET /v1/doctor`), `bosun backup` / `restore`,
  `bosun version`, `bosun config check`, `<data_dir>/env` for secrets outside
  unit files.
- **Packaging**: GoReleaser archives with checksums, `install.sh` with
  checksum verification, Homebrew tap, multi-arch Docker image on GHCR, and a
  Compose file with optional Ollama, SearXNG and Caddy.
- Project documents: README, GOALS, ARCHITECTURE, API, DEPLOY, INTEGRATIONS
  roadmap, SECURITY, CONTRIBUTING, CODE_OF_CONDUCT, THIRD_PARTY_LICENSES.

[Unreleased]: https://github.com/perishabledave/bosun/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/perishabledave/bosun/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/perishabledave/bosun/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/perishabledave/bosun/releases/tag/v0.1.0
