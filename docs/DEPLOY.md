# Deploying Bosun

Bosun is two static binaries: `bosund` (the daemon: agent loop, HTTP API, web
UI, scheduler) and `bosun` (the CLI). There are two supported ways to run it:

- **[On your own computer](#a-personal-machine)** (macOS or Linux): install
  the binaries, run `bosun setup`, store your model key, run
  `bosun service install`, check with `bosun doctor`. Bosun
  listens on `127.0.0.1` only.
- **[On a homelab server](#b-homelab-server-docker-compose)** for a
  household: Docker Compose, optionally with a local LLM (Ollama) and a
  private search engine (SearXNG), reached from your devices through
  Tailscale or a reverse proxy.

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
curl -fsSL https://raw.githubusercontent.com/perishabledave/bosun/main/install.sh | sh
# no sudo: install into ~/.local/bin
curl -fsSL https://raw.githubusercontent.com/perishabledave/bosun/main/install.sh | sh -s -- --user
```

The script itself downloads the binaries from the latest
[GitHub release](https://github.com/perishabledave/bosun/releases/latest);
each release also carries a copy of `install.sh` as an asset. Options:
`--user`, `--bin-dir DIR`, `--version v1.2.3`, and `--base-url URL`
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
`~/.bosun/bosun.yaml` (`llm.default`; Anthropic, a local Ollama model, or any
OpenAI-compatible endpoint). See the [README](../README.md#quick-start) for
every option.

### 3. Give it a model key

Skip this for a local Ollama model. For a cloud model, either:

- **the vault (recommended).** Once the service is running (next step) and you
  are logged in, store the key encrypted, shared by all users:

  ```sh
  bosun login
  bosun credentials set llm anthropic --shared   # hidden prompt; admin only
  ```

  It takes effect within 30 seconds, with no restart, and can be rotated the
  same way. The key is looked up in the vault first, then in the environment.
- **the env file.** Put `ANTHROPIC_API_KEY=sk-ant-...` in `~/.bosun/env`
  (`chmod 600`; bosund refuses to start if it is group- or world-readable or
  not yours). bosund reads it at startup. Use this when you want the key in
  place before the first start; it is also where other environment secrets
  (`client_secret_env`, a Telegram `token_env`) go.

Background services do not see variables exported in your shell profile, which
is why the key goes in the vault or the env file and never in a unit file.

### 4. Run it in the background

```sh
bosun service install
```

installs and starts a per-user launchd agent (macOS) or systemd user unit
(Linux) that runs `bosund` at login. Related commands: `bosun service
status|logs|restart|stop|start|uninstall`. On Linux run `sudo loginctl
enable-linger $USER` once so it keeps running after you log out. `--system`
installs a system unit running as a dedicated `bosun` user; `--print` shows
the unit without installing it.

With Homebrew you can use Homebrew's service manager instead:

```sh
brew services start bosun
```

The Homebrew service runs `bosund` with its default config,
`~/.bosun/bosun.yaml`, which is exactly where `bosun setup` writes it, so run
`bosun setup` first. Its log is `$(brew --prefix)/var/log/bosun.log`.

### 5. Check it

```sh
bosun doctor
```

reports ok / warn / fail for the config, data directory permissions, master
key, daemon reachability and version, each model provider's key source and
reachability, integrations and channels, TLS certificate expiry, disk space and
reverse-proxy settings, and exits non-zero on any failure. Then:

```sh
bosun chat "what's on my calendar today?"
open http://127.0.0.1:7777     # the web UI
```

(`bosun login` once, if you have not already.)

### Where files live

| Path | What |
|---|---|
| `~/.bosun/bosun.yaml` | the config (`--config` / `BOSUN_CONFIG` to use another) |
| `~/.bosun/bosun.db` (+ `-wal`, `-shm`) | SQLite: users, conversations, memory, policy, routines, the encrypted vault, the audit log |
| `~/.bosun/master.key` | vault master key, only when it is not in the OS keychain (see `daemon.master_key`) |
| `~/.bosun/tls/` | the self-signed certificate, if you enabled LAN mode |
| `~/.bosun/cli-token`, `cli-server.json` | the CLI's session token and server/pin |
| `~/.bosun/integrations/` | integration data (e.g. notes) |
| `~/.bosun/env` | `KEY=VALUE` environment for the daemon, read at startup; must be mode 0600 |
| binaries | `/usr/local/bin` or `~/.local/bin` (install.sh), `$(brew --prefix)/bin` (Homebrew) |

Everything Bosun knows about you is in `~/.bosun`, plus the master key in the
macOS Keychain or Linux Secret Service when `master_key` is `auto` (the
default) and a keychain is available.

### Upgrade

```sh
brew upgrade bosun && brew services restart bosun
# or: re-run the install script, then restart the service
#     (`bosun service restart`)
```

The database schema is migrated automatically when `bosund` starts. Your
`bosun.yaml` is never rewritten by an upgrade.

### Back up

```sh
bosun backup --out bosun-backup.tar.gz     # safe while bosund runs
bosun restore bosun-backup.tar.gz          # stop bosund first (bosun service stop)
```

The archive (mode 0600) holds `bosun.yaml`, a consistent database snapshot, and
the data directory's `master.key` (if file-based), `env`, `integrations/` and
`tls/`. If the master key is in the OS keychain it is **not** included (you get
a warning), so on another machine the backup alone cannot decrypt stored
credentials; either keep the keychain in your normal machine backup or pass
`--include-keychain-key`, after which the archive can read every credential:
store it encrypted. `restore` refuses a non-empty data directory unless
`--force` (which moves the old one aside, never deletes it).

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
   `http://searxng:8080`, email and calendar enabled, Telegram disabled until
   you store a bot token), and
2. generates a self-signed certificate in `/data/tls/`, because bosund
   requires TLS on non-loopback addresses (`tls.enabled: true` is the default:
   the safer choice if the port is ever exposed without a proxy).

An existing `/data/bosun.yaml` is never overwritten. Edit it with
`docker compose exec bosun vi /data/bosun.yaml`, then
`docker compose restart bosun`. To use Anthropic instead of Ollama, set
`llm.default: anthropic/claude-sonnet-5-5` and provide the key either in
`.env` (`ANTHROPIC_API_KEY`) or, after step 3, in the vault with
`docker compose exec bosun bosun credentials set llm anthropic --shared`. To
enable Telegram, store the bot token the same way (`bosun credentials set
channels telegram_bot_token --shared`), set `channels.telegram.enabled: true`
and restart. Run `docker compose exec bosun bosun doctor` to check the setup.

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

or open the web UI (step 4). `docker compose exec bosun bosun config check
--config /data/bosun.yaml` validates the file after you edit it.

**Household users.** `bosun setup` creates the first (admin) account. Each
person should have their own account so that their memory, connected
accounts and approvals stay separate. An admin adds people with an invite
code, so the admin never learns their password:

```sh
bosun users add alice            # prints a single-use code valid for 24 hours
```

Alice then runs `bosun accept-invite <code> --server https://myhost:7777`
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
(`https+insecure` because bosund serves its own self-signed certificate by
default; with the `behind_proxy` setup below, use `tailscale serve --bg 7777`.) A Tailscale sidecar container is sketched at
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

**`behind_proxy` (plain HTTP behind the proxy).** The defaults above keep TLS
on. If you would rather have the proxy terminate TLS alone, edit
`/data/bosun.yaml`: set `daemon.behind_proxy: true` and
`daemon.tls.enabled: false` (they are mutually exclusive), optionally
`daemon.trusted_proxies` (default: loopback and private ranges, which includes
the compose network), and `docker compose restart bosun`. bosund then serves
plain HTTP and believes `X-Forwarded-*` headers only from trusted proxies.
Switch the proxy at the same time: `reverse_proxy bosun:7777` in the Caddyfile
(drop the `transport http` block), or `tailscale serve --bg 7777`. Only do
this when every client reaches Bosun through the proxy: keep the published port
on `127.0.0.1`. `bosun doctor` flags a non-loopback listen behind a proxy and
forwarded headers from untrusted addresses.

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
docker compose exec bosun bosun backup --out /data/backup.tar.gz
docker compose cp bosun:/data/backup.tar.gz .   # then delete it from the volume

# or a cold copy of the whole volume:
docker compose stop bosun
docker run --rm -v bosun_bosun-data:/data:ro -v "$PWD":/backup alpine \
  tar czf /backup/bosun-data-$(date +%F).tgz -C /data .
docker compose start bosun
```

Restore a `bosun backup` archive with `bosun restore` into a fresh volume, or a volume copy by extracting into an empty `bosun_bosun-data` volume (as uid 1000)
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

### Code sandbox in Docker Compose

The [code sandbox](../README.md#sandbox-run-code) (`sandbox.run`) starts a
throwaway Docker container per run, so `bosund` needs a Docker daemon. On a
personal machine (section A) it simply uses your Docker Desktop or Docker
Engine: **that is the recommended setup if you want the sandbox.**

Inside the compose setup the bosun container has no Docker access, and the
compose file deliberately does **not** mount the Docker socket: access to
`/var/run/docker.sock` is **root-equivalent on the host** (anything that can
talk to it can start a privileged container that mounts `/`). Bosun only ever
starts locked-down containers and the model never sees the socket, but a
compromise of the bosun container would become a compromise of the host.

If you accept that, the opt-in overlay
[deploy/docker/compose.sandbox.yml](../deploy/docker/compose.sandbox.yml)
builds a local image with the docker CLI, mounts the socket and adds the
socket's group (it is an overlay file rather than a profile because a profile
cannot add a mount to the existing `bosun` service):

```sh
export DOCKER_GID=$(stat -c %g /var/run/docker.sock)
docker compose -f compose.yml -f compose.sandbox.yml up -d --build
docker compose exec bosun vi /data/bosun.yaml   # integrations: {sandbox: {enabled: true}}
docker compose restart bosun
docker compose exec bosun bosun doctor          # sandbox: docker reachable, images
```

Keep passing both `-f` files for later `up`/`pull` commands, or put
`COMPOSE_FILE=compose.yml:compose.sandbox.yml` in `.env`. Sandbox containers
are siblings on the host's Docker (named `bosun-sbx-*`, labelled
`bosun.sandbox=1`); no host paths are mounted into them, so no path mapping
is needed. Pre-pull the images on the host
(`docker pull python:3.12-alpine node:22-alpine alpine:3.22`) to avoid the
wait on first use.

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
  (`.env` for compose, mode 0600; `~/.bosun/env` for a service, mode 0600) or
  the vault (`bosun credentials set llm anthropic --shared`); account passwords and tokens
  are only ever in the encrypted vault.
- **The Docker socket is never mounted by default.** The code sandbox in
  compose needs it and it is root-equivalent on the host; see
  [Code sandbox in Docker Compose](#code-sandbox-in-docker-compose).
- **Keep it updated** (`brew upgrade`, the install script, or
  `docker compose pull`).
