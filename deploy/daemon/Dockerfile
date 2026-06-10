# syntax=docker/dockerfile:1.7

# Matrix v1 daemon image.
#
# Runtime layout inside the container:
#   /opt/matrix/bin/mcl-execute          static go binary (the MCL daemon, backend :8081)
#   /opt/matrix/bin/neo                  static go binary (the Neo conversational front :8080)
#   /opt/matrix/skills/                  baked-in skill corpus (read-only)
#   /opt/matrix/agents/default.json      MCP manifest — daemon (full, incl. money tools)
#   /opt/matrix/agents/neo.json          MCP manifest — Neo front (reversible-only, no money)
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
#   /opt/matrix/tools/deus/              deus stdio MCP proxy (Node.js) ->
#                                        Deus control plane via MATRIX_DEUS_URL
#   /opt/matrix/entrypoint.sh            init script: prep volume + run (neo front + daemon back)
#   /root/matrix -> /opt/matrix          symlink so default.json's hard-coded
#                                        /root/matrix/tools/paxeer/paxeer-net.mjs
#                                        path resolves at runtime
#   /workspace -> /data/workspace        symlink so MCP fs/git see persisted dir
#   /data/                               Fly Volume mount (per-Machine, per-user)
#     /data/cortex/                      Pebble (cortex-root)
#     /data/journal/                     signed envelope journal
#     /data/transcripts/                 per-message JSONL transcripts
#     /data/workspace/                   user's working filesystem
#     /data/services/                    exec service registry + logs (MCL daemon)
#     /data/neo/services/                exec service registry + logs (Neo front; isolated)
#     /data/.matrix/executor.key         ed25519 actor key (persisted)
#
# MCP servers are pre-cached at build time so cold starts don't hit the
# npm / PyPI registries. Node + Python + uv are installed in the runtime
# stage because the SKILL corpus shells out to npx / uvx per agents/default.json,
# and the paxeer-net bridge runs directly on Node.

# -------- 1. Builder --------
FROM golang:1.26-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Copy the four Go modules. Order chosen so the module manifests land
# first for layer caching.
COPY cortex/go.mod  cortex/go.sum  cortex/
COPY MCL/go.mod     MCL/go.sum     MCL/
COPY bridge/go.mod  bridge/go.sum  bridge/
COPY executor/go.mod executor/go.sum executor/
COPY neo/go.mod     neo/go.sum     neo/

# Now the source.
COPY cortex/   cortex/
COPY MCL/      MCL/
COPY bridge/   bridge/
COPY executor/ executor/
COPY neo/      neo/

WORKDIR /src/executor
RUN go build -ldflags="-s -w" -o /out/mcl-execute ./cmd/mcl-execute

# Neo — the conversational FRONT (matrix/neo). Reuses cortex/MCL/executor via
# the sibling replace directives in neo/go.mod (those trees are copied above).
WORKDIR /src/neo
RUN go build -ldflags="-s -w" -o /out/neo ./cmd/neo

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PATH="/root/.local/bin:${PATH}"

# Core runtime deps + Node (for the fs MCP server) + Python (for fetch/git
# MCP servers) + uv (the python tool runner used in agents/default.json).
# Node MUST be >= 22.12 (NodeSource, NOT Debian bookworm's node 18): the fetch
# MCP server's readabilipy pipeline pins html-encoding-sniffer@6, which
# require()s the ESM-only @exodus/bytes — ERR_REQUIRE_ESM on node 18 broke
# every fetch__fetch article extraction (ExtractArticle.js non-zero exit).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl git tini gnupg \
        python3 python3-pip python3-venv \
        zstd; \
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    rm -rf /var/lib/apt/lists/*; \
    node --version | grep -E '^v2[2-9]\.'; \
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
    command -v mcp-server-git; \
    # readabilipy's node extraction pipeline (what fetch__fetch uses for
    # article mode) needs its OWN npm deps — the wheel ships javascript/
    # package.json but NO node_modules and NO lockfile (it npm-installs lazily
    # at first call, which is why a fresh machine's first fetch was slow/flaky).
    # Install at build time, then smoke-test: ExtractArticle.js must parse HTML
    # and exit 0. (Also requires node >= 22: html-encoding-sniffer@6 resolves
    # here and require()s the ESM-only @exodus/bytes — ERR_REQUIRE_ESM on 18.)
    # NOTE: ExtractArticle.js writes JSON to -o (NOT stdout), and Readability
    # rejects short documents — the probe article must clear its char threshold.
    js_dir="$(find /root/.local/share/uv/tools/mcp-server-fetch -type d -name javascript -path '*readabilipy*' | head -1)"; \
    test -n "$js_dir"; \
    (cd "$js_dir" && npm install --omit=dev --no-audit --no-fund); \
    python3 -c "body = '<p>' + ('readability probe padding sentence for the char threshold. ' * 20) + 'probe-marker</p>'; open('/tmp/extract-probe.html','w').write('<html><head><title>t</title></head><body><article><h1>t</h1>' + body * 3 + '</article></body></html>')"; \
    (cd "$js_dir" && node ExtractArticle.js -i /tmp/extract-probe.html -o /tmp/extract-probe.json); \
    grep -q probe-marker /tmp/extract-probe.json; \
    rm -f /tmp/extract-probe.html /tmp/extract-probe.json

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

# Copy the static binaries first. mcl-execute = the MCL daemon (backend);
# neo = the Neo conversational front (matrix/neo serve).
COPY --from=builder /out/mcl-execute /opt/matrix/bin/mcl-execute
COPY --from=builder /out/neo         /opt/matrix/bin/neo

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
# deus stdio MCP proxy -> Deus control plane via MATRIX_DEUS_URL. Declared in
# agents/default.json; the daemon's strict spawn check exits if it's missing.
COPY tools/deus      /opt/matrix/tools/deus

# agents/default.json hard-codes /root/matrix/tools/paxeer/paxeer-net.mjs
# (matches the dev-box layout). Symlink /root/matrix → /opt/matrix so the
# baked-in path resolves inside the container without templating.
RUN mkdir -p /root && ln -sfn /opt/matrix /root/matrix

# Entrypoint: prep volume layout + exec daemon.
COPY deploy/daemon/entrypoint.sh /opt/matrix/entrypoint.sh
RUN chmod +x /opt/matrix/entrypoint.sh

# Health-check probe. In the default `neo` run mode :8080 is the Neo front,
# which reverse-proxies /healthz to the MCL daemon backend on :8081 — so a
# 200 here proves BOTH processes are up. Start period covers the two boots.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/healthz >/dev/null || exit 1

EXPOSE 8080

# Fly Volume is mounted at /data; the daemon expects it.
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/matrix/entrypoint.sh"]
# Default to the Neo front (Neo :8080 + MCL daemon :8081 co-located). The
# matrix-telegram sidecar overrides this with `daemon` for the solo daemon.
CMD ["neo"]