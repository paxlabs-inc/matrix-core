#!/usr/bin/env bash
# entrypoint.sh — map env vars to matrix-gateway flags, then exec it.
#
# Keeps the container and the box systemd unit
# (gateway/deploy/matrix-gateway.service) running the gateway identically:
# the unit hard-codes the flags; here we make each one env-overridable while
# defaulting to the same values, so a container and a systemd box behave the
# same unless explicitly tuned.

set -euo pipefail

# In the systemd unit the gateway binds 127.0.0.1 because nginx fronts it on
# the box. In a container the orchestrator maps the port, so we must listen on
# all interfaces or the published port connection-refuses.
ADDR="${GATEWAY_ADDR:-0.0.0.0:9090}"
FREE_TIER_ONLY="${GATEWAY_FREE_TIER_ONLY:-true}"
LOG_FORMAT="${GATEWAY_LOG_FORMAT:-json}"
DEFAULT_CAP_PAX="${GATEWAY_DEFAULT_CAP_PAX:-10}"
RATE_PER_SEC="${GATEWAY_RATE_PER_SEC:-5}"
RATE_BURST="${GATEWAY_RATE_BURST:-25}"
# Empty URI selects the in-memory ledger (dev / smoke). Production passes a
# Postgres URI so daily spend survives restarts. Quoting keeps an empty value
# from swallowing the next flag (a latent footgun in the systemd ExecStart).
POSTGRES_URI="${MATRIX_GATEWAY_POSTGRES_URI:-}"

case "${1:-gateway}" in
  gateway)
    shift || true
    exec matrix-gateway \
      -addr "${ADDR}" \
      -postgres-uri "${POSTGRES_URI}" \
      -free-tier-only="${FREE_TIER_ONLY}" \
      -log-format="${LOG_FORMAT}" \
      -default-cap-pax="${DEFAULT_CAP_PAX}" \
      -rate-per-sec="${RATE_PER_SEC}" \
      -rate-burst="${RATE_BURST}" \
      "$@"
    ;;
  *)
    # Custom command (sh/bash for debugging, or an explicit matrix-gateway
    # invocation) — exec it verbatim.
    exec "$@"
    ;;
esac
