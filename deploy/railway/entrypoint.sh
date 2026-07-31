#!/usr/bin/env bash
# entrypoint.sh — prep the per-service /data layout, then start Matrix.
# Railway variant of deploy/daemon/entrypoint.sh: same two run modes, same
# volume layout, same daemon flag set. Railway-specific notes:
#
#   - ONE volume: Railway allows exactly one volume mount per service. It
#     mounts at /data; /workspace stays a symlink to /data/workspace so the
#     MCP fs/git root convention is unchanged.
#   - Dual-stack listeners: the Railway private network is IPv6-only. Go's
#     net.Listen on ":port" (what mcl-execute -addr and neo serve -addr do)
#     binds dual-stack, so :: traffic is accepted without any flag changes.
#   - Wake-on-request: Railway Serverless sleeps the service when it is
#     network-quiet and wakes it on the first inbound packet. The router sets
#     MATRIX_SNAPSHOT_INTERVAL=-1s so the periodic snapshot push ticker stays
#     off (boot+shutdown snapshots only) and outbound traffic never keeps the
#     service awake. BootPull from MinIO is retained — it is the volume-seed /
#     Fly→Railway migration vehicle.
#
# Three run modes (selected by $1; the image CMD defaults to `neo`):
#
#   neo     The per-user runtime (DEFAULT). Boots the plumbing daemon in the
#           BACKGROUND on :8081 (healthz, /memory + /profile stores, volume
#           snapshots) and `neo serve` on :8080 as the agent FRONT — which
#           reverse-proxies every non-conversational route (/healthz,
#           /memory, /profile, …) to :8081. If EITHER exits, the script
#           exits so the platform's on-failure restart reboots the pair.
#
#   daemon  The MCL daemon ALONE on :8080 (legacy / compat).
#
#   workforce  The independent Workforce control plane on :8091. It uses the
#              same production image but retains its own process, authority,
#              PostgreSQL, Vault, Bubblewrap, and CodeGraph Ultra boundaries.
#
# Idempotent: every boot ensures the directory tree exists; on a fresh volume
# this creates everything; on a wake from sleep the dirs are already there.
#
# tini (PID 1, -g) forwards signals to the whole process group, so a platform
# stop/restart cleanly terminates every child.

set -euo pipefail

DATA_DIR="${MATRIX_DATA_DIR:-/data}"
WORKSPACE_LINK="/workspace"
MATRIX_HOME="${MATRIX_HOME:-/opt/matrix}"

# Backend (plumbing daemon) port when fronted by Neo. The front is always :8080.
NEO_BACKEND_PORT="${NEO_BACKEND_PORT:-8081}"

# 1. Ensure volume layout. /data/neo/services keeps Neo's exec service
#    registry isolated from the daemon's (/data/services) so the two
#    co-located exec MCP servers never share a registry.json.
mkdir -p \
    "${DATA_DIR}/cortex" \
    "${DATA_DIR}/journal" \
    "${DATA_DIR}/transcripts" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/media" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/services" \
    "${DATA_DIR}/workforce" \
    "${DATA_DIR}/.matrix"

# Agent-owned mutable paths are migrated once, then kept owned by the dedicated
# uid. Secret-bearing state remains root-only behind daemon APIs.
AGENT_UID="${MATRIX_AGENT_UID:-10001}"
AGENT_GID="${MATRIX_AGENT_GID:-10001}"
OWNER_MARKER="${DATA_DIR}/.matrix/agent-owner-v1"
AGENT_ENV=(
    "HOME=/home/matrix-agent"
    "USER=matrix-agent"
    "LOGNAME=matrix-agent"
    "SHELL=/bin/bash"
    "PATH=${PATH}"
    "PWD=${DATA_DIR}/workspace"
    "TERM=${TERM:-xterm-256color}"
    "LANG=${LANG:-C.UTF-8}"
)
for name in \
    CODY_USER_ID MATRIX_USER_ID CODY_PREVIEW_IMAGE \
    KINDLE_FRONTEND_URL KINDLE_MEDIA_GATEWAY KINDLE_METADATA_URL KINDLE_RPC_URL \
    MATRIX_BROWSER_URL MATRIX_CHRONOS_TOKEN MATRIX_CHRONOS_URL \
    MATRIX_COMPILER_ESCALATE_MODEL MATRIX_COMPILER_MODEL MATRIX_DATA_DIR \
    MATRIX_DEFAULT_SKILL MATRIX_DEUS_TIMEOUT_MS MATRIX_DEUS_URL \
    MATRIX_EXECUTOR_MODEL MATRIX_GATEWAY_TOKEN MATRIX_GATEWAY_URL \
    MATRIX_LAYERX_TOKEN MATRIX_LAYERX_URL MATRIX_LIAISON_MODEL MATRIX_PLANNER_MODEL \
    MATRIX_SEARXNG_TOKEN MATRIX_SEARXNG_URL MATRIX_SNAPSHOT_INTERVAL \
    MATRIX_TACHYON_TOKEN MATRIX_TACHYON_URL MATRIX_UWAC_TOKEN MATRIX_UWAC_URL \
    PAXC_API PAXC_TOKEN WEBSEARCH_PROVIDER NEO_AUTOMATRIX_ENABLED \
    NEO_AUTOMATRIX_INTERVAL NEO_AUTOMATRIX_JITTER NEO_AUTOMATRIX_MAX_PER_DAY \
    NEO_AUTOMATRIX_MIN_CONFIDENCE MATRIX_MEDIA_XAI_VIDEO_MODEL \
    NEO_CONTINUOUS_MEMORY VAULT_REQUIRED VOICE_IDLE_DISCONNECT_S NEO_RUNTIME \
    MATRIX_EXEC_STATE_DIR MATRIX_EXEC_WORKDIR MATRIX_EXEC_TIMEOUT_MS \
    MATRIX_EXEC_MAX_OUTPUT_BYTES MATRIX_EXEC_MAX_SERVICES MATRIX_EXEC_MAX_LOG_LINES
