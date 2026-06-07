# syntax=docker/dockerfile:1.7
#
# matrix-tachyon — the SHARED Solidity/EVM engine for the Matrix agent fleet.
#
# Runs tachyond (github.com/paxlabs-inc/tachyon-tools/cmd/tachyond) over its
# JSON-RPC transport (POST /rpc). Every per-user daemon reaches this single app
# over Fly's private 6PN network (http://matrix-tachyon.internal:8645/rpc)
# through the daemon-side stdio proxy tools/tachyon/tachyon.mjs.
#
# CUSTODY: the box holds NO wallet seed. It runs the embedded wallet signer in
# MULTI-TENANT (keyfile-empty) mode: every broadcast must carry a forwarded
# per-agent bearer (wallet_token), which the proxy mints from each daemon's own
# ed25519 executor key. So deploy/call sign + broadcast as the CALLING agent,
# and a compromise of this box leaks no signing keys.
#
# SOURCE MODEL: callers upload contract sources per request; the engine
# materializes them into an ephemeral Foundry workdir and links this image's
# baked dependency corpus (the @openzeppelin/contracts/ tree + forge-std) so
# imports resolve. That corpus + foundry.toml + remappings.txt are copied to
# /opt/tachyon (TACHYON_PROJECT_ROOT).
#
# PINNING: FOUNDRY_VERSION must produce a forge whose solc behavior matches the
# fleet (the dev box runs forge 1.7.1). Bump deliberately and re-test.
#
# Build context is the REPO ROOT (mirrors deploy/daemon/Dockerfile), so paths
# below are repo-relative (tachyon/...). Deploy: see README.md.

# -------- 1. Builder --------
FROM golang:1.22-bookworm AS builder

WORKDIR /src
ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath

COPY tachyon/go.mod tachyon/go.sum ./
RUN go mod download

COPY tachyon/ ./
RUN go build -ldflags="-s -w" -o /out/tachyond ./cmd/tachyond

# -------- 2. Runtime --------
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PATH="/root/.foundry/bin:${PATH}" \
    TACHYON_PROJECT_ROOT=/opt/tachyon \
    TACHYON_API_ADDR=0.0.0.0:8645 \
    TACHYON_REGISTRY_PATH=/opt/tachyon/registry.json \
    TACHYON_ARTIFACTS_DIR=/opt/tachyon/artifacts \
    TACHYON_WALLET_MODE=embedded

# Pinned Foundry release (forge + cast + anvil). Override at build time.
ARG FOUNDRY_VERSION=stable

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git tini; \
    rm -rf /var/lib/apt/lists/*; \
    curl -L https://foundry.paradigm.xyz | bash; \
    /root/.foundry/bin/foundryup -i "${FOUNDRY_VERSION}"; \
    forge --version

# The tachyond binary.
COPY --from=builder /out/tachyond /usr/local/bin/tachyond

# Bake the Foundry project the ephemeral source workdir links against:
# the @openzeppelin/contracts/ corpus (remapped @openzeppelin/contracts/=.oz/),
# the lib/ submodules (forge-std + friends), and the foundry config.
COPY tachyon/contracts     /opt/tachyon/contracts
COPY tachyon/lib           /opt/tachyon/lib
COPY tachyon/foundry.toml  /opt/tachyon/foundry.toml
COPY tachyon/remappings.txt /opt/tachyon/remappings.txt

# Chain presets (id/chain_id/explorer; RPC URLs resolved from env at boot).
# Without this the engine boots with ZERO chains — tachyon_chain_list returns
# [] and every deploy/call/simulate fails "chain not found". The paxeer-mainnet
# preset (chain 125) takes its RPC from PAXEER_RPC_URL (set in fly.toml).
COPY tachyon/chains        /opt/tachyon/chains

# Warm the pinned solc (0.8.27, per foundry.toml) into the image so the first
# real compile doesn't pay a solc download. Uses a throwaway project so a
# corpus quirk can never fail the image build.
RUN set -eux; \
    mkdir -p /tmp/warm/src; \
    printf '[profile.default]\nsrc = "src"\nout = "out"\nsolc_version = "0.8.27"\n' > /tmp/warm/foundry.toml; \
    printf '// SPDX-License-Identifier: MIT\npragma solidity 0.8.27;\ncontract Warm { uint256 public x; }\n' > /tmp/warm/src/Warm.sol; \
    forge build --root /tmp/warm; \
    rm -rf /tmp/warm

EXPOSE 8645

# /healthz is public (no bearer) and reports forge + chains + version.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8645/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "tachyond"]
