GO_INSTALL_DIR := $(HOME)/.go
GO_VERSION ?= $(shell awk '/^go / {print $$2; exit}' go.mod)
GO_MAJOR_MINOR := $(shell printf '%s' "$(GO_VERSION)" | cut -d. -f1,2)
GO_BIN ?= go
BACKEND_DIR ?= $(CURDIR)
BINARY_NAME ?= indexer
BINARY := $(CURDIR)/$(BINARY_NAME)
CACHE_ROOT := $(CURDIR)/.cache
export GOMODCACHE := $(CACHE_ROOT)/gomod
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "")
LDFLAGS ?= -X github.com/mordilloSan/indexer/internal/version.Version=$(VERSION) -X github.com/mordilloSan/indexer/internal/version.Commit=$(GIT_COMMIT) -X github.com/mordilloSan/indexer/internal/version.Date=$(BUILD_DATE)
GOAMD64 ?= v3
GO_BUILD_FLAGS ?= -trimpath -buildvcs=false
GO_LDSTRIP ?= -s -w
GOLANGCI_LINT_MODULE  := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= latest
GOLANGCI_LINT         := $(GO_INSTALL_DIR)/bin/golangci-lint
GOLANGCI_LINT_GO_MINOR ?= $(GO_MAJOR_MINOR)
GOLANGCI_LINT_OPTS ?= --modules-download-mode=mod
MODERNIZE_MODULE      := golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize
MODERNIZE_VERSION     ?= latest
MODERNIZE             := $(GO_INSTALL_DIR)/bin/modernize
GOCYCLO               := $(GO_INSTALL_DIR)/bin/gocyclo
GOCYCLO_MAX_COMPLEXITY ?= 20

.ONESHELL:
SHELL := /bin/bash

.PHONY: ensure-golint ensure-modernize ensure-gocyclo modernize golint golint-only run run-verbose build build-only test benchmark pgo compare-pgo index status localinstall

ensure-golint:
	@{ set -euo pipefail; \
	   bin="$(GOLANGCI_LINT)"; need=1; \
	   if [ -x "$$bin" ]; then \
	     out="$$( "$$bin" version 2>/dev/null || true)"; \
	     ver="$$( printf '%s' "$$out" | sed -n 's/^golangci-lint has version[[:space:]]\([v0-9.]\+\).*/\1/p' )"; \
	     ver_no_v="$${ver#v}"; major="$${ver_no_v%%.*}"; \
	     built_ok="$$( printf '%s' "$$out" | grep -Eq 'built with go$(GOLANGCI_LINT_GO_MINOR)(\.|$$)' && echo yes || echo no )"; \
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
	   echo "$$out" | grep -Eq 'built with go$(GOLANGCI_LINT_GO_MINOR)(\.|$$)' || { echo "ERROR: golangci-lint not built with Go $(GOLANGCI_LINT_GO_MINOR)"; exit 1; }; \
	   echo "golangci-lint v2 ready."; \
	}

ensure-gocyclo:
	@if [ ! -x "$(GOCYCLO)" ]; then \
	  echo "Installing gocyclo..."; \
	  GOBIN="$(GO_INSTALL_DIR)/bin" "$(GO_BIN)" install github.com/fzipp/gocyclo/cmd/gocyclo@latest; \
	fi
	@echo "gocyclo ready."

ensure-modernize:
	@if [ ! -x "$(MODERNIZE)" ]; then \
	  echo "Installing modernize..."; \
	  PATH="$(GO_INSTALL_DIR)/bin:$$PATH" GOBIN="$(GO_INSTALL_DIR)/bin" GOTOOLCHAIN=local GOFLAGS="-buildvcs=false" \
	    "$(GO_BIN)" install "$(MODERNIZE_MODULE)@$(MODERNIZE_VERSION)"; \
	fi
	@echo "modernize ready."

modernize: ensure-modernize
	@set -euo pipefail
	@echo "Running modernize..."
	@( cd "$(BACKEND_DIR)" && "$(MODERNIZE)" -fix ./... )

golint: ensure-golint ensure-modernize ensure-gocyclo
	@$(MAKE) --no-print-directory golint-only

golint-only:
	@set -euo pipefail
	@echo "Linting Go module in: $(BACKEND_DIR)"
	@echo "Running gofmt..."
