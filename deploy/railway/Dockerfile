# syntax=docker/dockerfile:1.7

# Matrix Railway dev-environment image.
#
# The Railway sibling of deploy/daemon/Dockerfile: the SAME daemon stack
# (mcl-execute + neo, baked skills/agents/tools, pre-cached MCP servers, mc)
# on an Ubuntu 24.04 base that additionally bakes the full dev toolchain the
# per-user environment shells into via the exec MCP server:
#
#   git, build-essential, Node 22 + pnpm, Go, Python 3.12 + uv, Rust, Foundry
#
# For Neo's coding workbench this image also bakes:
#   - Local data services as native server binaries (NOT docker-in-docker):
#     PostgreSQL, Redis, SQLite — started on demand by the exec bridge.
#   - The quality toolchain Neo verifies with: golangci-lint, ruff, eslint,
#     prettier, tsc, vitest, cargo-clippy.
#   - A PER-USER Playwright (@playwright/mcp + chromium) baked in and run
#     in-container by the entrypoint (BROWSER-FILMSTRIP req.1): navigation and
#     per-navigation screenshot capture are local and cheap on the dedicated
#     Railway machine, replacing the shelved shared Fly matrix-browser app.
#     MATRIX_BROWSER_URL points at the local instance; the browser.mjs bridge
#     forwards to it unchanged in wire shape.
#
# Railway specifics (vs the Fly image):
#   - ONE volume: Railway allows exactly one volume mount per service. It
#     mounts /data; /workspace stays a symlink to /data/workspace (the
#     entrypoint maintains it) so the MCP fs/git/exec root is unchanged.
#   - Dual-stack listeners: the Railway private network is IPv6-only. Go's
#     net.Listen(":port") binds dual-stack, so mcl-execute/neo accept ::
#     traffic without flag changes.
#   - Wake-on-request: the router injects MATRIX_SNAPSHOT_INTERVAL=-1s so the
#     periodic snapshot push ticker stays off and Serverless sleep engages;
#     BootPull from MinIO is retained as the volume-seed / Fly→Railway
#     migration vehicle.
#
# Runtime layout inside the container is identical to deploy/daemon/Dockerfile
# (see its header comment for the /opt/matrix + /data map).

# -------- 1. Builder --------
# neo/go.mod requires Go >= 1.25.0 (the other modules are 1.21); the builder
# base MUST be >= that or `go build ./cmd/neo` fails with GOTOOLCHAIN=local.
FROM golang:1.25-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

# Copy the Go modules. Order chosen so the module manifests land first for
# layer caching. construct is a pure-stdlib module (no go.sum) that both
# executor and neo pull in via `replace matrix/construct => ../construct`
# (the Construct projection engine + transport + Ask back-channel), so its
# tree MUST be present or `go build` can't resolve the replace target.
# vault (matrix/vault) — the ORACLE data-at-rest envelope-encryption module.
# Pure-stdlib (no go.sum). cortex, neo, executor, construct, bridge, and
# codegraph all pull it in via `replace matrix/vault => ../vault`, so its
# tree MUST be present or every downstream `go build` fails to resolve the
# replace target.
COPY vault/go.mod                  vault/
COPY cortex/go.mod  cortex/go.sum  cortex/
COPY MCL/go.mod     MCL/go.sum     MCL/
# codegraph (matrix/codegraph) — neo pulls it in via `replace matrix/codegraph
# => ../codegraph` (the self-model structural self-graph). It in turn replaces
# ../cortex and ../MCL (copied above), so its tree MUST be present for
# `go build ./cmd/neo` to resolve.
COPY codegraph/go.mod codegraph/go.sum codegraph/
COPY bridge/go.mod  bridge/go.sum  bridge/
COPY construct/go.mod              construct/
COPY executor/go.mod executor/go.sum executor/
COPY neo/go.mod     neo/go.sum     neo/
# cassandra (matrix/cassandra) — imported by the executor's critic stage
# (cmd/mcl-execute/critique.go), so its tree MUST be present for the daemon
# build. Neo no longer imports it (Cassandra 2.0 is in-process in neo).
COPY cassandra/go.mod              cassandra/

# Now the source.
COPY vault/     vault/
COPY cortex/    cortex/
COPY MCL/       MCL/
COPY codegraph/ codegraph/
COPY bridge/    bridge/
COPY construct/ construct/
COPY executor/  executor/
COPY neo/       neo/
COPY cassandra/ cassandra/