do
    if [[ -v "${name}" ]]; then
        AGENT_ENV+=("${name}=${!name}")
    fi
done
if [[ ! -f "${OWNER_MARKER}" ]]; then
    chown -R "${AGENT_UID}:${AGENT_GID}" \
        "${DATA_DIR}/workspace" \
        "${DATA_DIR}/services" \
        "${DATA_DIR}/neo/services"
    touch "${OWNER_MARKER}"
fi
chown "${AGENT_UID}:${AGENT_GID}" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/services"
chmod 0750 "${DATA_DIR}/workspace" "${DATA_DIR}/services" "${DATA_DIR}/neo/services"
chmod 0700 "${DATA_DIR}/cortex" "${DATA_DIR}/journal" "${DATA_DIR}/transcripts" "${DATA_DIR}/.matrix"

# 1b. Media plane. Generated + uploaded images/video/audio live on the volume
#     at /data/media and are served by the Neo front at /media. Export the dir
#     (and URL base) so BOTH the MCL daemon (agents/default.json) and the Neo
#     front (agents/neo.json) spawn their `media` MCP bridge writing to the
#     SAME served directory. Neo also derives this path itself, but exporting
#     it keeps the daemon-side bridge consistent. The bridge needs
#     XAI_API_KEY (Grok Imagine, primary) and/or NOVITA_API_KEY (mask ops +
#     TTS fallback) in the machine env to actually call the upstream APIs;
#     without them the bridge still starts (boot-safe) and errors only at call.
export MATRIX_MEDIA_DIR="${MATRIX_MEDIA_DIR:-${DATA_DIR}/media}"
export MATRIX_MEDIA_BASE="${MATRIX_MEDIA_BASE:-/media}"

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
    env -i "${AGENT_ENV[@]}" setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- \
        git -C "${DATA_DIR}/workspace" init -q -b main
    env -i "${AGENT_ENV[@]}" setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- \
        git -C "${DATA_DIR}/workspace" config user.email "matrix-daemon@${MATRIX_USER_ID:-unknown}.matrix.local"
    env -i "${AGENT_ENV[@]}" setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- \
        git -C "${DATA_DIR}/workspace" config user.name "matrix-daemon"
fi

# 3 / 3b. Snapshot (MinIO) + paxeer-net wallet env are inherited by the MCL
#         daemon's MCP spawns; the daemon decides pull-vs-fresh from the
#         /data/.matrix/seeded sentinel. Required env is documented in the
#         daemon flags below — unchanged from the Fly image.

# build_daemon_argv ADDR -> fills DAEMON_ARGV with the full daemon command.
# Identical flag set to the Fly image; only -addr is parameterised so the
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
    [[ -n "${MATRIX_USER_ID:-}" ]]                 && DAEMON_ARGV+=( -snapshot-user-id "${MATRIX_USER_ID}" )
    # Snapshot cadence knob. Unset → flag absent → daemon default (byte-compat
    # with the pre-knob image). A negative duration (e.g. "-1s") disables the
    # periodic push ticker (boot+shutdown snapshots only) — the outbound-quiet
    # mode wake-on-request providers need so Serverless sleep can engage.
    [[ -n "${MATRIX_SNAPSHOT_INTERVAL:-}" ]]       && DAEMON_ARGV+=( -snapshot-interval "${MATRIX_SNAPSHOT_INTERVAL}" )
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

