# Third-party licenses

Bosun is MIT licensed ([LICENSE](LICENSE)). **Every Go module compiled into
`bosund` and `bosun` is under MIT, BSD-2/3-Clause, Apache-2.0, or the public
domain (SQLite), all compatible with distributing Bosun under MIT.** The
binary archives, Homebrew formula and container image therefore carry no
copyleft obligations from Bosun's own dependencies. The only copyleft licenses
nearby are in software Bosun does not link: one module that appears in the
module graph but is never compiled (MPL-2.0), Alpine packages in the container
base image (separate programs, see below), and the optional SearXNG container
(AGPL-3.0), which Compose pulls from upstream and Bosun only talks to over HTTP.

Checked against the license files in the module cache (`go env GOMODCACHE`)
for the versions pinned in `go.mod` / `go.sum`, at the 0.1.0 cut. Update this
file when `go.mod` changes (`go list -m all`; `go list -deps ./...` shows what
is compiled in).

## Go modules compiled into the binaries

| Module | Version | License | Why |
|---|---|---|---|
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | CLI framework |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause | flags (via cobra) |
| gopkg.in/yaml.v3 | v3.0.1 | MIT and Apache-2.0 (see its NOTICE) | `bosun.yaml` |
| golang.org/x/crypto | v0.58.0 | BSD-3-Clause | argon2id password hashing |
| golang.org/x/term | v0.47.0 | BSD-3-Clause | password prompts |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause | via x/term, modernc |
| modernc.org/sqlite | v1.60.1 | BSD-3-Clause; bundles SQLite (public domain) and sqlite-vec (MIT) | database |
| modernc.org/libc | v1.77.1 | BSD-3-Clause; includes code from Go (BSD-3-Clause), musl libc (MIT), go-netdb (MIT), nixpkgs (MIT) | via sqlite |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause | via sqlite |
| modernc.org/memory | v1.12.1 | BSD-3-Clause; includes mmap-go (BSD-3-Clause) | via sqlite |
| github.com/dustin/go-humanize | v1.0.1 | MIT | via modernc.org/libc |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | via modernc.org/libc |
| github.com/mattn/go-isatty | v0.0.24 | MIT | via modernc.org/libc |
| github.com/ncruces/go-strftime | v1.0.0 | MIT | via modernc.org/libc |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | via modernc.org/mathutil |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 | via cobra, Windows builds only (Bosun does not ship Windows builds) |

The Go standard library and runtime (BSD-3-Clause) are linked into both
binaries as well.

## Go modules in the module graph, not compiled into the binaries

These appear in `go list -m all` because dependencies' `go.mod` files list
them (code generators, test-only tools), but no package from them is built
into Bosun.

| Module | Version | License |
|---|---|---|
| github.com/google/pprof | v0.0.0-20260802141513-ef3492d7dac3 | Apache-2.0 |
| github.com/hashicorp/golang-lru/v2 | v2.0.7 | MPL-2.0 (with BSD-3-Clause parts) |
| golang.org/x/mod | v0.41.0 | BSD-3-Clause |
| golang.org/x/net | v0.60.0 | BSD-3-Clause |
| golang.org/x/sync | v0.23.0 | BSD-3-Clause |
| golang.org/x/text | v0.43.0 | BSD-3-Clause |
| golang.org/x/tools | v0.50.0 | BSD-3-Clause |
| gopkg.in/check.v1 | v0.0.0-20161208181325-20d25e280405 | BSD-2-Clause |
| go.yaml.in/yaml/v3 | v3.0.4 | MIT and Apache-2.0 |
| github.com/cpuguy83/go-md2man/v2 | v2.0.6 | MIT |
| github.com/russross/blackfriday/v2 | v2.1.0 | BSD-2-Clause |
| modernc.org/cc/v4 | v4.29.7 | BSD-3-Clause |
| modernc.org/ccgo/v4 | v4.36.1 | BSD-3-Clause |
| modernc.org/fileutil | v1.4.0 | BSD-3-Clause |
| modernc.org/gc/v2 | v2.6.5 | BSD-3-Clause |
| modernc.org/gc/v3 | v3.1.5 | BSD-3-Clause |
| modernc.org/goabi0 | v0.2.0 | BSD-3-Clause |
| modernc.org/opt | v0.2.0 | BSD-3-Clause |
| modernc.org/sortutil | v1.2.1 | BSD-3-Clause |
| modernc.org/strutil | v1.2.1 | BSD-3-Clause |
| modernc.org/token | v1.1.0 | BSD-3-Clause |

Five of these (x/net, x/text, go.yaml.in/yaml, go-md2man, blackfriday) are
not even downloaded by a build, so their licenses were taken from upstream
rather than the local module cache.

## Container images

| Image | Where | License | Relationship to Bosun |
|---|---|---|---|
| `golang:1.26-alpine` | `Dockerfile` build stage | BSD-3-Clause (Go) | build only; not in the shipped image |
| `alpine:3.22` | base of `ghcr.io/perishabledave/bosun` | Alpine is a collection of packages under their own licenses: musl (MIT), BusyBox and apk-tools (GPL-2.0), OpenSSL (Apache-2.0), ca-certificates (MPL-2.0), tzdata (public domain) | separate programs aggregated in the image; Bosun does not link them. Sources: [Alpine aports](https://gitlab.alpinelinux.org/alpine/aports) |
| `ollama/ollama` | `deploy/docker/compose.yml`, optional profile | MIT | separate service, pulled from upstream, reached over HTTP |
| `searxng/searxng` | `deploy/docker/compose.yml`, optional profile | AGPL-3.0 | separate service, pulled from upstream, reached over HTTP; Bosun ships only its own `searxng/settings.yml` |
| `caddy:2` | `deploy/docker/compose.yml`, optional profile | Apache-2.0 | separate reverse proxy, pulled from upstream |
| `tailscale/tailscale` | commented-out example in `compose.yml` | BSD-3-Clause | separate service, if you enable it |

Running these images alongside Bosun does not change Bosun's license. If you
modify SearXNG itself and offer it to others over a network, the AGPL applies
to your modified SearXNG, not to Bosun.
