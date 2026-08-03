#!/usr/bin/env bash
set -euo pipefail

PROJECT_ID="${RAILWAY_PROJECT_ID:-c0eef468-7b23-43cf-9ec7-1e7ed155986f}"
ENVIRONMENT="${RAILWAY_ENVIRONMENT:-Production}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORDER=(
  matrix-d1015d90-c85f-4e84-8ca6-08
  matrix-c43702a2-76d9-4a80-ba28-2c
  matrix-33128b9b-3c40-4cca-af5f-9b
  matrix-b25093e3-8d6c-4ebe-b714-96
)

usage() {
  echo "usage: $0 order | audit [service] | verify <service> | redeploy <service> | enforce <service>" >&2
}

known_service() {
  local candidate="$1" service
  for service in "${ORDER[@]}"; do
    [[ "${candidate}" == "${service}" ]] && return 0
  done
  return 1
}

require_service() {
  local service="${1:-}"
  if [[ -z "${service}" ]] || ! known_service "${service}"; then
    echo "service must be one of the four audited daemon services" >&2
    exit 2
  fi
}

require_approval() {
  [[ "${MATRIX_RAILWAY_ROLLOUT_APPROVED:-}" == "YES" ]] || {
    echo "production mutation blocked: export MATRIX_RAILWAY_ROLLOUT_APPROVED=YES after owner approval" >&2
    exit 3
  }
}

remote_exec() {
  local service="$1"
  shift
  railway ssh \
    --project "${PROJECT_ID}" \
    --environment "${ENVIRONMENT}" \
    --service "${service}" \
    "$@"
}

verify_service() {
  local service="$1"
  # shellcheck disable=SC2016
  remote_exec "${service}" bash -lc '
    set -euo pipefail
    curl -fsS http://127.0.0.1:8080/healthz >/dev/null
    test "${NEO_CODING_RUNTIME_ENABLED:-}" = "true"
    test "${NEO_CODING_RUNTIME_REQUIRED:-}" = "true"
    test -x /opt/agentcore/.venv/bin/agentcore
    test -r /etc/matrix-agentcore/config.yaml
    native_tools="$(curl -fsS http://127.0.0.1:8080/diag/native-tools)"
    jq -e "
      .enabled == true and .mode == \"in_process\" and
      ([\"read_text_file\",\"read_multiple_files\",\"write_file\",\"edit_file\",\"create_directory\",\"list_directory\",\"directory_tree\",\"move_file\",\"search_files\",\"get_file_info\",\"shell\",\"service_start\",\"service_list\",\"service_logs\",\"service_stop\",\"service_restart\",\"git_status\",\"git_diff\",\"git_log\",\"git_show\",\"git_branch\"] - .tools | length == 0)
    " <<<"${native_tools}" >/dev/null
    curl -fsS http://127.0.0.1:8080/build-jobs >/dev/null
    test -f "${MATRIX_EXEC_BRIDGE_PATH:-/opt/matrix/tools/exec/exec.mjs}"
    test -d /data/tmp
    test -d /data/cache
    test -d /data/neo/tmp
    test -d /data/neo/cache
    test -d /data/build-jobs
    test -d /data/agentcore
    test -d /data/native-services
    if test -f /data/native-services/services.json; then
      jq -e ".services | type == \"array\"" /data/native-services/services.json >/dev/null
      test "$(stat -c %a /data/native-services/services.json)" = "600"
      if jq -r ".services[].command // empty" /data/native-services/services.json |
        grep -Eiq "(TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|PRIVATE_KEY)[[:space:]]*=|Authorization:[[:space:]]*Bearer|https?://[^/@[:space:]]+:[^/@[:space:]]+@|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}"; then
        echo "native service registry contains an inline credential" >&2
        exit 1
      fi
    fi
    for manifest in /opt/matrix/agents/default.json /opt/matrix/agents/neo.json; do
      test -f "${manifest}"
      jq -e '"'"'[.servers[].alias] | all(. != "fs" and . != "git" and . != "exec")'"'"' "${manifest}" >/dev/null
    done
    recovery_probe="$(mktemp)"
    recovery_status="$(curl -sS -o "${recovery_probe}" -w "%{http_code}" \
      -X POST -H "content-type: application/json" \
      --data "{\"confirm\":\"VERIFY_ONLY\"}" \
      http://127.0.0.1:8080/recovery/clean-environment)"
    test "${recovery_status}" = "400"
    grep -q "confirm must equal CLEAN" "${recovery_probe}"
    rm -f "${recovery_probe}"
    for state_dir in /data/services /data/neo/services; do
      MATRIX_EXEC_STATE_DIR="${state_dir}" \
        node /opt/matrix/tools/exec/exec.mjs --verify-registry
    done
  '
}

case "${1:-}" in
  order)
    printf '%s\n' "${ORDER[@]}"
    ;;
  audit)
    shift
    if [[ $# -eq 0 ]]; then
      "${SCRIPT_DIR}/audit-service-supervisors.sh"
    else
      require_service "$1"
      "${SCRIPT_DIR}/audit-service-supervisors.sh" "$1"
    fi
    ;;
  verify)
    require_service "${2:-}"
    verify_service "$2"
    ;;
  redeploy)
    require_service "${2:-}"
    require_approval
    railway redeploy \
      --project "${PROJECT_ID}" \
      --environment "${ENVIRONMENT}" \
      --service "$2" \
      --from-source \
      --yes
    echo "redeploy requested for $2; run: $0 verify $2" >&2
    ;;
  enforce)
    require_service "${2:-}"
    require_approval
    # shellcheck disable=SC2016
    remote_exec "$2" bash -lc '
      set -euo pipefail
      for state_dir in /data/services /data/neo/services; do
        MATRIX_EXEC_STATE_DIR="${state_dir}" \
          node /opt/matrix/tools/exec/exec.mjs --verify-no-inline-credentials
      done
    '
    railway variable set \
      --project "${PROJECT_ID}" \
      --environment "${ENVIRONMENT}" \
      --service "$2" \
      --skip-deploys \
      MATRIX_EXEC_INLINE_SECRET_POLICY=block
    railway redeploy \
      --project "${PROJECT_ID}" \
      --environment "${ENVIRONMENT}" \
      --service "$2" \
      --from-source \
      --yes
    echo "block policy and redeploy requested for $2; run: $0 verify $2" >&2
    ;;
  *)
    usage
    exit 2
    ;;
esac
