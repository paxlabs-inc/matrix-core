# Matrix — root Makefile
# Drives the four sibling Go modules (cortex/, MCL/, bridge/, executor/) plus
# repo-wide tasks (lint, fmt, e2e, deploy artefacts).
#
# Style: zero implicit magic. Every target is grep-able and self-contained.
# Convention: per-module targets fan out via $(MODULES); top-level targets
# aggregate. No target ever cd's silently — modules are explicit.

SHELL              := /usr/bin/env bash
.SHELLFLAGS        := -eu -o pipefail -c
.DEFAULT_GOAL      := help

MODULES            := MCL bridge executor gateway router cortex tachyon deus
GO                 ?= go
GOFLAGS            ?=
GOTEST_FLAGS       ?= -count=1
GOLANGCI_LINT      ?= golangci-lint
GOLANGCI_VERSION   ?= v1.61.0

REPO_ROOT          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BIN_DIR            := $(REPO_ROOT)/bin
COVERAGE_DIR       := $(REPO_ROOT)/coverage

# Colours (disabled when not a TTY).
ifeq ($(shell [ -t 1 ] && echo tty),tty)
  C_RESET := \033[0m
  C_BOLD  := \033[1m
  C_BLUE  := \033[38;5;33m
  C_GREEN := \033[38;5;42m
  C_RED   := \033[38;5;160m
  C_DIM   := \033[2m
else
  C_RESET :=
  C_BOLD  :=
  C_BLUE  :=
  C_GREEN :=
  C_RED   :=
  C_DIM   :=
endif

define HEADER
	@printf "$(C_BOLD)$(C_BLUE)==> %s$(C_RESET)\n" "$(1)"
endef

# ---------------------------------------------------------------------------
# Meta
# ---------------------------------------------------------------------------

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\n  $(C_BOLD)Matrix$(C_RESET) — pair-programmer monorepo\n\n  $(C_BOLD)Usage:$(C_RESET) make $(C_DIM)<target>$(C_RESET)\n\n  $(C_BOLD)Targets:$(C_RESET)\n"} /^[a-zA-Z0-9_.\/-]+:.*?##/ { printf "    $(C_BLUE)%-22s$(C_RESET) %s\n", $$1, $$2 } /^##@/ { printf "\n  $(C_BOLD)%s$(C_RESET)\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@printf "\n  $(C_DIM)Modules: $(MODULES)$(C_RESET)\n\n"

.PHONY: version
version: ## Print toolchain versions.
	$(call HEADER,Toolchain)
	@$(GO) version
	@$(GO) env GOPATH GOCACHE GOMODCACHE | sed 's/^/  /'
	@command -v $(GOLANGCI_LINT) >/dev/null && $(GOLANGCI_LINT) --version || printf "  $(C_DIM)golangci-lint not installed$(C_RESET)\n"
	@command -v docker          >/dev/null && docker --version          || printf "  $(C_DIM)docker not installed$(C_RESET)\n"

# ---------------------------------------------------------------------------
##@ Build
# ---------------------------------------------------------------------------

.PHONY: build
build: $(addprefix build/,$(MODULES)) ## Build every module.

build/%: ## Build one module (e.g. build/cortex).
	$(call HEADER,build $*)
	@$(GO) -C $* build $(GOFLAGS) ./...

.PHONY: tidy
tidy: $(addprefix tidy/,$(MODULES)) ## Run `go mod tidy` per module.

tidy/%:
	$(call HEADER,tidy $*)
	@$(GO) -C $* mod tidy

.PHONY: install
install: ## Install runnable binaries into ./bin.
	$(call HEADER,install binaries)
	@mkdir -p $(BIN_DIR)
	@$(GO) -C cortex   build -o $(BIN_DIR)/cortex-shell    ./cmd/cortex-shell
	@$(GO) -C MCL      build -o $(BIN_DIR)/mclc            ./cmd/mclc
	@$(GO) -C bridge   build -o $(BIN_DIR)/mclc-cortex     ./cmd/mclc-cortex
	@$(GO) -C executor build -o $(BIN_DIR)/mcl-execute     ./cmd/mcl-execute
	@$(GO) -C executor build -o $(BIN_DIR)/mcl-tools       ./cmd/mcl-tools
	@$(GO) -C executor build -o $(BIN_DIR)/mcl-e2e         ./cmd/mcl-e2e
	@$(GO) -C gateway  build -o $(BIN_DIR)/matrix-gateway  ./cmd/matrix-gateway
	@$(GO) -C router   build -o $(BIN_DIR)/matrix-router   ./cmd/matrix-router
	@printf "  $(C_GREEN)binaries$(C_RESET) -> $(BIN_DIR)\n"
	@ls -1 $(BIN_DIR) | sed 's/^/    /'

# ---------------------------------------------------------------------------
##@ Test
# ---------------------------------------------------------------------------

.PHONY: test
test: $(addprefix test/,$(MODULES)) ## Run unit tests for every module.

test/%: ## Run tests for one module (e.g. test/cortex).
	$(call HEADER,test $*)
	@$(GO) -C $* test $(GOTEST_FLAGS) $(GOFLAGS) ./...

.PHONY: test-short
test-short: ## Run tests with -short across all modules.
	@for m in $(MODULES); do \
	  printf "$(C_BOLD)$(C_BLUE)==> test-short $$m$(C_RESET)\n"; \
	  $(GO) -C $$m test -short $(GOTEST_FLAGS) $(GOFLAGS) ./...; \
	done

.PHONY: race
race: ## Run race detector across all modules.
	@for m in $(MODULES); do \
	  printf "$(C_BOLD)$(C_BLUE)==> race $$m$(C_RESET)\n"; \
	  $(GO) -C $$m test -race $(GOTEST_FLAGS) $(GOFLAGS) ./...; \
	done

.PHONY: cover
cover: ## Generate per-module coverage profiles under ./coverage.
	@mkdir -p $(COVERAGE_DIR)
	@for m in $(MODULES); do \
	  printf "$(C_BOLD)$(C_BLUE)==> cover $$m$(C_RESET)\n"; \
	  $(GO) -C $$m test $(GOTEST_FLAGS) $(GOFLAGS) -covermode=atomic -coverprofile=$(COVERAGE_DIR)/$$m.out ./...; \
	done
	@printf "  $(C_GREEN)coverage profiles$(C_RESET) -> $(COVERAGE_DIR)\n"

# ---------------------------------------------------------------------------
##@ Quality
# ---------------------------------------------------------------------------

.PHONY: vet
vet: $(addprefix vet/,$(MODULES)) ## Run `go vet ./...` per module.

vet/%:
	$(call HEADER,vet $*)
	@$(GO) -C $* vet $(GOFLAGS) ./...

.PHONY: fmt
fmt: ## Run gofmt -s -w across modules.
	$(call HEADER,gofmt)
	@for m in $(MODULES); do \
	  gofmt -s -w $$m; \
	done

.PHONY: fmt-check
fmt-check: ## Fail if any Go source needs gofmt.
	$(call HEADER,gofmt -l)
	@unformatted=$$(for m in $(MODULES); do gofmt -l $$m; done); \
	if [ -n "$$unformatted" ]; then \
	  printf "$(C_RED)needs gofmt:$(C_RESET)\n%s\n" "$$unformatted"; \
	  exit 1; \
	fi

.PHONY: lint
lint: ## Run golangci-lint on every module.
	@command -v $(GOLANGCI_LINT) >/dev/null || { \
	  printf "$(C_RED)golangci-lint not installed$(C_RESET)\n  install: $$GOPATH/bin or 'make lint-install'\n"; exit 1; }
	@for m in $(MODULES); do \
	  printf "$(C_BOLD)$(C_BLUE)==> lint $$m$(C_RESET)\n"; \
	  ( cd $$m && $(GOLANGCI_LINT) run --config=$(REPO_ROOT)/.golangci.yml ./... ); \
	done

.PHONY: lint-install
lint-install: ## Install golangci-lint into $GOPATH/bin.
	$(call HEADER,install golangci-lint $(GOLANGCI_VERSION))
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
	  | sh -s -- -b $$($(GO) env GOPATH)/bin $(GOLANGCI_VERSION)

.PHONY: ci
ci: fmt-check vet test ## Full CI gate locally (matches .github/workflows/ci.yml).
	@printf "$(C_BOLD)$(C_GREEN)ci ok$(C_RESET)\n"

# ---------------------------------------------------------------------------
##@ E2E / smokes
# ---------------------------------------------------------------------------

.PHONY: e2e
e2e: ## Run the live mcl-e2e harness (requires FIREWORKS_API_KEY + TOGETHER_API_KEY).
	$(call HEADER,e2e)
	@[ -n "$$FIREWORKS_API_KEY" ] || { printf "$(C_RED)FIREWORKS_API_KEY not set$(C_RESET)\n"; exit 1; }
	@[ -n "$$TOGETHER_API_KEY" ]  || { printf "$(C_RED)TOGETHER_API_KEY not set$(C_RESET)\n";  exit 1; }
	@$(GO) -C executor run ./cmd/mcl-e2e

.PHONY: e2e-sweep
e2e-sweep: ## Run the sweep harness wrapper at tools/e2e/run_sweep.sh.
	@tools/e2e/run_sweep.sh

# ---------------------------------------------------------------------------
##@ Deploy
# ---------------------------------------------------------------------------

.PHONY: docker-daemon
docker-daemon: ## Build the daemon container image (deploy/daemon/Dockerfile).
	$(call HEADER,docker build matrix-daemon:dev)
	@docker build -f deploy/daemon/Dockerfile -t matrix-daemon:dev .

.PHONY: docker-daemon-run
docker-daemon-run: ## Run the daemon container locally (binds 8080 + mounts ./runs/data).
	@mkdir -p $(REPO_ROOT)/runs/data
	@docker run --rm -it \
	  -p 8080:8080 \
	  -v $(REPO_ROOT)/runs/data:/data \
	  --env-file $(REPO_ROOT)/.env \
	  matrix-daemon:dev

# ---------------------------------------------------------------------------
##@ Housekeeping
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build artefacts, coverage, bin/, transient runs/.
	$(call HEADER,clean)
	@rm -rf $(BIN_DIR) $(COVERAGE_DIR)
	@rm -f cortex/cortex-shell cortex/two-model-smoke cortex/embed-smoke
	@rm -f MCL/mclc MCL/mcl-fmt MCL/mcl-validate
	@rm -f bridge/mclc-cortex
	@rm -f executor/mcl-execute executor/mcl-e2e executor/mcl-tools
	@rm -f gateway/matrix-gateway
	@rm -f router/matrix-router

.PHONY: verify-modules
verify-modules: ## Sanity-check every module's go.mod (version + module path).
	@for m in $(MODULES); do \
	  printf "$(C_BOLD)$(C_BLUE)==> $$m/go.mod$(C_RESET)\n"; \
	  head -3 $$m/go.mod | sed 's/^/  /'; \
	done

# Block accidental top-level `go` calls inside a module by routing through
# `go -C <module>`. CI and local devs should run `make test` rather than
# `go test ./...` from the repo root.
