GO_INSTALL_DIR := $(HOME)/.go
GO_BIN ?= go

# Always treat the project root as the directory containing this makefile,
# regardless of the shell's current working directory.
PROJECT_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BACKEND_DIR ?= $(PROJECT_ROOT)
BIN_DIR ?= $(PROJECT_ROOT)
BINARY_NAME ?= indexer
BINARY := $(BIN_DIR)/$(BINARY_NAME)
GOLANGCI_LINT_MODULE  := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= latest
GOLANGCI_LINT         := $(GO_INSTALL_DIR)/bin/golangci-lint
GOLANGCI_LINT_OPTS ?= --modules-download-mode=mod

.ONESHELL:
SHELL := /bin/bash

.PHONY: ensure-golint golint run build run-root test create-dev dev

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
	     echo "⬇ Installing golangci-lint $(GOLANGCI_LINT_VERSION) (v2) with local Go ($(GO_BIN))..."; \
	     rm -f "$$bin" || true; \
	     PATH="$(GO_INSTALL_DIR)/bin:$$PATH" GOBIN="$(GO_INSTALL_DIR)/bin" GOTOOLCHAIN=local GOFLAGS="-buildvcs=false" \
	       "$(GO_BIN)" install "$(GOLANGCI_LINT_MODULE)@$(GOLANGCI_LINT_VERSION)"; \
	   fi; \
	   "$$bin" version | head -n1; \
	   out="$$( "$$bin" version )"; \
	   ver="$$( printf '%s' "$$out" | sed -n 's/^golangci-lint has version[[:space:]]\([v0-9.]\+\).*/\1/p' )"; \
	   ver_no_v="$${ver#v}"; major="$${ver_no_v%%.*}"; \
	   [ "$$major" = "2" ] || { echo "❌ not a v2 golangci-lint"; exit 1; }; \
	   echo "$$out" | grep -Eq 'built with go1\.25(\.|$$)' || { echo "❌ golangci-lint not built with Go 1.25"; exit 1; }; \
	   echo "✔ golangci-lint v2 ready."; \
	}

golint: ensure-golint
	@set -euo pipefail
	@echo "📁 Linting Go module in: $(BACKEND_DIR)"
	@echo "🔍 Running gofmt..."
ifneq ($(CI),)
	@fmt_out="$$(cd "$(BACKEND_DIR)" && gofmt -s -l .)"; \
	if [ -n "$$fmt_out" ]; then echo "The following files are not gofmt'ed:"; echo "$$fmt_out"; exit 1; fi
else
	@( cd "$(BACKEND_DIR)" && gofmt -s -w . )
endif
	@echo "🔍 Ensuring go.mod is tidy..."
	@( cd "$(BACKEND_DIR)" && go mod tidy && go mod download )
	@echo "🔍 Running golangci-lint..."
	@( cd "$(BACKEND_DIR)" && "$(GOLANGCI_LINT)" run --fix ./... --timeout 3m $(GOLANGCI_LINT_OPTS) )
	@echo "✅ Go Linting Ok!"

run:
	@set -euo pipefail
	@echo "🚀 Running indexer from $(BACKEND_DIR) against / (verbose)"
	@( cd "$(BACKEND_DIR)" && $(GO_BIN) run . -path / -verbose -include-hidden )

build:
	@set -euo pipefail
	@echo "🔧 Building indexer binary at $(BINARY)"
	@mkdir -p "$(BIN_DIR)"
	@( cd "$(BACKEND_DIR)" && $(GO_BIN) build -o "$(BINARY)" . )
	@size="$$(stat -c '%s' "$(BINARY)")"; \
	 human="$$(ls -lh "$(BINARY)" | awk '{print $$5}')"; \
	 echo "📦 Binary size: $$size bytes ($$human)"

test:
	@set -euo pipefail
	@echo "🧪 Running tests from $(BACKEND_DIR)"
	@( cd "$(BACKEND_DIR)" && \
	   tmp="$$(mktemp)"; \
	   trap 'rm -f "$$tmp"' EXIT; \
	   { $(GO_BIN) test ./... -timeout 5m -count=1 2>&1 | grep -v '\[no test files\]'; } | tee "$$tmp"; \
	   passed_pkgs="$$(grep -c '^ok[[:space:]]' "$$tmp" || true)"; \
	   failed_pkgs="$$(grep -c '^FAIL[[:space:]]' "$$tmp" || true)"; \
	   echo "📊 Package summary: $$passed_pkgs ok, $$failed_pkgs failed."; \
	   test "$$failed_pkgs" -eq 0 )

create-dev:
	@set -euo pipefail
	@read -p "Enter new tag name (e.g. v1.2.3): " tag; \
	if [ -z "$$tag" ]; then echo "❌ Tag name is required"; exit 1; fi; \
	if git rev-parse "$$tag" >/dev/null 2>&1; then echo "❌ Ref '$$tag' already exists"; exit 1; fi; \
	echo "🏷  Creating tag '$$tag' at HEAD..."; \
	git tag "$$tag"; \
	default_branch="dev/$$tag"; \
	echo "🌱 Creating branch '$$branch' from tag '$$tag'..."; \
	git checkout -b "$$branch" "$$tag"; \
	echo "✅ Tag '$$tag' and branch '$$branch' created and checked out."
