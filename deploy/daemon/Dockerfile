# syntax=docker/dockerfile:1.7

# Matrix v1 daemon image.
#
# Runtime layout inside the container:
#   /opt/matrix/bin/mcl-execute          static go binary (the daemon)
#   /opt/matrix/skills/                  baked-in skill corpus (read-only)
#   /opt/matrix/agents/default.json      MCP server manifest (read-only)
#   /opt/matrix/tools/paxeer/            paxeer-net stdio MCP bridge (Node.js)
#   /opt/matrix/tools/websearch/         web-search stdio MCP bridge (Node.js;
#                                        needs TAVILY_API_KEY or BRAVE_API_KEY in env)
#   /opt/matrix/tools/browser/           browser stdio MCP proxy (Node.js) ->
#                                        shared matrix-browser Fly app via
#                                        MATRIX_BROWSER_URL
#   /opt/matrix/tools/exec/              exec stdio MCP server (Node.js): shell
#                                        + long-lived service supervisor; state
#                                        persisted to /data/services
#   /opt/matrix/tools/tachyon/           tachyon stdio MCP proxy (Node.js) ->
#                                        shared matrix-tachyon Fly app via
#                                        MATRIX_TACHYON_URL (Solidity/EVM engine)
#   /opt/matrix/entrypoint.sh            init script: prep volume + exec daemon
#   /root/matrix -> /opt/matrix          symlink so default.json's hard-coded
#                                        /root/matrix/tools/paxeer/paxeer-net.mjs
#                                        path resolves at runtime
#   /workspace -> /data/workspace        symlink so MCP fs/git see persisted dir
#   /data/                               Fly Volume mount (per-Machine, per-user)
#     /data/cortex/                      Pebble (cortex-root)
#     /data/journal/                     signed envelope journal
#     /data/transcripts/                 per-message JSONL transcripts
#     /data/workspace/                   user's working filesystem
#     /data/services/                    exec service registry + per-service logs
#     /data/.matrix/executor.key         ed25519 actor key (persisted)
#
# MCP servers are pre-cached at build time so cold starts don't hit the
# npm / PyPI registries. Node + Python + uv are installed in the runtime
# stage because the SKILL corpus shells out to npx / uvx per agents/default.json,
# and the paxeer-net bridge runs directly on Node.

# -------- 1. Builder --------
FROM golang:1.22-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Copy the four Go modules. Order chosen so the module manifests land
# first for layer caching.
COPY cortex/go.mod  cortex/go.sum  cortex/
COPY MCL/go.mod     MCL/go.sum     MCL/
COPY bridge/go.mod  bridge/go.sum  bridge/
COPY executor/go.mod executor/go.sum executor/

# Now the source.
COPY cortex/   cortex/
COPY MCL/      MCL/
COPY bridge/   bridge/
COPY executor/ executor/

WORKDIR /src/executor
RUN go build -ldflags="-s -w" -o /out/mcl-execute ./cmd/mcl-execute

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PATH="/root/.local/bin:${PATH}"

# Core runtime deps + Node (for the fs MCP server) + Python (for fetch/git
# MCP servers) + uv (the python tool runner used in agents/default.json).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl git tini \
        python3 python3-pip python3-venv \
        nodejs npm \
        zstd; \
    rm -rf /var/lib/apt/lists/*; \
    pip3 install --break-system-packages --no-cache-dir uv; \
    npm config set update-notifier false; \
    update-ca-certificates

# Pre-install MCP servers so first cold-start does not hit registries.
# Versions PINNED to releases whose tools/list exactly matches default.json.
RUN set -eux; \
    npm install -g @modelcontextprotocol/server-filesystem@2026.1.14; \
    uv tool install mcp-server-fetch==2025.4.7; \
    uv tool install mcp-server-git==2026.1.14; \
    command -v mcp-server-filesystem; \
    command -v mcp-server-fetch; \
    command -v mcp-server-git

# Pre-install the MinIO client (`mc`) for snapshot sync. Phase 3 wires
# this into the daemon as a goroutine; for now `mc` is shelled.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) mc_arch="linux-amd64" ;; \
        arm64) mc_arch="linux-arm64" ;; \
        *) echo "unsupported arch $arch"; exit 1 ;; \
    esac; \
    curl -fsSL "https://dl.min.io/client/mc/release/${mc_arch}/mc" -o /usr/local/bin/mc; \
    chmod +x /usr/local/bin/mc

WORKDIR /opt/matrix

# Copy the static binary first.
COPY --from=builder /out/mcl-execute /opt/matrix/bin/mcl-execute

# Bake the skill corpus + agent manifest + chain-bridge tools. These are
# part of the image identity — same image SHA implies same skill hashes
# and same paxeer-net bridge bytes.
COPY skills/         /opt/matrix/skills/
COPY agents/         /opt/matrix/agents/
COPY tools/paxeer    /opt/matrix/tools/paxeer
COPY tools/websearch /opt/matrix/tools/websearch
COPY tools/browser   /opt/matrix/tools/browser
COPY tools/exec      /opt/matrix/tools/exec
COPY tools/tachyon   /opt/matrix/tools/tachyon

# agents/default.json hard-codes /root/matrix/tools/paxeer/paxeer-net.mjs
# (matches the dev-box layout). Symlink /root/matrix → /opt/matrix so the
# baked-in path resolves inside the container without templating.
RUN mkdir -p /root && ln -sfn /opt/matrix /root/matrix

# Entrypoint: prep volume layout + exec daemon.
COPY deploy/daemon/entrypoint.sh /opt/matrix/entrypoint.sh
RUN chmod +x /opt/matrix/entrypoint.sh

# Health-check probe (Fly will hit /healthz on its own; keep this for
# `docker run` smoke tests).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/healthz >/dev/null || exit 1

EXPOSE 8080

# Fly Volume is mounted at /data; the daemon expects it.
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/matrix/entrypoint.sh"]
CMD ["daemon"]