#!/usr/bin/env bash
# scaffold-go.sh — production Go service (standard project layout).
# Tooling: golangci-lint v2 · air (hot reload) · Makefile · distroless Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "go" "$@"
require_cmd go
common_init_target
step "Go service → $PROJECT_SLUG"

MODULE="${SCAFFOLD_MODULE:-github.com/${SCAFFOLD_VCS_ORG}/${PROJECT_SLUG}}"
GO_VERSION="$(go env GOVERSION 2>/dev/null | sed 's/^go//' | cut -d. -f1,2)"
: "${GO_VERSION:=1.23}"
APP="$PROJECT_SLUG"

mkdir -p "cmd/${APP}" internal/server internal/config pkg api configs scripts test

write_if_absent go.mod <<EOF
module ${MODULE}

go ${GO_VERSION}
EOF

write_if_absent "cmd/${APP}/main.go" <<EOF
// Command ${APP} is the service entrypoint.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"${MODULE}/internal/config"
	"${MODULE}/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg := config.Load()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
EOF

write_if_absent internal/config/config.go <<'EOF'
// Package config loads runtime configuration from the environment.
package config

import "os"

type Config struct {
	Addr string
}

func Load() Config {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return Config{Addr: addr}
}
EOF

write_if_absent internal/server/server.go <<'EOF'
// Package server wires the HTTP router.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

func New(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return logging(logger, mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func logging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
EOF

write_if_absent internal/server/server_test.go <<'EOF'
package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	srv := New(slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want %d", rec.Code, http.StatusOK)
	}
}
EOF

# --- golangci-lint v2 -------------------------------------------------------
write_if_absent .golangci.yml <<'EOF'
version: "2"
run:
  timeout: 5m
linters:
  default: standard
  enable:
    - bodyclose
    - errcheck
    - errorlint
    - gocritic
    - gosec
    - govet
    - ineffassign
    - misspell
    - revive
    - staticcheck
    - unconvert
    - unparam
    - unused
formatters:
  enable:
    - gofumpt
    - goimports
EOF

# --- air (hot reload) -------------------------------------------------------
write_if_absent .air.toml <<EOF
root = "."
tmp_dir = "tmp"

[build]
  cmd = "go build -o ./tmp/main ./cmd/${APP}"
  bin = "./tmp/main"
  include_ext = ["go"]
  exclude_dir = ["tmp", "vendor", "test", "docs"]
  delay = 200
EOF

# --- Makefile ---------------------------------------------------------------
write_if_absent Makefile <<EOF
APP := ${APP}
PKG := ./...

.PHONY: help build run dev test cover lint fmt tidy docker clean
help: ## show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' \$(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n",\$\$1,\$\$2}'

build: ## build binary
	go build -o bin/\$(APP) ./cmd/\$(APP)

run: build ## build + run
	./bin/\$(APP)

dev: ## hot reload (requires air)
	air

test: ## run tests
	go test -race -count=1 \$(PKG)

cover: ## tests with coverage
	go test -race -coverprofile=coverage.out \$(PKG) && go tool cover -func=coverage.out

lint: ## golangci-lint
	golangci-lint run

fmt: ## format
	gofumpt -w . && goimports -w .

tidy: ## tidy modules
	go mod tidy

docker: ## build container image
	docker build -t \$(APP):latest .

clean:
	rm -rf bin tmp coverage.out
EOF

# --- Dockerfile (distroless) ------------------------------------------------
write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/${APP} ./cmd/${APP}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/${APP} /${APP}
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/${APP}"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "go" "/bin/
/tmp/
*.out
vendor/"

gen_github_ci "$(cat <<YAML
name: ci
on:
  push: { branches: [main] }
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '${GO_VERSION}', cache: true }
      - run: go build ./...
      - run: go test -race -count=1 ./...
      - uses: golangci/golangci-lint-action@v6
        with: { version: latest }
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "Go service" \
  "go mod download" "make dev" "make build" "make test" "make lint"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then
  info "resolving modules"; go mod tidy || warn "go mod tidy failed (no network?)"
fi

finalize_git
common_done "Go service · module ${MODULE}"
