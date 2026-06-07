SHELL              := /usr/bin/env bash
.SHELLFLAGS        := -eu -o pipefail -c
.DEFAULT_GOAL      := help

GO                 ?= go
BIN_DIR            := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))/bin
FORGE              ?= forge

.PHONY: help
help: ## Show targets.
	@grep -E '^[a-zA-Z0-9_.-]+:.*?##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?##"}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: deps
deps: forge-setup ## Install Foundry deps (forge-std).

.PHONY: forge-setup
forge-setup: ## Install forge-std into lib/.
	@command -v $(FORGE) >/dev/null || { echo "forge not found — run: curl -L https://foundry.paradigm.xyz | bash && foundryup"; exit 1; }
	@if [ ! -d lib/forge-std ]; then \
	  $(FORGE) install foundry-rs/forge-std; \
	fi

.PHONY: build
build: ## Build tachyond and tachyon binaries.
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/tachyond ./cmd/tachyond
	$(GO) build -o $(BIN_DIR)/tachyon ./cmd/tachyon
	@echo "binaries -> $(BIN_DIR)"

.PHONY: run
run: build ## Run the daemon (foreground).
	$(BIN_DIR)/tachyond

.PHONY: test
test: ## Run Go unit tests.
	$(GO) test ./...

.PHONY: forge-build
forge-build: forge-setup ## Compile contracts with forge (skips .t.sol until optional test deps are installed).
	$(FORGE) build --skip test

.PHONY: forge-test
forge-test: forge-setup forge-test-deps ## Run Forge tests.
	$(FORGE) test

.PHONY: forge-test-deps
forge-test-deps: ## Optional OZ test helpers (erc4626-tests, halmos-cheatcodes).
	@if [ ! -d lib/erc4626-tests ]; then $(FORGE) install a16z/erc4626-tests; fi
	@if [ ! -d lib/halmos-cheatcodes ]; then $(FORGE) install a16z/halmos-cheatcodes; fi

.PHONY: mcp-selftest
mcp-selftest: build ## Verify MCP tool list matches docs.
	$(BIN_DIR)/tachyond --selftest

.PHONY: forge-smoke
forge-smoke: forge-test-deps ## Run Create2.t.sol smoke test.
	$(FORGE) test --match-path test/utils/Create2.t.sol

.PHONY: e2e
e2e: build ## End-to-end REST + RPC + MCP smoke (daemon must be running).
	@bash scripts/e2e-smoke.sh

.PHONY: e2e-all
e2e-all: build ## Start daemon, run e2e smoke, stop daemon.
	@pidfile=/tmp/tachyond-e2e.pid; \
	if [ -f $$pidfile ]; then kill $$(cat $$pidfile) 2>/dev/null || true; rm -f $$pidfile; fi; \
	$(BIN_DIR)/tachyond & echo $$! > $$pidfile; \
	sleep 2; \
	bash scripts/e2e-smoke.sh; rc=$$?; \
	kill $$(cat $$pidfile) 2>/dev/null || true; rm -f $$pidfile; \
	exit $$rc

.PHONY: ci
ci: build test mcp-selftest forge-build forge-smoke ## Local CI gate.

.PHONY: clean
clean: ## Remove build artefacts.
	rm -rf $(BIN_DIR) out cache artifacts
