# Security policy

Bosun holds your mail, calendar and API credentials and acts on your behalf,
so security reports are the most valuable contributions we get. Thank you.

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request for a
vulnerability.**

Report it privately through GitHub:
**[Report a vulnerability](https://github.com/perishabledave/bosun/security/advisories/new)**
(the repository's *Security* tab → *Advisories* → *Report a vulnerability*).
This opens a private security advisory visible only to you and the
maintainer.

Please include:

- the Bosun version (`bosun version`) and how it runs (install script,
  Homebrew, Docker; macOS or Linux);
- the affected component and, if relevant, the relevant parts of `bosun.yaml`
  (with secrets removed);
- steps to reproduce, or a proof of concept, and the impact you see.

What to expect: an acknowledgement within 7 days, an assessment and plan
within 14 days, and a fix released as soon as is practical. We will credit you
in the advisory and the changelog unless you prefer otherwise. Bosun is
maintained by one person, so please allow some slack around holidays, and
please give us a reasonable window (90 days by default) before disclosing.

## Supported versions

Bosun is pre-1.0. Only the **latest release** gets security fixes; please
reproduce on it (or on `main`) before reporting.

## Scope

In scope:

- **The daemon** (`bosund`): HTTP API and SSE, authentication, sessions and
  invites, TLS and LAN mode, `behind_proxy` / forwarded-header handling, the
  credential vault and master-key handling, backups, the `env` file, routines
  and hooks.
- **The CLI** (`bosun`): token and certificate-pin storage, `bosun service`
  unit files, `bosun backup` / `restore`.
- **The web UI** embedded in `bosund` (XSS, CSRF, session handling).
- **Integrations and channels**: `email`, `calendar`, `notes`, `websearch`,
  the MCP bridge, OAuth account linking, the Telegram channel. For example:
  credentials sent to the wrong host, SSRF through `websearch.fetch` or ICS
  feeds, path traversal in `notes`, cross-user data access.
- **The permission model**: any way to run a tool without a policy decision,
  bypass an `ask` or `deny` rule, approve on another user's behalf, act
  outside a granted capability, or reach a tool handler other than through
  the dispatcher.
- Release artifacts: `install.sh`, the Docker image and compose file.

Out of scope:

- Vulnerabilities in third-party MCP servers you configure, in LLM providers,
  or in mail/calendar servers (report those upstream).
- Attacks that need control of the user account running `bosund`, or read
  access to `data_dir` together with the master key: the [threat
  model](ARCHITECTURE.md#security-model) treats these as already game over.
- Script hooks doing what their configured script does (they are an
  explicit, admin-only, off-by-default way to run your own programs).
- A model *producing* a harmful tool call that the policy then stops or that
  waits for approval: that is the system working as designed.

## Prompt injection

Content from email, calendars, web pages, notes and MCP servers is treated as
untrusted data: it is wrapped in delimited blocks, the model is told never to
follow instructions inside them, and memory extraction is skipped on turns
that read untrusted content. **This resistance is best-effort; no current
model can be made immune.** The real backstop is the permission model: write
and destructive actions (sending mail, changing events, deleting notes) ask
for your approval by default, and every tool call is audited.

So a report that "a crafted email made the model try to send mail" is
expected behaviour if the send waited for approval. It **is** a vulnerability
if injected content can get an action executed without the approval your
policy requires, change your policy, approve something, leak credentials, or
reach another user's data. Reports that meaningfully weaken our mitigations
are welcome too; please mark them as hardening rather than a vulnerability.

The full threat model is in [ARCHITECTURE.md, "Security model"](ARCHITECTURE.md#security-model),
and the operator checklist is in [docs/DEPLOY.md](docs/DEPLOY.md#security-checklist).
