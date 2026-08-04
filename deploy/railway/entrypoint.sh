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

# Router-side Workforce configuration must never be inherited by a per-user
# runtime. In particular, ROUTER_WORKFORCE_ROOT_SECRET can derive every user's
# authority and belongs only in Router/Chronos control-plane services. Existing
# services created before Workforce reconciliation may still carry these names;
# diagnose that state, then scrub them before starting any browser, voice,
# daemon, Neo, Workforce, or MCP process.
if [[ "${ROUTER_WORKFORCE_ENABLED:-false}" == "true" && "${WORKFORCE_ENABLED:-false}" != "true" ]]; then
    echo "entrypoint: Workforce is enabled on the Router but this user runtime has not been reconciled; WORKFORCE_ENABLED and derived per-user authority are absent" >&2
fi
unset ROUTER_WORKFORCE_ENABLED ROUTER_WORKFORCE_PORT
unset ROUTER_WORKFORCE_POSTGRES_URI ROUTER_WORKFORCE_ROOT_SECRET ROUTER_WORKFORCE_WAKE_TOKEN

DATA_DIR="${MATRIX_DATA_DIR:-/data}"
WORKSPACE_LINK="/workspace"
MATRIX_HOME="${MATRIX_HOME:-/opt/matrix}"

# Backend (plumbing daemon) port when fronted by Neo. The front is always :8080.
NEO_BACKEND_PORT="${NEO_BACKEND_PORT:-8081}"

# 1. Ensure volume layout. The two legacy service registries remain isolated so
#    Recovery can audit and clean process records created by older deployments.
mkdir -p \
    "${DATA_DIR}/cortex" \
    "${DATA_DIR}/journal" \
    "${DATA_DIR}/transcripts" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/media" \
    "${DATA_DIR}/tmp" \
    "${DATA_DIR}/cache" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/tmp" \
    "${DATA_DIR}/neo/cache" \
    "${DATA_DIR}/neo/services" \
    "${DATA_DIR}/build-jobs" \
    "${DATA_DIR}/agentcore" \
    "${DATA_DIR}/neocortex" \
    "${DATA_DIR}/workforce" \
    "${DATA_DIR}/.matrix"

