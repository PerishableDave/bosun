# Changelog

All notable changes to Bosun are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0,
minor versions may contain breaking changes; they are called out under
**Changed** or **Removed**.

## [Unreleased]

First public release, planned as `0.1.0`.

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

[Unreleased]: https://github.com/perishabledave/bosun/commits/main
