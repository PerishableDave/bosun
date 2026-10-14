# Deploying Bosun

Bosun is two static binaries: `bosund` (the daemon: agent loop, HTTP API, web
UI, scheduler) and `bosun` (the CLI). There are two supported ways to run it:

- **[On your own computer](#a-personal-machine)** (macOS or Linux): install
  the binaries, run `bosun setup`, start the background service. Bosun
  listens on `127.0.0.1` only.
- **[On a homelab server](#b-homelab-server-docker-compose)** for a
  household: Docker Compose, optionally with a local LLM (Ollama) and a
  private search engine (SearXNG), reached from your devices through
  Tailscale or a reverse proxy.

Commands marked *(coming in this release)* are being finished and are not in
every build yet; each one has a manual fallback next to it.

---

## A. Personal machine

### 1. Install

**Homebrew** (macOS and Linux):

```sh
brew install perishabledave/tap/bosun
```

**Install script** (macOS and Linux, amd64/arm64; verifies the SHA-256
checksum of the download):

```sh
curl -fsSL https://github.com/perishabledave/bosun/releases/latest/download/install.sh | sh
# no sudo: install into ~/.local/bin
curl -fsSL https://github.com/perishabledave/bosun/releases/latest/download/install.sh | sh -s -- --user
```

Options: `--user`, `--bin-dir DIR`, `--version v1.2.3`, and `--base-url URL`
(a mirror holding `bosun_<os>_<arch>.tar.gz` and `checksums.txt`). You can
also download an archive from the
[releases page](https://github.com/perishabledave/bosun/releases) and check it
against `checksums.txt` yourself, or build from source (`make build`, Go
1.26+).

### 2. Set up

```sh
bosun setup
```

This writes `~/.bosun/bosun.yaml`, creates the data directory `~/.bosun`
(mode 0700) and the database, asks for your admin username and password, and
offers to connect your email and calendar. Choose a model in
`~/.bosun/bosun.yaml` (`llm.default`; Anthropic with `ANTHROPIC_API_KEY`, a
local Ollama model, or any OpenAI-compatible endpoint). See the
[README](../README.md#quick-start) for every option.

### 3. Run it in the background

```sh
bosun service install      # (coming in this release)
```

installs and starts a per-user launchd agent (macOS) or systemd user unit
(Linux) that runs `bosund` at login.

With Homebrew you can use Homebrew's service manager instead:

```sh
brew services start bosun
```

The Homebrew service runs `bosund` with its default config,
`~/.bosun/bosun.yaml`, which is exactly where `bosun setup` writes it, so run
`bosun setup` first. Its log is `$(brew --prefix)/var/log/bosun.log`.

Background services do not see variables exported in your shell profile. If
you use a cloud model, give bosund its API key through the daemon's env file
`~/.bosun/env` or the credential vault *(coming in this release)*; until
then, run `bosund` from a terminal where `ANTHROPIC_API_KEY` is set, or use a
local Ollama model, which needs no key.

Then:

```sh
bosun login
bosun chat "what's on my calendar today?"
open http://127.0.0.1:7777     # the web UI
```

### Where files live

| Path | What |
|---|---|
| `~/.bosun/bosun.yaml` | the config (`--config` / `BOSUN_CONFIG` to use another) |
| `~/.bosun/bosun.db` (+ `-wal`, `-shm`) | SQLite: users, conversations, memory, policy, routines, the encrypted vault, the audit log |
| `~/.bosun/master.key` | vault master key, only when it is not in the OS keychain (see `daemon.master_key`) |
| `~/.bosun/tls/` | the self-signed certificate, if you enabled LAN mode |
| `~/.bosun/cli-token`, `cli-server.json` | the CLI's session token and server/pin |
| `~/.bosun/integrations/` | integration data (e.g. notes) |
| `~/.bosun/env` | environment for the daemon *(coming in this release)* |
| binaries | `/usr/local/bin` or `~/.local/bin` (install.sh), `$(brew --prefix)/bin` (Homebrew) |

Everything Bosun knows about you is in `~/.bosun`, plus the master key in the
macOS Keychain or Linux Secret Service when `master_key` is `auto` (the
default) and a keychain is available.

### Upgrade

```sh
brew upgrade bosun && brew services restart bosun
# or: re-run the install script, then restart the service
#     (bosun service install, or stop and start bosund)
```

The database schema is migrated automatically when `bosund` starts. Your
`bosun.yaml` is never rewritten by an upgrade.

### Back up

```sh
bosun backup               # (coming in this release)
```

Until then: stop `bosund` (or use `sqlite3 ~/.bosun/bosun.db ".backup
bosun-backup.db"` while it runs) and copy `~/.bosun`. If the master key is in
the keychain, the copy alone cannot decrypt stored credentials on another
machine; keep the keychain (e.g. your normal macOS backup) too. If the key is
in `master.key`, the backup contains everything needed to read your
credentials: store it encrypted.

---

## B. Homelab server (Docker Compose)

The image `ghcr.io/perishabledave/bosun` is multi-arch (linux/amd64 and
linux/arm64), runs as the non-root user `bosun` (uid 1000), and keeps all
state in the volume mounted at `/data`.

### 1. Get the compose files

```sh
git clone https://github.com/perishabledave/bosun.git
cd bosun/deploy/docker
```

(Only `compose.yml`, `Caddyfile` and `searxng/settings.yml` are needed; you
can copy that directory anywhere.)

Optional: put API keys in `.env` next to `compose.yml` (never in
`bosun.yaml`):

```sh
cat > .env <<'EOF'
ANTHROPIC_API_KEY=sk-ant-...
TELEGRAM_BOT_TOKEN=
SEARXNG_SECRET=<a long random string>
TZ=Europe/Madrid
EOF
chmod 600 .env
```

### 2. Start

```sh
docker compose up -d                                         # Bosun only
docker compose --profile ollama --profile searxng up -d      # + local LLM and web search
docker compose exec ollama ollama pull llama3.1              # once, ~5 GB
```

On first start the container:

1. copies the built-in template to `/data/bosun.yaml`
   ([deploy/docker/bosun.yaml](../deploy/docker/bosun.yaml): listen
   `0.0.0.0:7777` inside the container, `data_dir: /data`, `master_key:
   file`, web UI on, `llm.default: ollama/llama3.1` with Ollama at
   `http://ollama:11434`, Anthropic via `ANTHROPIC_API_KEY`, websearch via
   `http://searxng:8080`, email, calendar and Telegram enabled), and
2. generates a self-signed certificate in `/data/tls/`, because bosund
   requires TLS on non-loopback addresses.

An existing `/data/bosun.yaml` is never overwritten. Edit it with
`docker compose exec bosun vi /data/bosun.yaml`, then
`docker compose restart bosun`. To use Anthropic instead of Ollama, set
`llm.default: anthropic/claude-sonnet-5-5`.

The port is published on the host's **loopback only**
(`127.0.0.1:7777:7777`): nothing on your network can reach it until you add
Tailscale or a proxy (step 4). `docker compose ps` shows the health check
(`GET /v1/health`).

### 3. Create the admin and log in

```sh
docker compose exec bosun bosun setup --config /data/bosun.yaml --admin alice
```

This uses the existing config, asks for alice's password, and offers to
connect her email and calendar. Then, from the server:

```sh
docker compose exec bosun bosun login
docker compose exec bosun bosun chat "hello"
```

or open the web UI (step 4).

**Household users.** `bosun setup` creates the first (admin) account. Each
person should have their own account so that their memory, connected
accounts and approvals stay separate. An admin adds people with an invite
code, so the admin never learns their password:

```sh
bosun users add alice            # prints a single-use code valid for 24 hours
```

Alice then runs `bosun accept-invite <code> --server https://bosun.home:7777`
on her own machine (or opens the web UI and chooses "Have an invite code?"),
picks a password, and is logged in. From there she connects her own mail and
calendar with `bosun connect email` / `bosun connect calendar` and links
Telegram with `bosun channels link telegram`. Admins manage accounts with
`bosun users list|disable|enable|reset-password|remove` or the Users tab in
the web UI; removing a user deletes everything they own except audit entries.

### 4. Reach it from your devices

**Tailscale (recommended).** No ports opened on your router or LAN, real
HTTPS certificates, and only devices on your tailnet can connect. With
Tailscale installed on the server:

```sh
tailscale serve --bg https+insecure://127.0.0.1:7777
```

Bosun is then at `https://<server>.<tailnet>.ts.net/` for your tailnet only.
(`https+insecure` because bosund currently serves its own self-signed
certificate; once it runs with `behind_proxy: true` and TLS off, use
`tailscale serve --bg 7777`.) A Tailscale sidecar container is sketched at
the bottom of `compose.yml`. Do not use `tailscale funnel`, which publishes
to the internet.

**Caddy on your LAN.** Point a local DNS name (e.g. `bosun.home.example`, in
your router, Pi-hole or AdGuard) at the server, edit the name in
`deploy/docker/Caddyfile`, and start the `caddy` profile:

```sh
docker compose --profile caddy up -d
```

The example uses `tls internal` (Caddy's own CA; install its root
certificate from the `caddy-data` volume on your devices) and proxies to
bosund over HTTPS without verifying its self-signed certificate, which is
acceptable because that hop never leaves the compose network.

**`behind_proxy`** *(coming in this release)*: when bosund supports
`daemon.behind_proxy`, uncomment `behind_proxy: true` in `/data/bosun.yaml`
and set `daemon.tls.enabled: false`; bosund then serves plain HTTP inside the
compose network and trusts the proxy's forwarded headers. Switch the Caddyfile
to `reverse_proxy bosun:7777` at the same time. Only do this when every
client reaches Bosun through the proxy: keep the published port on
`127.0.0.1`.

The CLI from another computer (when going through the proxy, the proxy's
certificate is the one the CLI sees):

```sh
bosun --server https://bosun.home.example login
```

### Upgrade

```sh
docker compose pull && docker compose up -d
```

Migrations run at start. The template only applies on first run, so new
config options never appear in your `/data/bosun.yaml` by themselves; check
the release notes.

### Back up

Everything is in the `bosun-data` volume (named `bosun_bosun-data` by
compose), **including `master.key`**, so the backup can decrypt every stored
credential: keep it encrypted and off the server.

```sh
docker compose exec bosun bosun backup     # (coming in this release)

# or a cold copy of the whole volume:
docker compose stop bosun
docker run --rm -v bosun_bosun-data:/data:ro -v "$PWD":/backup alpine \
  tar czf /backup/bosun-data-$(date +%F).tgz -C /data .
docker compose start bosun
```

Restore by extracting into an empty `bosun_bosun-data` volume (as uid 1000)
before `docker compose up`.

### Resource needs

| Setup | RAM | CPU | Disk |
|---|---|---|---|
| Bosun with a cloud model (Anthropic, OpenAI-compatible) | ~75 MB idle; 256 MB is plenty | any 64-bit CPU; idle near 0% | 70 MB image + your data |
| + SearXNG | +150-250 MB | light | ~200 MB image |
| + Ollama, 7-8B model (llama3.1) on CPU | 8 GB minimum, 16 GB comfortable | 4+ modern cores; expect a few tokens/s | ~5 GB per model |
| + Ollama with a GPU | as above, model in VRAM (8 GB VRAM for 8B at Q4) | GPU does the work | ~5 GB per model |

The agent loop runs several model calls per request (tool use, memory
extraction), so a slow local model makes Bosun feel slow. If the server is
small, run Ollama on a desktop with a GPU and point
`llm.providers.ollama.base_url` at it (over Tailscale or your LAN).

### Raspberry Pi

- Use a **64-bit** OS (Raspberry Pi OS 64-bit or Ubuntu arm64): the image
  is `linux/arm64` only; 32-bit `armv7` is not supported.
- A Pi 4 or 5 with 2 GB+ runs Bosun, SearXNG and Caddy easily with a cloud
  model.
- Local models on a Pi are slow: a Pi 5 (8 GB) manages small models
  (`llama3.2:3b`, `qwen2.5:3b`) at a few tokens per second; an 8B model is
  impractically slow. Prefer a cloud model or Ollama on another machine.
- Put the Docker data on an SSD rather than the SD card (SQLite writes on
  every turn).
- Set `TZ` in `.env` (or `daemon.timezone`) so routines fire at your local
  time; the container defaults to UTC.

---

## Security checklist

- **Loopback by default.** `bosun setup` listens on `127.0.0.1:7777`; the
  compose file publishes only on the host's `127.0.0.1`. Anything wider is a
  choice you make.
- **TLS or a proxy for anything off-host.** bosund refuses a non-loopback
  listen address without TLS (`bosun setup --lan` generates a certificate and
  prints its fingerprint for pinning). In Docker, reach it through Tailscale
  Serve or an HTTPS reverse proxy; do not publish 7777 on `0.0.0.0`, and never
  expose Bosun directly to the internet.
- **Approvals default to ask.** `policy.defaults` is `read: allow`,
  `write: ask`, `destructive: ask`. Loosen per capability (`bosun policy`)
  only for actions you are happy to have happen unattended.
- **Read the audit log.** Every tool call, approval, policy change and model
  request is in the `audit_log` table of `<data_dir>/bosun.db`
  (`~/.bosun/bosun.db`; `/data/bosun.db` in Docker). Content is redacted
  unless `audit.log_content: true`.
- **Know where the master key is.** On a personal machine `master_key: auto`
  keeps it in the OS keychain when one is available, so a stolen copy of
  `~/.bosun` cannot decrypt your credentials. In Docker it is
  `/data/master.key` (`master_key: file`), next to the data it protects:
  protect the volume and its backups accordingly.
- **Secrets stay out of `bosun.yaml`.** API keys come from the environment
  (`.env` for compose, mode 0600) or the vault; account passwords and tokens
  are only ever in the encrypted vault.
- **Keep it updated** (`brew upgrade`, the install script, or
  `docker compose pull`).
