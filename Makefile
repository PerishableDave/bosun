GO ?= go
BIN := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
# goreleaser from PATH if installed, else run it through the Go toolchain.
GORELEASER ?= $(shell command -v goreleaser 2>/dev/null || echo "$(GO) run github.com/goreleaser/goreleaser/v2@latest")
IMAGE ?= bosun:dev

.PHONY: all build test test-race check vet lint fmt clean release-snapshot docker

all: build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/bosund ./cmd/bosund
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/bosun ./cmd/bosun

test:
	$(GO) test ./...

# test-race is what CI runs (needs cgo, i.e. a C compiler, on Linux).
test-race:
	$(GO) test -race -count=1 ./...

# check is everything CI checks for Go code: run it before opening a PR.
check: lint test-race

vet:
	$(GO) vet ./...

# lint runs gofmt checks, go vet, and the architecture guard tests: the
# integration import guard (no os/exec, store, auth, vault, dispatch or other
# integrations under internal/integrations) and the dispatcher choke-point
# scanner. golangci-lint is intentionally not required.
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) test -count=1 -run 'TestIntegrationImports|TestGuardCatchesViolations' ./internal/integrations
	$(GO) test -count=1 -run 'TestOnlyDispatcherInvokesHandlers|TestChokepointScannerDetects' ./internal/dispatch

fmt:
	gofmt -w .

# release-snapshot builds every release artifact into dist/ without a tag and
# without publishing anything. Pass GORELEASER_FLAGS=--skip=docker when
# Docker is not running.
release-snapshot:
	$(GORELEASER) release --snapshot --clean $(GORELEASER_FLAGS)

# docker builds the image from source for the host platform.
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

clean:
	rm -rf $(BIN) dist
