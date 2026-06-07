#!/usr/bin/env bash
# deploy.sh — create + deploy the shared matrix-tachyon Fly app.
#
# matrix-tachyon is the SHARED, PRIVATE Solidity/EVM engine (tachyond) that the
# per-user daemon fleet reaches over 6PN at matrix-tachyon.internal:8645 via the
# stdio proxy tools/tachyon/tachyon.mjs. See deploy/tachyon/README.md.
#
# This script is idempotent: it creates the app on first run and re-deploys on
# subsequent runs. The build context is the REPO ROOT (the Dockerfile COPYs
# tachyon/...), so it always invokes flyctl from the repo root.
#
# CREDENTIALS — read carefully:
#   The FLY_API_TOKEN in /etc/matrix/router.env is app-scoped to matrix-daemon
#   and CANNOT create apps. App creation + first deploy need an org-capable
#   credential. Either:
#     (a) run `flyctl auth login` first (this script will use that login), or
#     (b) export MATRIX_TACHYON_DEPLOY_TOKEN=<app-scoped token for matrix-tachyon>
#         (mint once: `flyctl tokens create deploy -a matrix-tachyon`).
#   This script UNSETS FLY_API_TOKEN unless MATRIX_TACHYON_DEPLOY_TOKEN is set,
#   so the daemon-scoped token never shadows your org login.
#
# Usage:
#   deploy/tachyon/deploy.sh                 # create (if needed) + deploy
#   FLY_ORG=personal deploy/tachyon/deploy.sh
#   TACHYON_AUTH_TOKEN=<tok> deploy/tachyon/deploy.sh   # also set the engine bearer
#   FOUNDRY_VERSION=stable deploy/tachyon/deploy.sh
#
# After a successful first deploy, wire the fleet (one-time): set
#   MATRIX_TACHYON_URL=http://matrix-tachyon.internal:8645/rpc
# (already the router default) and, if you set TACHYON_AUTH_TOKEN, also set
#   MATRIX_TACHYON_TOKEN=<same tok>
# in /etc/matrix/router.env, then restart matrix-router.

set -euo pipefail

APP="${APP:-matrix-tachyon}"
FLY_ORG="${FLY_ORG:-personal}"
PRIMARY_REGION="${PRIMARY_REGION:-iad}"
FLY_CONFIG="deploy/tachyon/fly.toml"

# Resolve repo root from this script's location (deploy/tachyon/deploy.sh).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT"

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\n\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

command -v flyctl >/dev/null 2>&1 || die "flyctl not found on PATH"
[ -f "$FLY_CONFIG" ] || die "missing $FLY_CONFIG (run from a checkout that contains deploy/tachyon/)"
[ -f "tachyon/go.mod" ] || die "tachyon/ source not found at repo root ($ROOT)"

# Credential selection (see header).
if [ -n "${MATRIX_TACHYON_DEPLOY_TOKEN:-}" ]; then
  log "Using MATRIX_TACHYON_DEPLOY_TOKEN as FLY_API_TOKEN (app-scoped deploy token)"
  export FLY_API_TOKEN="$MATRIX_TACHYON_DEPLOY_TOKEN"
else
  if [ -n "${FLY_API_TOKEN:-}" ]; then
    log "Unsetting FLY_API_TOKEN so it does not shadow your org login (set MATRIX_TACHYON_DEPLOY_TOKEN to override)"
  fi
  unset FLY_API_TOKEN || true
  flyctl auth whoami >/dev/null 2>&1 || die "not logged in to Fly — run 'flyctl auth login' (org-capable), or set MATRIX_TACHYON_DEPLOY_TOKEN"
fi

# Ensure the Foundry lib submodules are present (copied into the image).
if [ -d "tachyon/lib" ] && [ -z "$(ls -A tachyon/lib 2>/dev/null || true)" ]; then
  log "tachyon/lib is empty — initializing Foundry submodules"
  git -C tachyon submodule update --init --recursive || \
    die "failed to init tachyon submodules; populate tachyon/lib before deploy"
fi

# Create the app on first run (private: no public IP allocated).
if flyctl status -a "$APP" >/dev/null 2>&1; then
  log "App '$APP' already exists — re-deploying"
else
  log "Creating private app '$APP' in org '$FLY_ORG' (no public IP)"
  flyctl apps create "$APP" --org "$FLY_ORG"
fi

# Optional engine bearer: set as a Fly secret BEFORE deploy so the first boot
# enforces it. The matching MATRIX_TACHYON_TOKEN must go in the router env.
if [ -n "${TACHYON_AUTH_TOKEN:-}" ]; then
  log "Setting TACHYON_AUTH_TOKEN secret on '$APP'"
  flyctl secrets set "TACHYON_AUTH_TOKEN=$TACHYON_AUTH_TOKEN" -a "$APP"
fi

# Deploy. Build context = repo root (.), Dockerfile path is in fly.toml.
BUILD_ARGS=()
if [ -n "${FOUNDRY_VERSION:-}" ]; then
  BUILD_ARGS+=(--build-arg "FOUNDRY_VERSION=$FOUNDRY_VERSION")
fi

log "Deploying '$APP' (context=$ROOT, config=$FLY_CONFIG)"
flyctl deploy --ha=false --now -c "$FLY_CONFIG" "${BUILD_ARGS[@]}" .

# Verify: must be PRIVATE (no public v4/v6) + healthy.
log "Verifying private networking (expect NO public v4/v6 addresses)"
flyctl ips list -a "$APP" || true

log "App status"
flyctl status -a "$APP" || true

log "Engine health (/healthz over the machine, via SSH)"
if flyctl ssh console -a "$APP" -C "curl -fsS http://127.0.0.1:8645/healthz"; then
  printf '\n'
  log "Health OK."
else
  printf '\n'
  die "healthz probe failed — check 'flyctl logs -a $APP'"
fi

cat <<EOF

==> Done.

Next (one-time fleet wiring), in /etc/matrix/router.env on the router box:
  MATRIX_TACHYON_URL=http://matrix-tachyon.internal:8645/rpc   # already the router default
$( [ -n "${TACHYON_AUTH_TOKEN:-}" ] && echo "  MATRIX_TACHYON_TOKEN=<the TACHYON_AUTH_TOKEN you just set>" )
then: systemctl restart matrix-router

The daemon proxy (tools/tachyon/tachyon.mjs) dials this lazily; daemons that are
already running pick it up on their next tachyon_* call.
EOF
