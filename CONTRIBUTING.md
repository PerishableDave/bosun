# Contributing to Bosun

Thanks for helping. Bosun's whole point is that the agent can never do more
than you allowed, so most of the rules below exist to keep that promise
checkable. Read [GOALS.md](GOALS.md) (what we optimise for, in priority order)
and skim [ARCHITECTURE.md](ARCHITECTURE.md) before a larger change.

Security issues: please follow [SECURITY.md](SECURITY.md), not the issue
tracker.

## Development setup

You need **Go 1.26** (see `go.mod`) and, for `make test-race`, a C compiler
on Linux. There is no cgo in the build itself: SQLite is `modernc.org/sqlite`
(pure Go). macOS and Linux are supported; Windows is not.

```sh
git clone https://github.com/perishabledave/bosun.git && cd bosun
make build       # bin/bosund and bin/bosun
make test        # go test ./...
make lint        # gofmt, go vet, and the architecture guard tests
make check       # lint + go test -race -count=1 ./...  (what CI runs; do this before a PR)
```

To try your build: `./bin/bosun --config /tmp/bosun-dev/bosun.yaml setup`
creates a throwaway config and data directory, then run
`./bin/bosund --config /tmp/bosun-dev/bosun.yaml` in another terminal. No API
key is needed for most work: tests use the fake LLM in `internal/llm/fake`,
and a local Ollama model works for manual testing.

Packaging changes can be checked with `make docker` and `make
release-snapshot` (GoReleaser into `dist/`, nothing published).

## Rules enforced by tests

These are not style preferences; CI fails if you break them, and reviewers
will not accept a change that weakens a guard to make it pass.

1. **Integration import guard** (`internal/integrations/integrations_test.go`).
   Nothing under `internal/integrations/` may import `os/exec`,
   `internal/store`, `internal/auth`, `internal/vault`, `internal/dispatch`,
   or another integration's package. The single exception is `os/exec` in
   `internal/integrations/mcp` itself (stdio MCP servers are subprocesses).
   Integrations are in-process, so this is our substitute for process
   isolation. Shared helpers go in a neutral package (e.g.
   `internal/providers`), not in another integration.
2. **The dispatcher is the single choke point**
   (`internal/dispatch/chokepoint_test.go`). Only `internal/dispatch` may call
   a tool's `Handler`, and only after `policy.Decide`. Never invoke a handler
   from the agent loop, an API handler, a routine or a hook; go through the
   dispatcher so policy, approvals, hooks, schema validation and the audit
   log all apply.
3. **Untrusted results must be flagged.** Any `integration.Result` whose
   content came from outside Bosun (mail, events, web pages, notes another
   user could write, MCP output, file names, subjects, sender names) sets
   `Untrusted: true`. The dispatcher then wraps it in a delimited data block,
   and memory extraction is skipped for that turn. When in doubt, flag it.
4. **User data is deleted with the user** (`TestDeleteUserCascades` in
   `internal/api/user_handlers_test.go`). The test discovers every table with
   a `user_id` column and fails if it has no seeded row. A new table with
   `user_id` needs an `ON DELETE CASCADE` (or explicit cleanup in the store),
   plus a seed row in that test. Only `audit_log` deliberately outlives users.
5. **Capabilities are declared.** The registry rejects an integration whose
   tool names or capabilities are not `<name>.`-prefixed, or whose tool uses a
   capability missing from its manifest (`internal/integration/registry.go`).

Also expected, though enforced by review: secrets never appear in logs, unit
files, flags, error messages or the audit log (redact them), credentials go
only over TLS (except to loopback), and anything that makes a network call is
explicit and logged.

## Adding an integration

Look at `internal/integrations/notes` (small, per-user) and `websearch`
(shared, network) first.

1. **Package.** `internal/integrations/<name>/`, implementing
   `integration.Integration`: `Manifest()`, `Init(ctx, cfg, creds)`,
   `Tools()`, `Close()`.
2. **Manifest.** `Name` is lowercase (`[a-z][a-z0-9_-]*`). `Scope` is
   `integration.ScopePerUser` (each user links their own account; most
   integrations) or `ScopeShared` (one household-wide backend whose credential
   only an admin can set). `Capabilities` lists every capability any tool
   uses. `CredentialKinds` lists the vault kinds you accept, e.g. `password`,
   `apikey`, `bearer`, `oauth2:google`; a kind with a `:<suffix>` (like
   `ics:<name>`) needs the optional `CredentialKindLister`.
