# Bosun — Project Goals

> An open-source personal agent that gets real work done, but only through
> integrations you've explicitly granted, so it can never do more than you allowed.

## Vision

Bosun is an agentic assistant in the vein of today's personal AI agents, built
around three things those tools often trade away: **privacy**, **safety**, and
**user control**. Agents like OpenClaw get their power from broad system access.
Bosun gets its power from a growing set of controlled integrations, each one
scoped, permissioned, and auditable.

## Core Principles

Listed in priority order. When two principles conflict, the one higher on the list wins.

### 1. Capability-scoped by design
- The agent has **no** general shell, filesystem, or open-ended browser access.
- It can act only through integrations, and every integration declares its
  capabilities up front (e.g. `calendar.read`, `calendar.write`, `email.draft`,
  `email.send`).
- The user grants capabilities individually. Anything not granted is denied.

### 2. Privacy first
- Data stays local by default: memory, logs, credentials, and configuration.
- Fully offline operation is supported with local models.
- Every outbound call (cloud LLM, external API) is explicit, configurable, and logged.
- No telemetry unless the user opts in.

### 3. Safe by default
- Risky actions (send, delete, pay, post, share) require human approval by default.
- The user can loosen or tighten approval rules per capability: `allow` / `ask` / `deny`.
- Every action the agent takes is written to an audit log the user can read.
- Content coming from integrations (emails, web pages, documents) is treated as
  untrusted data, never as instructions. Prompt-injection resistance is part of
  the design from day one.

### 4. Modular everywhere
- Models, integrations, hooks, memory backends, and frontends are all pluggable.
- Each extension point has a stable, documented interface.
- Swapping a component (say, Anthropic → Ollama) is a configuration change, not a code change.

### 5. Easy to set up
- Install with a single command.
- A guided setup gets a non-developer to a working assistant in **under 10 minutes**.
- Configuration lives in one human-readable file, with sensible defaults.

## v1 Scope

| Area | Goal |
|---|---|
| **Agent core** | Model-agnostic agent loop: Anthropic, OpenAI-compatible APIs, Ollama and other local models |
| **Integrations** | Plugin system, ideally built on MCP, with a Bosun permission layer on top |
| **Permissions** | Policy engine (`allow` / `ask` / `deny` per capability) plus an approval flow |
| **Hooks** | `pre_tool_call`, `post_tool_call`, `on_schedule`, `on_event` |
| **Memory** | Persistent, local storage that the user can inspect and edit |
| **First-party integrations** | Calendar, email, notes, and web search, enough to prove the model works |
| **Frontends** | CLI and a simple local web UI |
| **Observability** | Audit log of every tool call, approval, and outbound request |

## Non-Goals (v1)

- Unrestricted computer, shell, or desktop control
- Hosted SaaS or multi-tenant deployment
- Training or fine-tuning our own models
- Mobile apps (chat-app frontends may come later)

## Success Criteria

- A new user installs Bosun and completes a real task, such as "summarize today's
  calendar and draft replies to anything urgent", in under 10 minutes.
- No integration can perform an action outside its granted capabilities. This is
  verified by tests.
- Switching LLM providers requires editing only the config file.
- A third party can write and ship a new integration using only the public docs.

## Open Decisions

These choices shape the architecture and should be settled before implementation starts.

1. **Integration protocol:** build on MCP or define our own plugin API?
   *Leaning: MCP.* We get its ecosystem for free, and Bosun's value is the
   permission and sandbox layer around it.
2. **Isolation strength:** is permission checking in the core process enough, or
   should each integration run in a sandboxed process or container? The second is
   stronger but makes setup more complex.
3. **Runtime model:** an always-on background service (needed for scheduled and
   proactive tasks) or something that only runs when invoked?
4. **Language and stack:** Python (richest AI ecosystem), TypeScript (strong MCP and
   web UI support), or Go/Rust (single-binary installs)?
5. **Proactivity:** should Bosun act on its own (briefings, inbox monitoring) or
   respond only when asked? This determines how strict the approval model must be.
6. **Audience:** a single user on their own machine, or households and small teams
   (which needs auth and multi-user support)?
