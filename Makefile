GO ?= go
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

BIN := bin
PKG := ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: all
all: lint test

.PHONY: build
build:
	$(GO) build $(PKG)

.PHONY: fasync
fasync:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/fasync ./cmd/fasync

.PHONY: install
install:
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/fasync

.PHONY: test
test:
	$(GO) test -race -count=1 $(PKG)

.PHONY: cover
cover:
	$(GO) test -race -count=1 -coverprofile=coverage.txt -covermode=atomic $(PKG)
	$(GO) tool cover -func=coverage.txt | tail -n 1

# Live suite against a FreeAgent sandbox. Never runs in PR CI: it needs real
# credentials and consumes the per-user rate limit.

# Compiled to a stable path rather than run through `go test`, which builds to
# a fresh temp path every time, so an outbound firewall authorises it once.
.PHONY: test-integration
test-integration: $(BIN)/integration.test
	$(BIN)/integration.test -test.v -test.count=1 -test.run '$(RUN)'

$(BIN)/integration.test:
	$(GO) test -c -tags=integration -o $@ ./...

RUN ?= .

# golangci-lint refuses to analyse a module whose Go version is newer than the
# one it was itself built with, so a system copy can fail on a 1.26 target.
# Prefer a locally built one when it is present.
GOLANGCI ?= $(if $(wildcard $(BIN)/golangci-lint),$(BIN)/golangci-lint,golangci-lint)

.PHONY: lint
lint:
	$(GOLANGCI) run

.PHONY: lint-tools
lint-tools:
	GOBIN=$(CURDIR)/$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

GOLANGCI_VERSION ?= v2.12.2

.PHONY: fmt
fmt:
	$(GO) fmt $(PKG)
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.txt
