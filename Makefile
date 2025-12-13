GO_INSTALL_DIR := $(HOME)/.go
GO_BIN ?= go
BACKEND_DIR ?= $(CURDIR)
BINARY_NAME ?= indexer
BINARY := $(CURDIR)/$(BINARY_NAME)
CACHE_ROOT := $(CURDIR)/.cache
export GOCACHE := $(CACHE_ROOT)/go-build
export GOMODCACHE := $(CACHE_ROOT)/gomod
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "")
LDFLAGS ?= -X github.com/mordilloSan/indexer/internal/version.Version=$(VERSION) -X github.com/mordilloSan/indexer/internal/version.Commit=$(GIT_COMMIT) -X github.com/mordilloSan/indexer/internal/version.Date=$(BUILD_DATE)
GOLANGCI_LINT_MODULE  := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= latest
GOLANGCI_LINT         := $(GO_INSTALL_DIR)/bin/golangci-lint
GOLANGCI_LINT_OPTS ?= --modules-download-mode=mod
GOCYCLO               := $(GO_INSTALL_DIR)/bin/gocyclo

.ONESHELL:
SHELL := /bin/bash

.PHONY: ensure-golint ensure-gocyclo golint run run-verbose build test benchmark index status

ensure-golint:
	@{ set -euo pipefail; \
	   bin="$(GOLANGCI_LINT)"; need=1; \
	   if [ -x "$$bin" ]; then \
	     out="$$( "$$bin" version 2>/dev/null || true)"; \
	     ver="$$( printf '%s' "$$out" | sed -n 's/^golangci-lint has version[[:space:]]\([v0-9.]\+\).*/\1/p' )"; \
	     ver_no_v="$${ver#v}"; major="$${ver_no_v%%.*}"; \
	     built_ok="$$( printf '%s' "$$out" | grep -Eq 'built with go1\.25(\.|$$)' && echo yes || echo no )"; \
	     if [ "$$major" = "2" ] && [ "$$built_ok" = "yes" ]; then need=0; fi; \
	   fi; \
	   if [ $$need -eq 1 ]; then \
	     echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) (v2) with local Go ($(GO_BIN))..."; \
	     rm -f "$$bin" || true; \
	     PATH="$(GO_INSTALL_DIR)/bin:$$PATH" GOBIN="$(GO_INSTALL_DIR)/bin" GOTOOLCHAIN=local GOFLAGS="-buildvcs=false" \
	       "$(GO_BIN)" install "$(GOLANGCI_LINT_MODULE)@$(GOLANGCI_LINT_VERSION)"; \
	   fi; \
	   "$$bin" version | head -n1; \
	   out="$$( "$$bin" version )"; \
	   ver="$$( printf '%s' "$$out" | sed -n 's/^golangci-lint has version[[:space:]]\([v0-9.]\+\).*/\1/p' )"; \
	   ver_no_v="$${ver#v}"; major="$${ver_no_v%%.*}"; \
	   [ "$$major" = "2" ] || { echo "ERROR: not a v2 golangci-lint"; exit 1; }; \
	   echo "$$out" | grep -Eq 'built with go1\.25(\.|$$)' || { echo "ERROR: golangci-lint not built with Go 1.25"; exit 1; }; \
	   echo "golangci-lint v2 ready."; \
	}

ensure-gocyclo:
	@if [ ! -x "$(GOCYCLO)" ]; then \
	  echo "Installing gocyclo..."; \
	  GOBIN="$(GO_INSTALL_DIR)/bin" "$(GO_BIN)" install github.com/fzipp/gocyclo/cmd/gocyclo@latest; \
	fi
	@echo "gocyclo ready."

golint: ensure-golint ensure-gocyclo
	@set -euo pipefail
	@echo "Linting Go module in: $(BACKEND_DIR)"
	@echo "Running gofmt..."
ifneq ($(CI),)
	@fmt_out="$$(cd "$(BACKEND_DIR)" && gofmt -s -l .)"; \
	if [ -n "$$fmt_out" ]; then echo "The following files are not gofmt'ed:"; echo "$$fmt_out"; exit 1; fi
else
	@( cd "$(BACKEND_DIR)" && gofmt -s -w . )
endif
	@echo "Ensuring go.mod is tidy..."
	@( cd "$(BACKEND_DIR)" && go mod tidy && go mod download )
	@echo "Running golangci-lint..."
	@( cd "$(BACKEND_DIR)" && "$(GOLANGCI_LINT)" run --fix ./... --timeout 3m $(GOLANGCI_LINT_OPTS) )
	@echo "Running gocyclo (complexity check)..."
	@( cd "$(BACKEND_DIR)" && "$(GOCYCLO)" -over 15 . )
	@echo "Go Linting complete!"

run: build
	@"$(BINARY)" \
		--path "/" \
		--name "root" \
		--include-hidden \
		--db-path "/tmp/indexer.db" \
		--socket-path "/tmp/indexer.sock" \
		--listen ":9999" \
		--verbose

run-verbose:
	@set -euo pipefail
	@echo "Running indexer from $(BACKEND_DIR) against / (verbose)"
	@( cd "$(BACKEND_DIR)" && $(GO_BIN) run . -path / -verbose -include-hidden )

build:
	@set -euo pipefail
	@echo "Building indexer binary at $(BINARY)"
	@( cd "$(BACKEND_DIR)" && $(GO_BIN) build -ldflags "$(LDFLAGS)" -o "$(BINARY)" . )
	@size="$$(stat -c '%s' "$(BINARY)")"; \
	 human="$$(ls -lh "$(BINARY)" | awk '{print $$5}')"; \
	 echo "Binary size: $$size bytes ($$human)"

test:
	@set -euo pipefail
	@echo "Running tests from $(BACKEND_DIR)"
	@( cd "$(BACKEND_DIR)" && \
	   tmp="$$(mktemp)"; \
	   trap 'rm -f "$$tmp"' EXIT; \
	   $(GO_BIN) test ./... -timeout 5m -count=1 -run . -json > "$$tmp"; \
	   awk ' \
	     /"Action":"pass"/ && /"Package":/ && !/"Test":/ { match($$0,/"Package":"([^"]+)"/,a); printf("ok      %s\n",a[1]); next } \
	     /"Action":"fail"/ && /"Package":/ && !/"Test":/ { match($$0,/"Package":"([^"]+)"/,a); printf("FAIL    %s\n",a[1]); next } \
	     /"Action":"skip"/ && /"Package":/ && !/"Test":/ { match($$0,/"Package":"([^"]+)"/,a); printf("SKIP    %s\n",a[1]); next } \
	     /"Action":"pass"/ && /"Test":/ { pass++ } \
	     /"Action":"fail"/ && /"Test":/ { fail++ } \
	     /"Action":"skip"/ && /"Test":/ { skip++ } \
	     END { printf("Summary: %d passed, %d failed, %d skipped.\n", pass, fail, skip); exit fail!=0 } \
	   ' "$$tmp" )

benchmark:
	@set -euo pipefail
	@( ./scripts/benchmark.sh )

index:
	@set -euo pipefail
	@(curl --unix-socket /var/run/indexer.sock -X POST http://localhost/index)

status:
	@set -euo pipefail
	@(curl --unix-socket /var/run/indexer.sock http://localhost/status)
