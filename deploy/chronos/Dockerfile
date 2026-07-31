# syntax=docker/dockerfile:1.7

# Matrix chronos image — the centralized agent scheduler / wake control plane.
#
# chronosd is a leaf Go module (jackc/pgx + robfig/cron); it stores alarms in
# Postgres, fires due ones, and delivers resume turns through the router's
# /internal/wake. The binary is a pure-static CGO_ENABLED=0 build, so the
# runtime layer needs only ca-certificates + tini. Migrations ship beside the
# binary and chronosd applies them at boot (CHRONOS_ROOT resolves the relative
# migrations dir, mirroring the systemd deployment layout).
#
# Config is env-first (chronos/internal/config; optional CHRONOS_CONFIG kvx
# overlay):
#   CHRONOS_POSTGRES_URI      required — postgres://... (alarm store)
#   CHRONOS_TOKEN             transport bearer (unset = auth off; private-net only)
#   CHRONOS_AGENT_AUTH_SECRET agent challenge/token secret
#   CHRONOS_ROUTER_WAKE_URL   router /internal/wake base URL
#   CHRONOS_WAKE_TOKEN        bearer for the router wake endpoint
#   CHRONOS_PORT              listen port (default 9096; Go ":port" binds
#                             dual-stack, so IPv6-only private networks work)
#   CHRONOS_TICK_MS / CHRONOS_MAX_FAILURES / CHRONOS_DEV — see config.go

# -------- 1. Builder --------
# chronos/go.mod requires Go >= 1.25.0 (workforce Chronos wake target); the
# builder base MUST be >= that or `go build ./cmd/chronosd` fails with
# GOTOOLCHAIN=local.
FROM golang:1.25-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Module manifests first for layer caching. chronos pulls in the workforce
# Chronos wake target via `replace matrix/workforce => ../workforce`, which in
# turn pulls in vault (matrix/vault, pure-stdlib, no go.sum) via
# `replace matrix/vault => ../vault`, so both trees MUST be present or
# `go build ./cmd/chronosd` fails to resolve the replace targets.
COPY vault/go.mod                    vault/
COPY workforce/go.mod workforce/go.sum workforce/
COPY chronos/go.mod   chronos/go.sum   chronos/
WORKDIR /src/chronos
RUN go mod download

# Then the source.
WORKDIR /src
COPY vault/     vault/
COPY workforce/ workforce/
COPY chronos/   chronos/
WORKDIR /src/chronos
RUN go build -ldflags="-s -w" -o /out/chronosd ./cmd/chronosd

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    CHRONOS_ROOT=/opt/matrix-chronos

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl tini; \
    rm -rf /var/lib/apt/lists/*; \
    update-ca-certificates

COPY --from=builder /out/chronosd       /opt/matrix-chronos/chronosd
COPY chronos/migrations                 /opt/matrix-chronos/migrations

EXPOSE 9096

# /healthz is unauthenticated. Override the probe port to match CHRONOS_PORT
# via CHRONOS_HEALTHCHECK_PORT when it is not the 9096 default.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${CHRONOS_HEALTHCHECK_PORT:-9096}/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/matrix-chronos/chronosd"]
