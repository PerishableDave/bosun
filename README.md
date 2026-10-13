# Bosun

An open-source personal agent that gets real work done, but only through
integrations you've explicitly granted, so it can never do more than you allowed.

See [GOALS.md](GOALS.md) for the why and [ARCHITECTURE.md](ARCHITECTURE.md) for
the how. The HTTP API is documented in [docs/API.md](docs/API.md).

## Status

Early foundation: config, SQLite store, auth, HTTP API skeleton, and the core
interfaces (`llm`, `integration`, `policy`, `approval`, `audit`, `hooks`).
Chat, tools, and approvals are not wired up yet.

## Quick start

Requires Go 1.26+.

```sh
make build
./bin/bosun setup          # writes bosun.yaml, creates data dir and first admin
./bin/bosund -config bosun.yaml
./bin/bosun status
./bin/bosun login
```

## Development

```sh
make test   # go test ./...
make vet    # go vet ./...
make lint   # gofmt + vet + integration import guard
```

Dependencies are kept minimal: `modernc.org/sqlite` (pure Go, no cgo),
`gopkg.in/yaml.v3`, `golang.org/x/crypto` (argon2id), `golang.org/x/term`
(password prompts), and `github.com/spf13/cobra` for the CLI.

## License

TBD.
