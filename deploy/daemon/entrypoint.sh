#!/usr/bin/env bash
# entrypoint.sh — prep the per-Machine /data layout, then exec the daemon.
#
# Idempotent: every boot ensures the directory tree exists; on a fresh
# Volume this creates everything; on a wake from suspend the dirs are
# already there and we just exec.
#
# Snapshot pull (Phase 3) will live here too: if /data/.matrix/seeded
# is missing AND $MATRIX_S3_ENDPOINT is set, we attempt to pull
# s3://$MATRIX_S3_BUCKET/users/$MATRIX_USER_ID/latest.tar.zst before
# starting the daemon. Until that lands, fresh Volumes start empty.

set -euo pipefail

DATA_DIR="${MATRIX_DATA_DIR:-/data}"
WORKSPACE_LINK="/workspace"
MATRIX_HOME="${MATRIX_HOME:-/opt/matrix}"

# 1. Ensure volume layout.
mkdir -p \
    "${DATA_DIR}/cortex" \
    "${DATA_DIR}/journal" \
    "${DATA_DIR}/transcripts" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/.matrix"

# 2. Symlink /workspace → /data/workspace so MCP fs/git see the persisted
#    user filesystem. agents/default.json hardcodes /workspace.
if [[ ! -L "${WORKSPACE_LINK}" ]]; then
    if [[ -d "${WORKSPACE_LINK}" ]] && [[ -z "$(ls -A "${WORKSPACE_LINK}" 2>/dev/null)" ]]; then
        rmdir "${WORKSPACE_LINK}" 2>/dev/null || true
    fi
    ln -sfn "${DATA_DIR}/workspace" "${WORKSPACE_LINK}"
fi

# 2b. Init /workspace as a git repo if not already. The git MCP server
#     (`mcp-server-git --repository /workspace`) refuses to initialize
#     unless the directory is a valid git working tree; without this it
#     errors out and the daemon's strict spawn check kills the process
#     after a 90s deadline.
if [[ ! -d "${DATA_DIR}/workspace/.git" ]]; then
    git -C "${DATA_DIR}/workspace" init -q -b main
    git -C "${DATA_DIR}/workspace" config user.email "matrix-daemon@${MATRIX_USER_ID:-unknown}.matrix.local"
    git -C "${DATA_DIR}/workspace" config user.name "matrix-daemon"
fi

# 3. Snapshot pull is now owned by the Go daemon (executor/internal/
#    snapshot package; sess#26). It runs BootPull before opening the
#    cortex Pebble DB and starts a 5-min push ticker + final on-shutdown
#    push. We only forward the env-driven flags here; the daemon decides
#    pull-vs-fresh based on /data/.matrix/seeded sentinel state.
#
#    Required env to enable snapshots:
#      MATRIX_S3_ENDPOINT  e.g. http://[fdaa:75:8960:...]:9000
#      MATRIX_S3_BUCKET    default matrix-state (daemon flag default)
#      MATRIX_S3_KEY       MinIO access key
#      MATRIX_S3_SECRET    MinIO secret key
#      MATRIX_USER_ID      Supabase user id (snapshot prefix)
#    Empty MATRIX_S3_ENDPOINT disables snapshots entirely (local-dev).
#
# 3b. Paxeer chain-bridge (paxeer-net MCP server) env. The Go daemon
#     inherits this process's environment when spawning every MCP
#     server, so any PAXEER_* var visible here reaches the bridge.
#     Required for chain reads: nothing (PAXEER_RPC_URL has a public
#     mainnet default).
#     Required for chain writes: ONE of —
#       PAXEER_WALLET_TOKEN   Supabase access_token (static custody bearer)
#       PAXEER_WALLET_EMAIL + PAXEER_WALLET_PASSWORD + PAXEER_SUPABASE_ANON_KEY
#                             (password-grant; bridge logs in on first write)
#     Optional spend caps (else daemon defaults — 1 PAX per call,
#     5 PAX aggregate):
#       PAXEER_SPEND_CAP_WEI / PAXEER_AGG_CAP_WEI
#     Without any wallet env the bridge stays read-only and chain
#     writes fail at dispatch with a clear "wallet not configured"
#     error rather than crashing the daemon.

# 4. Dispatch.
case "${1:-daemon}" in
    daemon)
        shift || true
        exec "${MATRIX_HOME}/bin/mcl-execute" daemon \
            -addr ":8080" \
            -manifest "${MATRIX_HOME}/agents/default.json" \
            -skills-root "${MATRIX_HOME}/skills" \
            -cortex-root "${DATA_DIR}/cortex" \
            -cortex-actor "${MATRIX_USER_ID:-executor}" \
            -journal-dir "${DATA_DIR}/journal" \
            -transcripts-dir "${DATA_DIR}/transcripts" \
            -keyfile "${DATA_DIR}/.matrix/executor.key" \
            -did "${MATRIX_USER_ID:-executor}" \
            ${MATRIX_S3_ENDPOINT:+-snapshot-endpoint "${MATRIX_S3_ENDPOINT}"} \
            ${MATRIX_S3_BUCKET:+-snapshot-bucket "${MATRIX_S3_BUCKET}"} \
            ${MATRIX_USER_ID:+-snapshot-user-id "${MATRIX_USER_ID}"} \
            -snapshot-data-dir "${DATA_DIR}" \
            ${MATRIX_DEFAULT_SKILL:+-skill-default "${MATRIX_DEFAULT_SKILL}"} \
            ${MATRIX_COMPILER_MODEL:+-compiler-model "${MATRIX_COMPILER_MODEL}"} \
            ${MATRIX_EXECUTOR_MODEL:+-executor-model "${MATRIX_EXECUTOR_MODEL}"} \
            ${MATRIX_WITH_FIREWORKS_EMBEDDER:+-with-fireworks-embedder} \
            ${MATRIX_ALLOW_SUB_DISPATCH:+-allow-sub-dispatch} \
            -workspace-root "${MATRIX_WORKSPACE_ROOT:-${DATA_DIR}/workspace}" \
            ${PAXEER_SPEND_CAP_WEI:+-paxeer-cap-wei "${PAXEER_SPEND_CAP_WEI}"} \
            ${PAXEER_AGG_CAP_WEI:+-paxeer-aggregate-cap-wei "${PAXEER_AGG_CAP_WEI}"} \
            ${PAXEER_SPEND_POLICY_DISABLE:+-paxeer-spend-policy-disable} \
            "$@"
        ;;
    walk|classify|loader)
        # Pass-through for ad-hoc CLI work inside the container.
        exec "${MATRIX_HOME}/bin/mcl-execute" "$@"
        ;;
    sh|bash)
        exec "$@"
        ;;
    *)
        # Custom command — exec it.
        exec "$@"
        ;;
esac