# start_local_browser -> boot the PER-USER Playwright MCP (@playwright/mcp,
# baked into the image) on loopback and point MATRIX_BROWSER_URL at it
# (BROWSER-FILMSTRIP req.1). The daemon-side stdio bridge
# (tools/browser/browser.mjs) forwards to it unchanged in wire shape; it
# answers initialize/tools/list locally and dials lazily, so a browser that is
# still booting (or crashed) never bricks daemon boot — browser_* calls just
# return a structured error until it is up. A tiny restart loop revives a
# crashed chromium without taking the daemon trio down. --isolated hands each
# MCP session a fresh context. Chromium runs under the dedicated agent uid with
# its sandbox enabled. Loopback-only traffic keeps the service outbound-quiet
# for Serverless sleep.
BROWSER_PORT="${MATRIX_BROWSER_PORT:-8931}"
start_local_browser() {
    command -v mcp-server-playwright >/dev/null 2>&1 || npm ls -g @playwright/mcp >/dev/null 2>&1 || {
        echo "entrypoint: @playwright/mcp not baked in this image; browser_* tools stay remote/disabled" >&2
        return 0
    }
    export MATRIX_BROWSER_URL="http://127.0.0.1:${BROWSER_PORT}/mcp"
    (
        while true; do
            env -i "${AGENT_ENV[@]}" "PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH}" \
                setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- \
                npx @playwright/mcp --headless --isolated --browser chromium \
                --host 127.0.0.1 --port "${BROWSER_PORT}" --allowed-hosts '*' \
                >>/tmp/pwmcp.log 2>&1 || true
            echo "entrypoint: local browser exited; restarting in 2s" >&2
            sleep 2
        done
    ) &
    BROWSER_PID=$!
}

start_voice_controller() {
    if [[ "${NEO_VOICE_ENABLED:-false}" != "true" ]]; then
        return 0
    fi
    if [[ -z "${MATRIX_LIVEKIT_URL:-}" || -z "${MATRIX_LIVEKIT_KEY:-}" || -z "${MATRIX_LIVEKIT_SECRET:-}" ]]; then
        echo "entrypoint: voice enabled but LiveKit is not configured; voice stays unavailable" >&2
        return 0
    fi
    export VOICE_CONTROLLER_URL="${VOICE_CONTROLLER_URL:-http://127.0.0.1:8791}"
    (
        while true; do
            /opt/voice-venv/bin/python "${MATRIX_HOME}/tools/voice/controller.py" \
                >>/tmp/voice-controller.log 2>&1 || true
            echo "entrypoint: voice controller exited; restarting in 2s" >&2
            sleep 2
        done
    ) &
    VOICE_CONTROLLER_PID=$!
}

case "${1:-neo}" in
    neo)
        shift || true

        # Per-user local browser (BROWSER-FILMSTRIP): boot it first so
        # MATRIX_BROWSER_URL is exported before the daemon/neo spawn their
        # browser.mjs bridges (the bridge reads it at spawn).
        start_local_browser
        start_voice_controller

        # Backend: the plumbing daemon on :8081 (background). Neo
        # reverse-proxies every non-conversational route to it (healthz,
        # /memory + /profile stores); it also owns the volume snapshots.
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

        # If EITHER exits, tear the other down and exit non-zero so the
        # platform's on-failure restart reboots the pair. (tini -g also
        # forwards a platform stop/restart signal to the whole group.)
        set +e
        wait -n
        EXIT=$?
        set -e
        echo "entrypoint: a co-located process exited (status ${EXIT}); stopping the pair" >&2
        kill "${DAEMON_PID}" "${NEO_PID}" ${BROWSER_PID:+"${BROWSER_PID}"} ${VOICE_CONTROLLER_PID:+"${VOICE_CONTROLLER_PID}"} 2>/dev/null || true
        wait 2>/dev/null || true
        exit "${EXIT}"
        ;;
    daemon)
        # Legacy / compat: the MCL daemon ALONE on :8080.
        shift || true
        build_daemon_argv ":8080"
        exec "${DAEMON_ARGV[@]}" "$@"
        ;;
    workforce)
        shift || true
        workforce_repository="${WORKFORCE_DEVELOPER_REPOSITORY:-/workspace}"
        mkdir -p "${workforce_repository}/.cg"
        "${WORKFORCE_CODEGRAPH_EXECUTABLE:-/usr/local/bin/cg}" \
            --db "${workforce_repository}/.cg/codegraph.db" \
            build "${workforce_repository}" \
            --exclude .cg \
            --exclude .codegraph \
            --exclude .git \
            --exclude node_modules \
            --exclude .next
        exec "${MATRIX_HOME}/bin/workforced" \
            -serve \
            -listen ":${WORKFORCE_PORT:-8091}" \
            "$@"
        ;;
    walk|classify|loader)
        # Pass-through for ad-hoc CLI work inside the container.
        exec "${MATRIX_HOME}/bin/mcl-execute" "$@"
        ;;
    sh|bash)
        exec env -i "${AGENT_ENV[@]}" \
            setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- "$@"
        ;;
    *)
        # Custom commands never receive the root service identity.
        exec env -i "${AGENT_ENV[@]}" \
            setpriv --reuid="${AGENT_UID}" --regid="${AGENT_GID}" --init-groups --no-new-privs -- "$@"
        ;;
esac
