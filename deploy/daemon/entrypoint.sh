#!/usr/bin/env bash
# entrypoint.sh — prep the per-Machine /data layout, then start Matrix.
#
# Two run modes (selected by $1; the image CMD defaults to `neo`):
#
#   neo     The per-user runtime (DEFAULT). Boots the MCL daemon in the
#           BACKGROUND on :8081 (the rigorous, replayable backend that Neo
#           reaches via core_execute) and runs `neo serve` in the FOREGROUND
#           on :8080 as the conversational FRONT — which reverse-proxies every
#           non-conversational route (/healthz, /messages, /memory, /tools, …)
#           to :8081. If EITHER process exits, the script exits so Fly's
#           on-failure restart reboots the pair.
#
#   daemon  The MCL daemon ALONE on :8080 (legacy / compat). Used by the
#           matrix-telegram sidecar (FROM this image with CMD `daemon`) and any
#           direct daemon use. Byte-for-byte the same command as the pre-Neo
#           image.
#
# Idempotent: every boot ensures the directory tree exists; on a fresh Volume
# this creates everything; on a wake from suspend the dirs are already there.
#
# tini (PID 1, -g) forwards signals to the whole process group, so a Fly
# stop/restart cleanly terminates every child.

set -euo pipefail

DATA_DIR="${MATRIX_DATA_DIR:-/data}"
WORKSPACE_LINK="/workspace"
MATRIX_HOME="${MATRIX_HOME:-/opt/matrix}"

# Backend (MCL daemon) port when fronted by Neo. The front is always :8080.
NEO_BACKEND_PORT="${NEO_BACKEND_PORT:-8081}"

# 1. Ensure volume layout. /data/neo/services keeps Neo's exec service
#    registry isolated from the daemon's (/data/services) so the two
#    co-located exec MCP servers never share a registry.json.
mkdir -p \
    "${DATA_DIR}/cortex" \
    "${DATA_DIR}/journal" \
    "${DATA_DIR}/transcripts" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/services" \
    "${DATA_DIR}/.matrix"

# 2. Symlink /workspace → /data/workspace so MCP fs/git see the persisted
#    user filesystem. agents/*.json hardcode /workspace.
if [[ ! -L "${WORKSPACE_LINK}" ]]; then
    if [[ -d "${WORKSPACE_LINK}" ]] && [[ -z "$(ls -A "${WORKSPACE_LINK}" 2>/dev/null)" ]]; then
        rmdir "${WORKSPACE_LINK}" 2>/dev/null || true
    fi
    ln -sfn "${DATA_DIR}/workspace" "${WORKSPACE_LINK}"
fi

# 2b. Init /workspace as a git repo if not already — the git MCP server
#     refuses to start unless /workspace is a valid working tree, and the
#     daemon's strict spawn check would then kill the process.
if [[ ! -d "${DATA_DIR}/workspace/.git" ]]; then
    git -C "${DATA_DIR}/workspace" init -q -b main
    git -C "${DATA_DIR}/workspace" config user.email "matrix-daemon@${MATRIX_USER_ID:-unknown}.matrix.local"
    git -C "${DATA_DIR}/workspace" config user.name "matrix-daemon"
fi

# 3 / 3b. Snapshot (MinIO) + paxeer-net wallet env are inherited by the MCL
#         daemon's MCP spawns; the daemon decides pull-vs-fresh from the
#         /data/.matrix/seeded sentinel. Required env is documented in the
#         daemon flags below — unchanged from the pre-Neo image.

# build_daemon_argv ADDR -> fills DAEMON_ARGV with the full daemon command.
# Identical flag set to the pre-Neo image; only -addr is parameterised so the
# `neo` mode can place the daemon on :8081 behind the Neo front.
build_daemon_argv() {
    local addr="$1"
    DAEMON_ARGV=(
        "${MATRIX_HOME}/bin/mcl-execute" daemon
        -addr "${addr}"
        -manifest "${MATRIX_HOME}/agents/default.json"
        -skills-root "${MATRIX_HOME}/skills"
        -cortex-root "${DATA_DIR}/cortex"
        -cortex-actor "${MATRIX_USER_ID:-executor}"
        -journal-dir "${DATA_DIR}/journal"
        -transcripts-dir "${DATA_DIR}/transcripts"
        -keyfile "${DATA_DIR}/.matrix/executor.key"
        -did "${MATRIX_USER_ID:-executor}"
        -snapshot-data-dir "${DATA_DIR}"
        -workspace-root "${MATRIX_WORKSPACE_ROOT:-${DATA_DIR}/workspace}"
    )
    [[ -n "${MATRIX_S3_ENDPOINT:-}" ]]             && DAEMON_ARGV+=( -snapshot-endpoint "${MATRIX_S3_ENDPOINT}" )
    [[ -n "${MATRIX_S3_BUCKET:-}" ]]               && DAEMON_ARGV+=( -snapshot-bucket "${MATRIX_S3_BUCKET}" )
    [[ -n "${MATRIX_USER_ID:-}" ]]                 && DAEMON_ARGV+=( -snapshot-user-id "${MATRIX_USER_ID}" )
    [[ -n "${MATRIX_DEFAULT_SKILL:-}" ]]           && DAEMON_ARGV+=( -skill-default "${MATRIX_DEFAULT_SKILL}" )
    [[ -n "${MATRIX_COMPILER_MODEL:-}" ]]          && DAEMON_ARGV+=( -compiler-model "${MATRIX_COMPILER_MODEL}" )
    [[ -n "${MATRIX_EXECUTOR_MODEL:-}" ]]          && DAEMON_ARGV+=( -executor-model "${MATRIX_EXECUTOR_MODEL}" )
    [[ -n "${MATRIX_WITH_FIREWORKS_EMBEDDER:-}" ]] && DAEMON_ARGV+=( -with-fireworks-embedder )
    [[ -n "${MATRIX_ALLOW_SUB_DISPATCH:-}" ]]      && DAEMON_ARGV+=( -allow-sub-dispatch )
    [[ -n "${PAXEER_SPEND_CAP_WEI:-}" ]]           && DAEMON_ARGV+=( -paxeer-cap-wei "${PAXEER_SPEND_CAP_WEI}" )
    [[ -n "${PAXEER_AGG_CAP_WEI:-}" ]]             && DAEMON_ARGV+=( -paxeer-aggregate-cap-wei "${PAXEER_AGG_CAP_WEI}" )
    [[ -n "${PAXEER_SPEND_POLICY_DISABLE:-}" ]]    && DAEMON_ARGV+=( -paxeer-spend-policy-disable )
    # IMPORTANT: a trailing `[[ … ]] && …` whose test is false returns 1, which
    # under `set -e` would abort the caller. Always return success.
    return 0
}