WORKDIR /src/executor
RUN go build -ldflags="-s -w" -o /out/mcl-execute ./cmd/mcl-execute

# Neo — the conversational FRONT (matrix/neo). Reuses cortex/MCL/executor via
# the sibling replace directives in neo/go.mod (those trees are copied above).
WORKDIR /src/neo
RUN go build -ldflags="-s -w" -o /out/neo ./cmd/neo

# -------- 2. Runtime --------
FROM ubuntu:24.04 AS runtime

ARG TARGETARCH
# Dev toolchain pins. Override at build time to bump.
ARG GO_VERSION=1.22.5
ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH="/root/.local/bin:/usr/local/go/bin:/usr/local/cargo/bin:/usr/local/foundry/bin:${PATH}"

# Core runtime deps + dev toolchain base + Node (for the fs MCP server AND the
# user's dev shell) + Python (for fetch/git MCP servers AND the user's dev
# shell) + uv. Ubuntu 24.04 ships Python 3.12; pip needs --break-system-packages
# (PEP 668) exactly like bookworm.
# Node MUST be >= 22.12 (NodeSource, NOT Ubuntu's node): the fetch MCP
# server's readabilipy pipeline pins html-encoding-sniffer@6, which
# require()s the ESM-only @exodus/bytes — ERR_REQUIRE_ESM on older node broke
# every fetch__fetch article extraction (ExtractArticle.js non-zero exit).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl git tini gnupg \
        build-essential pkg-config \
        python3 python3-pip python3-venv \
        unzip zstd jq ffmpeg tmux; \
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    rm -rf /var/lib/apt/lists/*; \
    node --version | grep -E '^v2[2-9]\.'; \
    pip3 install --break-system-packages --no-cache-dir uv; \
    npm config set update-notifier false; \
    npm install -g pnpm; \
    pnpm --version; \
    update-ca-certificates

# Go toolchain for the user's dev shell (separate from the builder stage's Go,
# which only compiles the daemon binaries).
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) go_arch="linux-amd64" ;; \
        arm64) go_arch="linux-arm64" ;; \
        *) echo "unsupported arch $arch"; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${go_arch}.tar.gz" | tar -C /usr/local -xz; \
    go version

# Rust via rustup, installed system-wide (RUSTUP_HOME/CARGO_HOME above) so the
# exec MCP shell finds cargo/rustc on PATH regardless of $HOME.
RUN set -eux; \
    curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain stable; \
    rustc --version; \
    cargo --version

# Foundry (forge/cast/anvil/chisel) for the Solidity/EVM workflows that
# tachyon delegates back to the workspace shell. foundryup installs into
# FOUNDRY_DIR; /usr/local/foundry/bin is already on PATH.
RUN set -eux; \
    curl -fsSL https://foundry.paradigm.xyz | FOUNDRY_DIR=/usr/local/foundry bash; \
    FOUNDRY_DIR=/usr/local/foundry /usr/local/foundry/bin/foundryup; \
    forge --version; \
    cast --version; \
    anvil --version

# Local data services for the coding workbench: PostgreSQL, Redis and SQLite
# baked as native server binaries — NOT docker-in-docker. Neo's exec bridge
# (the agent's shell tool) starts them ON DEMAND from inside the workspace
# shell, e.g.
#   pg_ctlcluster 16 main start   # or: pg_ctl -D <datadir> start
#   redis-server --daemonize yes
#   sqlite3 <file>                # serverless; no daemon to start
# They are deliberately NOT auto-started at boot (no systemd in the container);
# the entrypoint only supervises the neo/daemon pair. `postgresql` pulls the
# versioned server (Ubuntu 24.04 → PostgreSQL 16) plus pg_ctlcluster; the
# default cluster it auto-creates is left stopped.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        postgresql postgresql-contrib \
        redis-server \
        sqlite3 libsqlite3-dev; \
    rm -rf /var/lib/apt/lists/*; \
    # Stop the cluster/daemon the postinst may have started — services are
    # exec-bridge-startable, not boot services.
    (pg_ctlcluster "$(ls /etc/postgresql 2>/dev/null | head -1)" main stop || true) 2>/dev/null; \
    (service redis-server stop || true) 2>/dev/null; \
    psql --version; \
    redis-server --version; \
    sqlite3 --version

# Per-user Playwright (BROWSER-FILMSTRIP req.1): @playwright/mcp + chromium
# baked into the image so a freshly-provisioned per-user container can navigate
# and screenshot locally with no external dependency (ac 1.3). The entrypoint
# runs it on 127.0.0.1:8931 and points MATRIX_BROWSER_URL at it; the daemon-side
# stdio bridge (tools/browser/browser.mjs) forwards unchanged in wire shape
# (ac 1.2). PWMCP_VERSION must match the version whose tools/list is mirrored in
# tools/browser/playwright-tools.json (and enumerated in agents/neo.json) —
# same pin discipline as the retired deploy/browser image. Install the browser
# with @playwright/mcp's OWN bundled playwright CLI (NOT `npx playwright
# install`) so the chromium revision matches the mcp's pinned playwright.
ARG PWMCP_VERSION=0.0.75
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN set -eux; \
    npm install -g @playwright/mcp@${PWMCP_VERSION}; \
    node "$(npm root -g)/@playwright/mcp/node_modules/playwright/cli.js" install --with-deps chromium; \
    npm cache clean --force; \
    rm -rf /var/lib/apt/lists/*

# Quality toolchain Neo shells into for its verify stages.
# Pins chosen to match real published releases; bump via a rebuild.
ARG GOLANGCI_LINT_VERSION=v1.61.0
ARG RUFF_VERSION=0.6.9
ARG ESLINT_VERSION=9.13.0
ARG PRETTIER_VERSION=3.3.3
ARG TYPESCRIPT_VERSION=5.6.3
ARG VITEST_VERSION=2.1.3
RUN set -eux; \
    # Go: golangci-lint via its pinned official installer (script tag == binary
    # tag) into /usr/local/bin so the exec shell finds it on PATH.
    curl -sSfL "https://raw.githubusercontent.com/golangci/golangci-lint/${GOLANGCI_LINT_VERSION}/install.sh" \
        | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"; \
    golangci-lint --version; \
    # Python: ruff as a standalone uv-managed tool (same mechanism as the MCP
    # servers above; shim lands in /root/.local/bin, already on PATH).
    uv tool install "ruff==${RUFF_VERSION}"; \
    ruff --version; \
    # JS/TS: eslint + prettier + typescript(tsc) + vitest as global npm bins.
    npm install -g \
        "eslint@${ESLINT_VERSION}" \
        "prettier@${PRETTIER_VERSION}" \
        "typescript@${TYPESCRIPT_VERSION}" \
        "vitest@${VITEST_VERSION}"; \
    eslint --version; \
    prettier --version; \
    tsc --version; \
    vitest --version; \
    # Rust: clippy as a rustup component on the stable toolchain installed above.
    rustup component add clippy; \
    cargo clippy --version

# Docker CLI (+ buildx/compose plugins), CLI-ONLY — no daemon is baked. Lets the
# exec shell drive `docker build` / `docker compose` against a mounted socket or
# a remote host (the Hermes model). Daemon/DinD is intentionally NOT included.
RUN set -eux; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
        > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        docker-ce-cli docker-buildx-plugin docker-compose-plugin; \
    rm -rf /var/lib/apt/lists/*; \
    docker --version; \
    docker buildx version; \
    docker compose version

# Extra language package managers so lockfile-pinned projects build without
# friction: yarn + bun (JS, alongside npm/pnpm above) and poetry (Python,
# alongside uv). bun installs into /usr/local/bin via BUN_INSTALL; poetry is a
# uv-managed tool (shim in /root/.local/bin, already on PATH).
RUN set -eux; \
    npm install -g yarn; \
    yarn --version; \
    curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash; \
    bun --version; \
    uv tool install poetry; \
    poetry --version

# Realtime voice worker dependencies live in an isolated runtime environment;
# the source tree's development venv is never copied into the image.
COPY tools/voice/requirements.txt /tmp/voice-requirements.txt
RUN set -eux; \
    python3 -m venv /opt/voice-venv; \
    /opt/voice-venv/bin/pip install --no-cache-dir -r /tmp/voice-requirements.txt; \
    rm -f /tmp/voice-requirements.txt

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

# Pre-install the MinIO client (`mc`) for snapshot sync.
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

# Copy the static binaries first. mcl-execute = the co-located plumbing daemon
# (healthz, /memory + /profile stores, volume snapshots); neo = the agent
# (matrix/neo serve) — the only agent in the runtime.
COPY --from=builder /out/mcl-execute /opt/matrix/bin/mcl-execute
COPY --from=builder /out/neo         /opt/matrix/bin/neo

# Bake the skill corpus + agent manifests + tool bridges — the same set as
# deploy/daemon/Dockerfile (see its per-tool comments); the daemon's strict
# spawn check exits if any manifest-declared bridge file is missing, so ALL
# of them MUST be baked.
COPY skills/         /opt/matrix/skills/
# agents/ carries every agent manifest (agents/neo.json + agents/default.json).
COPY agents/         /opt/matrix/agents/
# rules/ is the language/standards library Neo's coding prompts reference.
COPY rules/          /opt/matrix/rules/
# graph/self-model is the GENERATED structural self-graph (the codegraph
# self-scope output) that neo READS at runtime via cfg.SelfModelGraph — the
# relative default "graph/self-model", resolved from WORKDIR /opt/matrix. It is
# committed and anti-drift-gated in CI; bake it so the self-model's structural
# facet is populated in prod instead of degrading to empty.
COPY graph/self-model/ /opt/matrix/graph/self-model/
COPY tools/paxeer    /opt/matrix/tools/paxeer
COPY tools/websearch /opt/matrix/tools/websearch
COPY tools/finance   /opt/matrix/tools/finance
COPY tools/browser   /opt/matrix/tools/browser
COPY tools/machinemail /opt/matrix/tools/machinemail
COPY tools/exec      /opt/matrix/tools/exec
COPY tools/tachyon   /opt/matrix/tools/tachyon
COPY tools/kindle    /opt/matrix/tools/kindle
COPY tools/deus      /opt/matrix/tools/deus
COPY tools/media     /opt/matrix/tools/media
COPY tools/voice/*.py /opt/matrix/tools/voice/
COPY tools/voice/requirements.txt /opt/matrix/tools/voice/requirements.txt
RUN set -eux; \
    cd /opt/matrix/tools/voice; \
    /opt/voice-venv/bin/python agent.py download-files
COPY tools/uwac      /opt/matrix/tools/uwac
COPY tools/chronos   /opt/matrix/tools/chronos
COPY tools/layerx    /opt/matrix/tools/layerx
COPY tools/sandbox   /opt/matrix/tools/sandbox
# DOJO disposable-desktop actuation bridge (wave 2): agents/neo.json spawns it
# at /root/matrix/tools/desktop/desktop.mjs (the symlink below); it needs no
# credentials — MATRIX_DOJO_URL/MATRIX_DOJO_TOKEN are injected by the daemon's
# serve wiring at runtime.
COPY tools/desktop   /opt/matrix/tools/desktop
# Scaffolder suite (scaffold-<stack>.sh): Neo runs these via exec for
# greenfield project structure instead of hand-writing boilerplate.
COPY tools/scaffold  /opt/matrix/tools/scaffold
RUN chmod +x /opt/matrix/tools/scaffold/*.sh
# Paxeer Cloud CLI (static Go binary; driven through the exec shell tool, not
# MCP). Symlink onto PATH so `paxc` resolves from any shell cwd.
COPY tools/paxeer-cloud /opt/matrix/tools/paxeer-cloud
RUN chmod +x /opt/matrix/tools/paxeer-cloud/paxc \
    && ln -sfn /opt/matrix/tools/paxeer-cloud/paxc /usr/local/bin/paxc
# agents/default.json hard-codes /root/matrix/tools/paxeer/paxeer-net.mjs
# (matches the dev-box layout). Symlink /root/matrix → /opt/matrix so the
# baked-in path resolves inside the container without templating.
RUN mkdir -p /root && ln -sfn /opt/matrix /root/matrix

# Entrypoint: prep volume layout + exec daemon (Railway variant — adds the
# MATRIX_SNAPSHOT_INTERVAL knob wiring; otherwise mirrors the Fly script).
COPY deploy/railway/entrypoint.sh /opt/matrix/entrypoint.sh
RUN chmod +x /opt/matrix/entrypoint.sh

# Health-check probe. In the default `neo` run mode :8080 is the Neo front,
# which reverse-proxies /healthz to the MCL daemon backend on :8081 — so a
# 200 here proves BOTH processes are up. Start period covers the two boots.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/healthz >/dev/null || exit 1

EXPOSE 8080

# The Railway volume is mounted at /data; the daemon expects it.
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/matrix/entrypoint.sh"]
# Default to the Neo front (Neo :8080 + MCL daemon :8081 co-located).
CMD ["neo"]
