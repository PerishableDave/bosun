GO ?= go
BIN := bin

.PHONY: all build test vet lint fmt clean

all: build

build:
	$(GO) build -o $(BIN)/bosund ./cmd/bosund
	$(GO) build -o $(BIN)/bosun ./cmd/bosun

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# lint runs gofmt checks, go vet, and the integration import guard test.
# golangci-lint is intentionally not required.
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) test ./internal/integrations/...

fmt:
	gofmt -w .

clean:
	rm -rf $(BIN)
