# syntax=docker/dockerfile:1.7

# Matrix v1 gateway image — the metered LLM proxy (credit_ledger + budget caps).
#
# matrix-gateway is a leaf Go module (only github.com/lib/pq); it talks to the
# per-user daemons over HTTP, never via Go imports, so this image builds from
# the gateway/ module alone. The binary is a pure-static CGO_ENABLED=0 build,
# so the runtime layer needs only ca-certificates (HTTPS to the Fireworks /
# Together upstreams) + tini for PID-1 signal forwarding.
#
# Config is env-driven via entrypoint.sh, which maps env -> the same flags the
# box systemd unit passes (gateway/deploy/matrix-gateway.service):
#   MATRIX_GATEWAY_TOKEN          required — shared bearer token (auth.New)
#   FIREWORKS_API_KEY             required when GATEWAY_FREE_TIER_ONLY=true
#   TOGETHER_API_KEY              optional — Together upstream key
#   MATRIX_GATEWAY_POSTGRES_URI   optional — empty selects the in-memory ledger
#   MATRIX_GATEWAY_DISABLED       optional — kill switch (503 except /healthz)
#   GATEWAY_ADDR                  listen addr (default 0.0.0.0:9090)
#   GATEWAY_FREE_TIER_ONLY        default true
#   GATEWAY_DEFAULT_CAP_PAX       default 10
#   GATEWAY_RATE_PER_SEC          default 5
#   GATEWAY_RATE_BURST            default 25
#   GATEWAY_LOG_FORMAT            default json

# -------- 1. Builder --------
FROM golang:1.22-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Module manifest first for layer caching.
COPY gateway/go.mod gateway/go.sum gateway/
WORKDIR /src/gateway
RUN go mod download

# Then the source.
WORKDIR /src
COPY gateway/ gateway/
WORKDIR /src/gateway
RUN go build -ldflags="-s -w" -o /out/matrix-gateway ./cmd/matrix-gateway

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl tini; \
    rm -rf /var/lib/apt/lists/*; \
    update-ca-certificates

COPY --from=builder /out/matrix-gateway /usr/local/bin/matrix-gateway
COPY deploy/gateway/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Loopback default in the systemd unit is fronted by nginx; in a container
# the orchestrator does the port mapping, so entrypoint.sh binds 0.0.0.0:9090.
EXPOSE 9090

# /healthz returns 200 when serving (503 under the kill switch, which is the
# correct "do not route to me" signal). Probe the in-container loopback.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:9090/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["gateway"]