# wait_for_health URL [TRIES] -> 0 once the URL answers (any HTTP), else 1.
wait_for_health() {
    local url="$1" tries="${2:-80}" i
    for (( i = 0; i < tries; i++ )); do
        if curl -fsS "${url}" >/dev/null 2>&1; then return 0; fi
        sleep 0.5
    done
    return 1
}

case "${1:-neo}" in
    neo)
        shift || true

        # Backend: the MCL daemon on :8081 (background). Neo reaches it for
        # core_execute (rigorous / money tasks) and reverse-proxies every
        # non-conversational route to it.
        build_daemon_argv ":${NEO_BACKEND_PORT}"
        "${DAEMON_ARGV[@]}" &
        DAEMON_PID=$!

        # Best-effort wait so Neo's proxy + first core_execute land cleanly.
        # Non-fatal: Neo serves /chat without the backend; only delegation
        # and proxied routes need it (and the router re-probes /healthz).
        wait_for_health "http://127.0.0.1:${NEO_BACKEND_PORT}/healthz" 80 \
            || echo "entrypoint: backend daemon not ready on :${NEO_BACKEND_PORT} yet (continuing)" >&2

        # Front: Neo on :8080.
        #  - MATRIX_EXEC_STATE_DIR isolates Neo's exec service registry.
        #  - cortex actor `neo` is a separate Pebble store under the shared
        #    /data/cortex root (no lock conflict with the daemon's user actor).
        #  - NEO_ACTOR_DID attributes Neo's metered LLM spend to the user.
        #  - LLM provider/key/metering are inherited from the machine env
        #    (MATRIX_GATEWAY_URL/TOKEN); Neo declares its own gateway slot=neo.
        #  - NEO_DAEMON_TOKEN is only meaningful if the operator set
        #    MATRIX_DAEMON_TOKEN (else the loopback daemon is auth-open).
        export MATRIX_EXEC_STATE_DIR="${DATA_DIR}/neo/services"
        export NEO_DAEMON_URL="http://127.0.0.1:${NEO_BACKEND_PORT}"
        export NEO_DAEMON_TOKEN="${MATRIX_DAEMON_TOKEN:-}"
        # The gateway requires a DID-shaped actor (auth.looksLikeDID:
        # did:<method>:<id>). A BARE user id is rejected with
        # "actor_invalid: malformed X-Matrix-Actor-DID". Use a per-user,
        # Neo-scoped DID so Neo's metered LLM spend attributes distinctly from
        # the MCL daemon's (did:matrix:<user>:<key16>) in the credit ledger.
        export NEO_ACTOR_DID="did:matrix:${MATRIX_USER_ID:-neo}:neo"
        export NEO_SKILLS_ROOT="${MATRIX_HOME}/skills"

        "${MATRIX_HOME}/bin/neo" serve \
            -addr ":8080" \
            -backend "http://127.0.0.1:${NEO_BACKEND_PORT}" \
            -manifest "${MATRIX_HOME}/agents/neo.json" \
            -cortex-root "${DATA_DIR}/cortex" \
            -actor "neo" \
            "$@" &
        NEO_PID=$!

        # If EITHER process exits, tear the other down and exit non-zero so
        # Fly's on-failure restart reboots the pair. (tini -g also forwards a
        # Fly stop/restart signal to the whole group.)
        set +e
        wait -n
        EXIT=$?
        set -e
        echo "entrypoint: a co-located process exited (status ${EXIT}); stopping the pair" >&2
        kill "${DAEMON_PID}" "${NEO_PID}" 2>/dev/null || true
        wait 2>/dev/null || true
        exit "${EXIT}"
        ;;
    daemon)
        # Legacy / compat: the MCL daemon ALONE on :8080.
        shift || true
        build_daemon_argv ":8080"
        exec "${DAEMON_ARGV[@]}" "$@"
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