# Agent-owned mutable paths are migrated once, then kept owned by the dedicated
# uid. Secret-bearing state remains root-only behind daemon APIs.
AGENT_UID="${MATRIX_AGENT_UID:-10001}"
AGENT_GID="${MATRIX_AGENT_GID:-10001}"
OWNER_MARKER="${DATA_DIR}/.matrix/agent-owner-v1"
export UV_PYTHON="${UV_PYTHON:-/usr/bin/python3}"
AGENT_ENV=(
    "HOME=/home/matrix-agent"
    "USER=matrix-agent"
    "LOGNAME=matrix-agent"
    "SHELL=/bin/bash"
    "PATH=${PATH}"
    "UV_PYTHON=/usr/bin/python3"
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
    NEO_NEOCORTEX_SOCKET \
    MATRIX_EXEC_STATE_DIR MATRIX_EXEC_WORKDIR MATRIX_EXEC_TIMEOUT_MS \
    MATRIX_EXEC_MAX_OUTPUT_BYTES MATRIX_EXEC_MAX_SERVICES MATRIX_EXEC_MAX_LOG_LINES \
    MATRIX_EXEC_INLINE_SECRET_POLICY
do
    if [[ -v "${name}" ]]; then
        AGENT_ENV+=("${name}=${!name}")
    fi
done

# The Neocortex actor capability belongs only to the Neo memory seam. Keep it
# out of every inherited process environment and inject it explicitly into
# Neo after cortexd is ready. In particular, AGENT_ENV must never carry it to
# Playwright, workspace shells, or custom commands.
NEOCORTEX_ACTOR_TOKEN="${NEO_NEOCORTEX_TOKEN:-}"
unset NEO_NEOCORTEX_TOKEN
if [[ ! -f "${OWNER_MARKER}" ]]; then
    chown -R "${AGENT_UID}:${AGENT_GID}" \
        "${DATA_DIR}/workspace" \
        "${DATA_DIR}/tmp" \
        "${DATA_DIR}/cache" \
        "${DATA_DIR}/services" \
        "${DATA_DIR}/neo/tmp" \
        "${DATA_DIR}/neo/cache" \
        "${DATA_DIR}/neo/services" \
        "${DATA_DIR}/agentcore"
    touch "${OWNER_MARKER}"
fi
chown "${AGENT_UID}:${AGENT_GID}" \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/tmp" \
    "${DATA_DIR}/cache" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/tmp" \
    "${DATA_DIR}/neo/cache" \
    "${DATA_DIR}/neo/services" \
    "${DATA_DIR}/agentcore"
chmod 0750 \
    "${DATA_DIR}/workspace" \
    "${DATA_DIR}/tmp" \
    "${DATA_DIR}/cache" \
    "${DATA_DIR}/services" \
    "${DATA_DIR}/neo/tmp" \
    "${DATA_DIR}/neo/cache" \
    "${DATA_DIR}/neo/services"
chmod 0700 "${DATA_DIR}/agentcore"
chmod 0700 "${DATA_DIR}/build-jobs" "${DATA_DIR}/cortex" "${DATA_DIR}/journal" "${DATA_DIR}/transcripts" "${DATA_DIR}/neocortex" "${DATA_DIR}/.matrix"
chown 0:0 "${DATA_DIR}/neocortex"

# Neo's user-confirmed recovery controls operate only on these two supervised
# service registries and the bridge baked into this image. Keeping the targets
# explicit prevents a recovery request from expanding to arbitrary directories.
export MATRIX_EXEC_BRIDGE_PATH="${MATRIX_EXEC_BRIDGE_PATH:-${MATRIX_HOME}/tools/exec/exec.mjs}"
export MATRIX_RECOVERY_EXEC_STATE_DIRS="${MATRIX_RECOVERY_EXEC_STATE_DIRS:-${DATA_DIR}/services:${DATA_DIR}/neo/services}"

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
    local memory_mode="${2:-enabled}"
    DAEMON_ARGV=(
        "${MATRIX_HOME}/bin/mcl-execute" daemon
        -addr "${addr}"
        -manifest "${MATRIX_HOME}/agents/default.json"
        -skills-root "${MATRIX_HOME}/skills"
        -journal-dir "${DATA_DIR}/journal"
        -transcripts-dir "${DATA_DIR}/transcripts"
        -keyfile "${DATA_DIR}/.matrix/executor.key"
        -did "${MATRIX_USER_ID:-executor}"
        -snapshot-data-dir "${DATA_DIR}"
        -workspace-root "${MATRIX_WORKSPACE_ROOT:-${DATA_DIR}/workspace}"
    )
    if [[ "${memory_mode}" == "disabled" ]]; then
        DAEMON_ARGV+=( -memory-disabled )
    else
        DAEMON_ARGV+=(
            -cortex-root "${DATA_DIR}/cortex"
            -cortex-actor "${MATRIX_USER_ID:-executor}"
        )
    fi
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

random_hex() {
    local bytes="$1"
    od -An -N "${bytes}" -tx1 /dev/urandom | tr -d '[:space:]'
}

prepare_neocortex_config() {
    local state_dir="${DATA_DIR}/neocortex"
    local key_path="${state_dir}/bootstrap.keys"
    local config_path="${state_dir}/engine.conf"
    local log_path="${state_dir}/engine.log"
    local socket_path="${NEO_NEOCORTEX_SOCKET:-${state_dir}/engine.sock}"
    local key_tmp config_tmp key value
    local user_id="" kek="" signing_seed="" admin_token=""

    if [[ -L "${state_dir}" || -L "${key_path}" || -L "${config_path}" || -L "${log_path}" ]]; then
        echo "entrypoint: refusing symlinked Neocortex state or configuration" >&2
        return 1
    fi
    if [[ "${socket_path}" != /* || "${socket_path}" == *$'\n'* || "$(dirname -- "${socket_path}")" != "${state_dir}" ]]; then
        echo "entrypoint: NEO_NEOCORTEX_SOCKET must be a direct child of ${state_dir}" >&2
        return 1
    fi
    if [[ ! "${NEOCORTEX_ACTOR_TOKEN}" =~ ^[[:xdigit:]]{64}$ ]]; then
        echo "entrypoint: Neocortex requires a 64-hex NEO_NEOCORTEX_TOKEN" >&2
        return 1
    fi

    mkdir -p "${state_dir}/data"
    chown 0:0 "${state_dir}" "${state_dir}/data"
    chmod 0700 "${state_dir}" "${state_dir}/data"
    touch "${log_path}"
    chown 0:0 "${log_path}"
    chmod 0600 "${log_path}"
    if [[ ! -f "${key_path}" ]]; then
        key_tmp="$(mktemp "${state_dir}/.bootstrap.keys.XXXXXX")"
        chmod 0600 "${key_tmp}"
        {
            printf 'user=%s\n' "$(random_hex 16)"
            printf 'kek=%s\n' "$(random_hex 32)"
            printf 'signing_seed=%s\n' "$(random_hex 32)"
            printf 'admin_token=%s\n' "$(random_hex 32)"
        } >"${key_tmp}"
        mv -f "${key_tmp}" "${key_path}"
    fi
    chown 0:0 "${key_path}"
    chmod 0600 "${key_path}"

    while IFS='=' read -r key value; do
        case "${key}" in
            user) user_id="${value}" ;;
            kek) kek="${value}" ;;
            signing_seed) signing_seed="${value}" ;;
            admin_token) admin_token="${value}" ;;
            *)
                echo "entrypoint: invalid Neocortex bootstrap key file" >&2
                return 1
                ;;
        esac
    done <"${key_path}"
    if [[ ! "${user_id}" =~ ^[[:xdigit:]]{32}$ ||
          ! "${kek}" =~ ^[[:xdigit:]]{64}$ ||
          ! "${signing_seed}" =~ ^[[:xdigit:]]{64}$ ||
          ! "${admin_token}" =~ ^[[:xdigit:]]{64}$ ||
          "${admin_token,,}" == "${NEOCORTEX_ACTOR_TOKEN,,}" ]]; then
        echo "entrypoint: invalid Neocortex bootstrap key material" >&2
        return 1
    fi

    config_tmp="$(mktemp "${state_dir}/.cortexd.conf.XXXXXX")"
    chmod 0600 "${config_tmp}"
    {
        printf 'socket=%s\n' "${socket_path}"
        printf 'data=%s\n' "${state_dir}/data"
        printf 'user=%s\n' "${user_id}"
        printf 'kek=%s\n' "${kek}"
        printf 'signing_seed=%s\n' "${signing_seed}"
        printf 'admin_token=%s\n' "${admin_token}"
        printf 'actor=1:%s\n' "${NEOCORTEX_ACTOR_TOKEN,,}"
    } >"${config_tmp}"
    mv -f "${config_tmp}" "${config_path}"
    chown 0:0 "${config_path}"

    export NEO_NEOCORTEX_SOCKET="${socket_path}"
    NEOCORTEX_CONFIG_PATH="${config_path}"
    NEOCORTEX_LOG_PATH="${log_path}"
}

start_neocortex() {
    prepare_neocortex_config

    (
        set +e
        local child_pid="" status=0
        trap 'if [[ -n "${child_pid}" ]]; then kill "${child_pid}" 2>/dev/null; wait "${child_pid}" 2>/dev/null; fi; exit 0' TERM INT
        while true; do
            if [[ -L "${NEO_NEOCORTEX_SOCKET}" || ( -e "${NEO_NEOCORTEX_SOCKET}" && ! -S "${NEO_NEOCORTEX_SOCKET}" ) ]]; then
                echo "entrypoint: refusing non-socket Neocortex path ${NEO_NEOCORTEX_SOCKET}" >&2
                exit 1
            fi
            if [[ -S "${NEO_NEOCORTEX_SOCKET}" ]]; then
                rm -f -- "${NEO_NEOCORTEX_SOCKET}"
            fi
            "${MATRIX_HOME}/bin/cortexd" --config "${NEOCORTEX_CONFIG_PATH}" \
                >>"${NEOCORTEX_LOG_PATH}" 2>&1 &
            child_pid=$!
            wait "${child_pid}"
            status=$?
            child_pid=""
            echo "entrypoint: Neocortex exited (status ${status}); restarting in 1s" >&2
            sleep 1
        done
    ) &
    NEOCORTEX_SUPERVISOR_PID=$!

    local i
    for (( i = 0; i < 80; i++ )); do
        if [[ -S "${NEO_NEOCORTEX_SOCKET}" ]]; then
            return 0
        fi
        if ! kill -0 "${NEOCORTEX_SUPERVISOR_PID}" 2>/dev/null; then
            echo "entrypoint: Neocortex supervisor exited before readiness" >&2
            return 1
        fi
        sleep 0.25
    done
    echo "entrypoint: Neocortex socket did not become ready" >&2
    kill "${NEOCORTEX_SUPERVISOR_PID}" 2>/dev/null || true
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

bootstrap_workforce_codegraph() {
    local workforce_repository="${WORKFORCE_DEVELOPER_REPOSITORY:-/workspace}"
    mkdir -p "${workforce_repository}/.cg"
    if "${WORKFORCE_CODEGRAPH_EXECUTABLE:-/usr/local/bin/cg}" \
        --db "${workforce_repository}/.cg/codegraph.db" \
        build "${workforce_repository}" \
        --exclude .cg \
        --exclude .codegraph \
        --exclude .git \
        --exclude node_modules \
        --exclude .next \
        >>/tmp/workforce-codegraph.log 2>&1; then
        echo "entrypoint: Workforce CodeGraph bootstrap complete" >&2
    else
        echo "entrypoint: Workforce CodeGraph bootstrap failed; control plane remains available and Developer graph operations fail closed" >&2
    fi
}

start_workforce() {
    if [[ "${WORKFORCE_ENABLED:-false}" != "true" ]]; then
        return 0
    fi
    # Repository indexing can take minutes for an established workspace. It is
    # not part of HTTP readiness: bind the authenticated control plane now and
    # construct the durable Project Brain graph concurrently. Developer graph
    # operations remain fail-closed until the real build completes.
    bootstrap_workforce_codegraph &
    exec "${MATRIX_HOME}/bin/workforced" \
        -serve \
        -listen ":${WORKFORCE_PORT:-8091}" \
        "$@"
}

case "${1:-neo}" in
    neo)
        shift || true

        # Neocortex is mandatory and ready before either Neo process starts.
        start_neocortex

        # Per-user local browser (BROWSER-FILMSTRIP): boot it first so
        # MATRIX_BROWSER_URL is exported before the daemon/neo spawn their
        # browser.mjs bridges (the bridge reads it at spawn).
        start_local_browser
        start_voice_controller

		# Workforce is a separate deterministic runtime, co-located only at
		# the service boundary so the Router can address the same per-user
		# private hostname on :8091. It keeps its own Postgres, Vault,
		# authority, scheduler, Bubblewrap workers, and process lifecycle.
		if [[ "${WORKFORCE_ENABLED:-false}" == "true" ]]; then
			start_workforce &
			WORKFORCE_PID=$!
			# The background Workforce process received its own environment copy.
			# Remove its database and authority material before spawning Neo, the
			# plumbing daemon, or any MCP child process.
			unset WORKFORCE_POSTGRES_URI WORKFORCE_OWNER_TOKEN WORKFORCE_WAKE_TOKEN
			unset WORKFORCE_OWNER_PUBLIC_KEY WORKFORCE_RUNTIME_PRIVATE_KEY
			unset WORKFORCE_OWNER_KEY_ID WORKFORCE_RUNTIME_KEY_ID
		fi

        # Backend: the plumbing daemon on :8081 (background). Neo
        # reverse-proxies non-memory plumbing routes to it; Neo owns memory.
        build_daemon_argv ":${NEO_BACKEND_PORT}" disabled
        "${DAEMON_ARGV[@]}" &
        DAEMON_PID=$!

        # Best-effort wait so Neo's proxy + first core_execute land cleanly.
        # Non-fatal: Neo serves /chat without the backend; only delegation
        # and proxied routes need it (and the router re-probes /healthz).
        wait_for_health "http://127.0.0.1:${NEO_BACKEND_PORT}/healthz" 80 \
            || echo "entrypoint: backend daemon not ready on :${NEO_BACKEND_PORT} yet (continuing)" >&2

        # Front: Neo on :8080.
        #  - MATRIX_EXEC_STATE_DIR identifies Neo's legacy service registry for
        #    admin-only recovery and cleanup; it is not an agent tool surface.
        #  - Neocortex is the only memory engine available to Neo.
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

        NEO_PROCESS_ENV=("NEO_NEOCORTEX_TOKEN=${NEOCORTEX_ACTOR_TOKEN}")
        env "${NEO_PROCESS_ENV[@]}" "${MATRIX_HOME}/bin/neo" serve \
            -addr ":8080" \
            -backend "http://127.0.0.1:${NEO_BACKEND_PORT}" \
            -manifest "${MATRIX_HOME}/agents/neo.json" \
            -data-root "${DATA_DIR}/neo" \
            -actor "neo" \
            "$@" &
        NEO_PID=$!
        unset NEOCORTEX_ACTOR_TOKEN NEO_PROCESS_ENV

        # If any required runtime exits, tear the others down and exit non-zero so the
        # platform's on-failure restart reboots the service. (tini -g also
        # forwards a platform stop/restart signal to the whole group.)
        set +e
        wait -n
        EXIT=$?
        set -e
        echo "entrypoint: a co-located process exited (status ${EXIT}); stopping the runtime" >&2
        kill "${DAEMON_PID}" "${NEO_PID}" ${NEOCORTEX_SUPERVISOR_PID:+"${NEOCORTEX_SUPERVISOR_PID}"} ${WORKFORCE_PID:+"${WORKFORCE_PID}"} ${BROWSER_PID:+"${BROWSER_PID}"} ${VOICE_CONTROLLER_PID:+"${VOICE_CONTROLLER_PID}"} 2>/dev/null || true
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
        export WORKFORCE_ENABLED=true
        start_workforce "$@"
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