3. **Capabilities namespace.** Every capability and tool name is
   `<name>.<something>`. Group tools by what a user would want to grant
   separately: reading, changing, and irreversible or outward-facing actions
   get different capabilities (`email.read`, `email.draft`, `email.send`).
4. **Risk levels.** `RiskRead` (no side effects; allowed by default),
   `RiskWrite` (changes state the user can see and undo; asks by default),
   `RiskDestructive` (irreversible or leaves the user's control: sending,
   deleting, paying, posting; asks by default and is never loosened by
   us). Pick the stricter level when unsure.
5. **Credentials.** Get them only through the `CredentialSource` passed to
   `Init` (`creds.Get(ctx, kind)`; the user comes from the context). Never
   read the vault, config secrets or environment variables yourself. If a
   credential can be checked cheaply, implement `CredentialTester` so `bosun
   connect` and `POST /v1/integrations/{name}/test` can verify it before it is
   saved. Other optional interfaces: `StatusReporter`, `EventEmitter` and
   `UserLister` (background pollers emitting `<name>.*` events).
6. **Input schemas.** Give every tool a JSON Schema; the dispatcher validates
   arguments (and hook rewrites) against it before the handler runs.
7. **Register** the constructor in the `builtins` map in
   `cmd/bosund/daemon.go`; its settings live under `integrations.<name>` in
   `bosun.yaml` and arrive as `integration.Config` (decode with
   `cfg.Decode`). New integrations should be off unless enabled.
8. **Tests** use fakes, never real services or the network: an
   `httptest.Server` for HTTP APIs, or a small in-process fake protocol
   server (see `calendar/fake_test.go` for CalDAV and
   `email/fakeimap_test.go` / `fakesmtp_test.go` for IMAP/SMTP). Cover: every
   tool's happy path, bad input, a missing or wrong credential, untrusted
   flagging, and any host or path restriction (redirects to other hosts,
   private addresses, `..` paths).
9. **Docs.** A section in the README (capabilities, risk, what is untrusted,
   limits) and a line in the ARCHITECTURE.md integrations table.

Integrations that exist elsewhere as an MCP server may not need first-party
code at all: the MCP bridge already gives them policy, approvals and audit.

## Adding an LLM provider

1. A package `internal/llm/<name>/` implementing `llm.Provider` (`Name`,
   `Complete`, `Stream`) that translates between Bosun's neutral messages and
   tool specs and the provider's API. If it is OpenAI-compatible, configure
   the `openai` provider with a `base_url` instead, or wrap it the way
   `internal/llm/ollama` does.
2. Wire it into `internal/llm/providers/providers.go` (`FromConfig`), taking
   the API key from the `KeyFunc` it is given (vault first, then
   `api_key_env`), never from the environment directly. Update `NeedsKey` if
   it is a hosted API.
3. Add a cheap reachability probe in `internal/doctor/doctor.go`.
4. Tests against an `httptest.Server` that replays recorded-shape responses,
   including streaming, tool calls, errors and a missing key. See
   `internal/llm/anthropic/anthropic_test.go`.
5. Document it in the README's model section.

## Commits and pull requests

- Open an issue first for anything large or user-visible (new integration,
  config change, new permission behaviour) so we can agree on the shape.
- Keep PRs focused. Run `make check`; add tests for every behaviour change and
  a regression test for every bug fix.
- Commit messages: a short imperative subject (`Add Todoist integration`,
  `calendar: handle EXDATE with TZID`), a blank line, then why. Reference
  issues (`Fixes #12`).
- Update the docs (README, docs/, ARCHITECTURE.md) and the `Unreleased`
  section of [CHANGELOG.md](CHANGELOG.md) in the same PR.
- New dependencies need a good reason: we keep the dependency list short and
  MIT/BSD/Apache-2.0 compatible (see
  [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md)).
- By contributing you agree your work is licensed under the project's [MIT
  license](LICENSE).

## AI-assisted contributions

Welcome; much of Bosun was written with AI help. But you are the author: you
must have read, understood and tested every line you submit, and be able to
explain it in review. Say in the PR description that AI tools were used. PRs
that are plainly unreviewed generated output (invented APIs, tests that do not
test anything, guards weakened to pass) will be closed.

## Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
