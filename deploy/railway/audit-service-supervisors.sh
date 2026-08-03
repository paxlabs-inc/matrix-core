#!/usr/bin/env bash
set -euo pipefail

PROJECT_ID="${RAILWAY_PROJECT_ID:-c0eef468-7b23-43cf-9ec7-1e7ed155986f}"
ENVIRONMENT="${RAILWAY_ENVIRONMENT:-Production}"
SERVICES=(
  matrix-d1015d90-c85f-4e84-8ca6-08
  matrix-c43702a2-76d9-4a80-ba28-2c
  matrix-33128b9b-3c40-4cca-af5f-9b
  matrix-b25093e3-8d6c-4ebe-b714-96
)

command -v railway >/dev/null 2>&1 || {
  echo "railway CLI is required" >&2
  exit 1
}

if [[ $# -gt 0 ]]; then
  SERVICES=("$@")
fi

for service in "${SERVICES[@]}"; do
  echo "service=${service}"
  # shellcheck disable=SC2016
  railway ssh \
    --project "${PROJECT_ID}" \
    --environment "${ENVIRONMENT}" \
    --service "${service}" \
    bash -lc '
      set -euo pipefail
      for state_dir in /data/services /data/neo/services; do
        echo "registry=${state_dir}"
        MATRIX_EXEC_STATE_DIR="${state_dir}" \
          node /opt/matrix/tools/exec/exec.mjs --audit-registry
      done
    '
done
