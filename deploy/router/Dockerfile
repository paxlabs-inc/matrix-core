# syntax=docker/dockerfile:1.7

# Matrix v1 router image — the front door for Fly daemon Machines.
#
# matrix-router is a leaf Go module (jackc/pgx + golang.org/x/crypto); it
# verifies Supabase JWTs, reverse-proxies authenticated requests to per-user
# Fly Machines, and exposes an admin provisioning API. The binary is a
# pure-static CGO_ENABLED=0 build, so the runtime layer needs only
# ca-certificates (HTTPS to Supabase JWKS + the Fly Machines API) + tini.
#
# Fully env-driven (router/internal/config.Load); there are no flags. Required:
#   ROUTER_ADDR            public listen addr (e.g. :443 or :8080)
#   ROUTER_INTERNAL_ADDR   admin/health listen addr (e.g. :8088)
#   SUPABASE_URL           project URL (JWKS verification)
#   FLY_API_TOKEN          api.machines.dev bearer
#   FLY_APP_NAME           e.g. matrix-daemon
#   DATABASE_URL           postgres://...  (user + provision-job state)
# Optional set (ROUTER_ADMIN_TOKEN, MATRIX_GATEWAY_URL/TOKEN, MATRIX_S3_*,
# ROUTER_DAEMON_IMAGE, model pins, timeouts) — see config.go for the full list.

ARG VERSION=dev

# -------- 1. Builder --------
FROM golang:1.21-bookworm AS builder
ARG VERSION

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Module manifest first for layer caching.
COPY router/go.mod router/go.sum router/
WORKDIR /src/router
RUN go mod download

# Then the source. main.version is stamped via -ldflags so /v/version and the
# boot log report the build identity (the router logs version= on startup and
# the box install.sh greps for it).
WORKDIR /src
COPY router/ router/
WORKDIR /src/router
RUN go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/matrix-router ./cmd/matrix-router

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl tini; \
    rm -rf /var/lib/apt/lists/*; \
    update-ca-certificates

COPY --from=builder /out/matrix-router /usr/local/bin/matrix-router

# Public + internal listeners; the operator maps the real external ports.
EXPOSE 8080 8088

# Probe the internal listener's /healthz (always 200 when up; the public
# /healthz additionally pings Postgres). Override the port to match
# ROUTER_INTERNAL_ADDR via ROUTER_HEALTHCHECK_PORT when it is not :8088.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${ROUTER_HEALTHCHECK_PORT:-8088}/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/matrix-router"]
