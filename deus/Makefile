# Deus — module Makefile (see docs/11-modules.md §11.6)

SHELL       := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

REPO_ROOT   := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
MATRIX_ROOT := $(abspath $(REPO_ROOT)/..)
GO          ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: help
help: ## Show targets.
	@grep -E '^[a-zA-Z0-9_.-]+:.*?##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?##"}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: deus-build
deus-build: ## go build ./...
	@$(GO) build ./...

.PHONY: deus-test
deus-test: ## go test ./...
	@$(GO) test -count=1 ./...

.PHONY: deus-lint
deus-lint: ## golangci-lint (root config).
	@command -v $(GOLANGCI_LINT) >/dev/null || { echo "golangci-lint not installed; run: make lint-install from matrix root"; exit 1; }
	@$(GOLANGCI_LINT) run --config=$(MATRIX_ROOT)/.golangci.yml ./...

.PHONY: deus-migrate
deus-migrate: deus-build ## Run deusctl migrate.
	@$(GO) run ./cmd/deusctl migrate

.PHONY: deus-contracts
deus-contracts: ## forge build && forge test (contracts/).
	@command -v forge >/dev/null || { echo "foundry forge not installed"; exit 1; }
	@cd contracts && forge build && forge test

.PHONY: deus-mcp-selftest
deus-mcp-selftest: ## node tools/deus/deus.mjs --selftest
	@node $(MATRIX_ROOT)/tools/deus/deus.mjs --selftest

.PHONY: install
install: deus-build ## Install deusd and deusctl into ../bin.
	@mkdir -p $(MATRIX_ROOT)/bin
	@$(GO) build -o $(MATRIX_ROOT)/bin/deusd ./cmd/deusd
	@$(GO) build -o $(MATRIX_ROOT)/bin/deusctl ./cmd/deusctl