ifneq ($(CI),)
	@fmt_out="$$(cd "$(BACKEND_DIR)" && { \
		mapfile -t files < <(find . \( -path './.git' -o -path './.cache' \) -prune -o -type f -name '*.go' -print); \
		if [ "$${#files[@]}" -gt 0 ]; then gofmt -s -l "$${files[@]}"; fi; \
	})"; \
	if [ -n "$$fmt_out" ]; then echo "The following files are not gofmt'ed:"; echo "$$fmt_out"; exit 1; fi
else
	@( cd "$(BACKEND_DIR)" && \
	   mapfile -t files < <(find . \( -path './.git' -o -path './.cache' \) -prune -o -type f -name '*.go' -print); \
	   if [ "$${#files[@]}" -gt 0 ]; then gofmt -s -w "$${files[@]}"; fi )
endif
	@echo "Ensuring go.mod is tidy..."
	@( cd "$(BACKEND_DIR)" && go mod tidy && go mod download )
	@echo "Running modernize..."
	@( cd "$(BACKEND_DIR)" && "$(MODERNIZE)" -fix ./... )
	@echo "Running golangci-lint..."
	@( cd "$(BACKEND_DIR)" && "$(GOLANGCI_LINT)" run --fix ./... --timeout 3m $(GOLANGCI_LINT_OPTS) )
	@echo "Running gocyclo (complexity check)..."
	@( cd "$(BACKEND_DIR)" && \
	   mapfile -t files < <(find . \( -path './.git' -o -path './.cache' \) -prune -o -type f -name '*.go' ! -name '*_test.go' -print); \
	   if [ "$${#files[@]}" -gt 0 ]; then "$(GOCYCLO)" -over "$(GOCYCLO_MAX_COMPLEXITY)" "$${files[@]}"; fi )
	@echo "Go Linting complete!"

run: build-only
	@"$(BINARY)" daemon \
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
	@( cd "$(BACKEND_DIR)" && $(GO_BIN) run . daemon -path / -verbose -include-hidden )

build: golint test build-only

build-only:
	@set -euo pipefail
	@echo "Building indexer binary at $(BINARY)"
	@( cd "$(BACKEND_DIR)" && \
	   GOAMD64="$(GOAMD64)" \
	   $(GO_BIN) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDSTRIP) $(LDFLAGS)" -o "$(BINARY)" . )
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
	     /"Action":"pass"/ && /"Package":/ && !/"Test":/ { pkg=$$0; sub(/.*"Package":"/,"",pkg); sub(/".*/,"",pkg); printf("ok      %s\n",pkg); next } \
	     /"Action":"fail"/ && /"Package":/ && !/"Test":/ { pkg=$$0; sub(/.*"Package":"/,"",pkg); sub(/".*/,"",pkg); printf("FAIL    %s\n",pkg); next } \
	     /"Action":"skip"/ && /"Package":/ && !/"Test":/ { pkg=$$0; sub(/.*"Package":"/,"",pkg); sub(/".*/,"",pkg); printf("SKIP    %s\n",pkg); next } \
	     /"Action":"pass"/ && /"Test":/ { pass++ } \
	     /"Action":"fail"/ && /"Test":/ { fail++ } \
	     /"Action":"skip"/ && /"Test":/ { skip++ } \
	     END { printf("Summary: %d passed, %d failed, %d skipped.\n", pass, fail, skip); exit fail!=0 } \
	   ' "$$tmp" )

benchmark:
	@set -euo pipefail
	@( ./scripts/benchmark.sh )

pgo:
	@set -euo pipefail
	@if [ "$${PGO_USE_SUDO:-$${PGO_MODE:-real}}" != "synthetic" ]; then sudo -v; fi
	@( ./scripts/generate_pgo.sh )

compare-pgo:
	@set -euo pipefail
	@if [ "$${COMPARE_PGO_USE_SUDO:-true}" = "true" ]; then sudo -v; fi
	@( ./scripts/compare_pgo.sh )

localinstall: build-only
	@set -euo pipefail
	@sudo ./scripts/local_install.sh

index:
	@set -euo pipefail
	@(curl --unix-socket /var/run/indexer.sock -X POST http://localhost/index)

status:
	@set -euo pipefail
	@(curl --unix-socket /var/run/indexer.sock http://localhost/status)
