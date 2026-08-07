#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

CENTRAL_PROJECT_ID="${RAILWAY_CENTRAL_PROJECT_ID:-c0eef468-7b23-43cf-9ec7-1e7ed155986f}"
CENTRAL_ENVIRONMENT_ID="${RAILWAY_CENTRAL_ENVIRONMENT_ID:-81c6c189-6cf7-4ddc-9524-6876f9af53e7}"
CENTRAL_SERVICE_ID="${RAILWAY_CENTRAL_SERVICE_ID:-9d743506-9ed5-4e61-8890-09b00036eca4}"
DAEMON_IMAGE="${MATRIX_RAILWAY_DAEMON_IMAGE:-ghcr.io/paxlabs-inc/matrix-core/matrix-daemon-railway:latest}"
RAILWAY_BIN="${RAILWAY_BIN:-railway}"

mode="plan"
execute="false"
expected_binary_sha=""
expected_image_digest="sha256:0b2f238db3aaaeb401446b930a2d04c4a898be283950f92fcf3a120be1eaf2f2"
binary_path=""
concurrency=3
canaries_per_shard=1
timeout_seconds=600
force="false"
declare -a selected_shards=()

usage() {
  cat <<'USAGE'
Usage:
  rollout-neo-daemon-fleet.sh plan [options]
  rollout-neo-daemon-fleet.sh rollout --execute --binary PATH --expected-image-digest DIGEST [options]
  rollout-neo-daemon-fleet.sh rollout --execute --expected-binary-sha SHA256 --expected-image-digest DIGEST [options]

Commands:
  plan       Discover every registered shard and print the daemon rollout set.
  rollout    Redeploy the configured daemon image from source, canary first.

Safety:
  plan is the default and never mutates Railway.
  rollout additionally requires --execute and
  MATRIX_RAILWAY_FLEET_ROLLOUT_APPROVED=YES.
  Every changed daemon must answer /healthz and expose the expected Neo binary
  SHA-256 before the next batch begins. A failed canary stops the fleet rollout.

Options:
  --binary PATH                 Derive the expected Neo SHA-256 from a local binary.
  --expected-binary-sha SHA256  Expected /opt/matrix/bin/neo SHA-256 after rollout.
  --expected-image-digest VALUE Expected deployed OCI digest, including sha256:.
  --image IMAGE                 Exact configured Railway daemon image to pull.
  --concurrency N               Non-canary batch size, 1-8 (default: 3).
  --canaries-per-shard N        Sequential canaries per shard, 0-3 (default: 1).
  --timeout-seconds N           Per-service deployment timeout (default: 600).
  --shard SHARD_ID              Limit to a shard; repeatable.
  --force                       Redeploy even when the running binary already matches.
  --central-project ID          Override the central Railway project.
  --central-environment ID      Override the central Railway environment.
  --central-service ID          Override the central router service.
  --execute                     Required mutation acknowledgement.
  -h, --help                    Show this help.

The image must already contain the new Neo binary. This script does not publish
an image; it pulls each service's configured image with --from-source.
USAGE
}

die() {
  echo "rollout-neo-daemon-fleet: $*" >&2
  exit 1
}

is_uint() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

if [[ $# -gt 0 && "$1" != -* ]]; then
  mode="$1"
  shift
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --execute)
      execute="true"
      shift
      ;;
    --binary)
      [[ $# -ge 2 ]] || die "--binary requires a path"
      binary_path="$2"
      shift 2
      ;;
    --expected-binary-sha)
      [[ $# -ge 2 ]] || die "--expected-binary-sha requires a value"
      expected_binary_sha="${2,,}"
      shift 2
      ;;
    --expected-image-digest)
      [[ $# -ge 2 ]] || die "--expected-image-digest requires a value"
      expected_image_digest="${2,,}"
      shift 2
      ;;
    --image)
      [[ $# -ge 2 ]] || die "--image requires a value"
      DAEMON_IMAGE="$2"
      shift 2
      ;;
    --concurrency)
      [[ $# -ge 2 ]] || die "--concurrency requires a value"
      concurrency="$2"
      shift 2
      ;;
    --canaries-per-shard)
      [[ $# -ge 2 ]] || die "--canaries-per-shard requires a value"
      canaries_per_shard="$2"
      shift 2
      ;;
    --timeout-seconds)
      [[ $# -ge 2 ]] || die "--timeout-seconds requires a value"
      timeout_seconds="$2"
      shift 2
      ;;
    --shard)
      [[ $# -ge 2 ]] || die "--shard requires a value"
      selected_shards+=("$2")
      shift 2
      ;;
    --force)
      force="true"
      shift
      ;;
    --central-project)
      [[ $# -ge 2 ]] || die "--central-project requires a value"
      CENTRAL_PROJECT_ID="$2"
      shift 2
      ;;
    --central-environment)
      [[ $# -ge 2 ]] || die "--central-environment requires a value"
      CENTRAL_ENVIRONMENT_ID="$2"
      shift 2
      ;;
    --central-service)
      [[ $# -ge 2 ]] || die "--central-service requires a value"
      CENTRAL_SERVICE_ID="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ "$mode" == "plan" || "$mode" == "rollout" ]] || die "command must be plan or rollout"
if ! is_uint "$concurrency" || (( concurrency < 1 || concurrency > 8 )); then
  die "--concurrency must be 1-8"
fi
if ! is_uint "$canaries_per_shard" || (( canaries_per_shard > 3 )); then
  die "--canaries-per-shard must be 0-3"
fi
if ! is_uint "$timeout_seconds" || (( timeout_seconds < 60 )); then
  die "--timeout-seconds must be at least 60"
fi

command -v "$RAILWAY_BIN" >/dev/null 2>&1 || die "Railway CLI not found: $RAILWAY_BIN"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
"$RAILWAY_BIN" whoami >/dev/null 2>&1 || die "Railway CLI is not authenticated"

if [[ -n "$binary_path" ]]; then
  [[ -f "$binary_path" && -r "$binary_path" ]] || die "binary is not readable: $binary_path"
  local_binary_sha="$(sha256sum "$binary_path" | awk '{print tolower($1)}')"
  if [[ -n "$expected_binary_sha" && "$expected_binary_sha" != "$local_binary_sha" ]]; then
    die "--binary and --expected-binary-sha do not match"
  fi
  expected_binary_sha="$local_binary_sha"
fi

if [[ -n "$expected_binary_sha" && ! "$expected_binary_sha" =~ ^[0-9a-f]{64}$ ]]; then
  die "expected binary SHA must be exactly 64 hexadecimal characters"
fi
if [[ -n "$expected_image_digest" && ! "$expected_image_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  die "expected image digest must be sha256 followed by exactly 64 hexadecimal characters"
fi

if [[ "$mode" == "rollout" ]]; then
  [[ "$execute" == "true" ]] || die "rollout requires --execute"
  [[ "${MATRIX_RAILWAY_FLEET_ROLLOUT_APPROVED:-}" == "YES" ]] ||
    die "export MATRIX_RAILWAY_FLEET_ROLLOUT_APPROVED=YES after owner approval"
  [[ -n "$expected_binary_sha" ]] || die "rollout requires --binary or --expected-binary-sha"
  [[ -n "$expected_image_digest" ]] || die "rollout requires --expected-image-digest"
fi

work_dir="$(mktemp -d /tmp/matrix-railway-neo-rollout.XXXXXX)"
cleanup() {
  local status=$?
  if [[ -n "${work_dir:-}" && "$work_dir" == /tmp/matrix-railway-neo-rollout.* && -d "$work_dir" ]]; then
    rm -rf -- "$work_dir"
  fi
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

registry="$($RAILWAY_BIN variable list \
  --project "$CENTRAL_PROJECT_ID" \
  --environment "$CENTRAL_ENVIRONMENT_ID" \
  --service "$CENTRAL_SERVICE_ID" \
  --json | jq -er '.ROUTER_RAILWAY_SHARDS')" || die "could not load ROUTER_RAILWAY_SHARDS"

jq -e '
  type == "array" and length > 0 and
  all(.[];
    (.id | type == "string" and length > 0) and
    (.project_id | type == "string" and length > 0) and
    (.environment_id | type == "string" and length > 0) and
    (.router_service_id | type == "string" and length > 0)
  ) and
  ([.[].id] | length == (unique | length))
' <<<"$registry" >/dev/null || die "ROUTER_RAILWAY_SHARDS is invalid"

shard_selected() {
  local candidate="$1" selected
  if (( ${#selected_shards[@]} == 0 )); then
    return 0
  fi
  for selected in "${selected_shards[@]}"; do
    [[ "$candidate" == "$selected" ]] && return 0
  done
  return 1
}

fleet_file="$work_dir/fleet.tsv"
mismatch_file="$work_dir/mismatches.tsv"
: >"$fleet_file"
: >"$mismatch_file"

declare -A discovered_shards=()
while IFS=$'\t' read -r shard_id project_id environment_id router_service_id; do
  shard_selected "$shard_id" || continue
  discovered_shards["$shard_id"]=1
  services="$($RAILWAY_BIN service list \
    --project "$project_id" \
    --environment "$environment_id" \
    --json)" || die "could not list services for $shard_id"
  jq -e 'type == "array"' <<<"$services" >/dev/null || die "invalid service list for $shard_id"

  while IFS=$'\t' read -r service_id service_name source_image status deployment_id data_volume; do
    [[ "$service_id" != "$router_service_id" ]] || continue
    if [[ "$source_image" != "$DAEMON_IMAGE" || "$data_volume" != "true" ]]; then
      printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$shard_id" "$service_id" "$service_name" "$source_image" "$status" "$data_volume" >>"$mismatch_file"
      continue
    fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$shard_id" "$project_id" "$environment_id" "$service_id" \
      "$service_name" "$source_image" "$status" "$deployment_id" >>"$fleet_file"
  done < <(jq -r '
    .[]
    | select(.name | test("^matrix-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{2}$"))
    | [
        .id,
        .name,
        (.source.image // ""),
        (.status // "UNKNOWN"),
        (.deploymentId // ""),
        (any(.volumes[]?; .mountPath == "/data") | tostring)
      ]
    | @tsv
  ' <<<"$services")
done < <(jq -r '.[] | [.id,.project_id,.environment_id,.router_service_id] | @tsv' <<<"$registry")

if (( ${#selected_shards[@]} > 0 )); then
  for selected in "${selected_shards[@]}"; do
    [[ -n "${discovered_shards[$selected]:-}" ]] || die "selected shard is not registered: $selected"
  done
fi

if [[ -s "$mismatch_file" ]]; then
  echo "Blocked: deterministic daemon services with an unexpected image or missing /data volume:" >&2
  awk -F $'\t' '{printf "  shard=%s service=%s name=%s image=%s status=%s data_volume=%s\n",$1,$2,$3,$4,$5,$6}' \
    "$mismatch_file" >&2
  die "fix or explicitly match the daemon image before rollout"
fi

sort -t $'\t' -k1,1 -k5,5 -o "$fleet_file" "$fleet_file"
fleet_count="$(wc -l <"$fleet_file" | tr -d ' ')"
(( fleet_count > 0 )) || die "no daemon services matched the registered shards and image"

echo "Neo daemon fleet plan"
echo "  image: $DAEMON_IMAGE"
echo "  services: $fleet_count"
echo "  shards:"
awk -F $'\t' '{count[$1]++} END {for (shard in count) printf "    %s: %d\n",shard,count[shard]}' "$fleet_file" | sort
printf '\n%-9s %-38s %-37s %-10s\n' "SHARD" "SERVICE" "SERVICE ID" "STATUS"
while IFS=$'\t' read -r shard_id _project_id _environment_id service_id service_name _source_image status _deployment_id; do
  printf '%-9s %-38s %-37s %-10s\n' "$shard_id" "$service_name" "$service_id" "$status"
done <"$fleet_file"

if [[ "$mode" == "plan" ]]; then
  echo
  echo "Plan only; Railway was not mutated."
  exit 0
fi

remote_binary_sha() {
  local project_id="$1" environment_id="$2" service_id="$3" output
  output="$($RAILWAY_BIN ssh \
    --project "$project_id" \
    --environment "$environment_id" \
    --service "$service_id" \
    sh -lc 'sha256sum /opt/matrix/bin/neo' 2>&1)" || return 1
  awk '$2 == "/opt/matrix/bin/neo" && $1 ~ /^[0-9a-fA-F]{64}$/ {print tolower($1)}' <<<"$output" | tail -n 1
}

verify_remote() {
  local project_id="$1" environment_id="$2" service_id="$3" deadline output actual_sha
  deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if output="$($RAILWAY_BIN ssh \
      --project "$project_id" \
      --environment "$environment_id" \
      --service "$service_id" \
      sh -lc 'set -eu; curl -fsS http://127.0.0.1:8080/healthz >/dev/null; sha256sum /opt/matrix/bin/neo' 2>&1)"; then
      actual_sha="$(awk '$2 == "/opt/matrix/bin/neo" && $1 ~ /^[0-9a-fA-F]{64}$/ {print tolower($1)}' <<<"$output" | tail -n 1)"
      [[ "$actual_sha" == "$expected_binary_sha" ]] || {
        echo "binary SHA mismatch: expected=$expected_binary_sha actual=${actual_sha:-unreadable}" >&2
        return 1
      }
      return 0
    fi
    sleep 5
  done
  echo "health/SHA verification timed out" >&2
  return 1
}

wait_for_deployment() {
  local project_id="$1" environment_id="$2" service_id="$3" before_id="$4"
  local deadline status_json deployment_id status
  deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if status_json="$($RAILWAY_BIN service status \
      --project "$project_id" \
      --environment "$environment_id" \
      --service "$service_id" \
      --json 2>/dev/null)"; then
      deployment_id="$(jq -r '.deploymentId // ""' <<<"$status_json")"
      status="$(jq -r '.status // "UNKNOWN"' <<<"$status_json")"
      if [[ -n "$deployment_id" && "$deployment_id" != "$before_id" ]]; then
        case "$status" in
          SUCCESS|SLEEPING)
            printf '%s\n' "$deployment_id"
            return 0
            ;;
          FAILED|CRASHED|REMOVED|SKIPPED)
            echo "deployment $deployment_id reached terminal status $status" >&2
            return 1
            ;;
        esac
      fi
    fi
    sleep 3
  done
  echo "new deployment did not become healthy before timeout" >&2
  return 1
}

rollout_one() {
  local row="$1"
  local shard_id project_id environment_id service_id service_name source_image status before_id
  local current_sha current_meta current_image_digest new_deployment deployment_meta image_digest
  IFS=$'\t' read -r shard_id project_id environment_id service_id service_name source_image status before_id <<<"$row"
  echo "START shard=$shard_id service=$service_name id=$service_id"

  if [[ "$force" != "true" ]]; then
    current_sha="$(remote_binary_sha "$project_id" "$environment_id" "$service_id" || true)"
    current_meta="$($RAILWAY_BIN deployment list \
      --project "$project_id" \
      --environment "$environment_id" \
      --service "$service_id" \
      --limit 1 \
      --json 2>/dev/null || true)"
    current_image_digest="$(jq -r '.[0].meta.imageDigest // ""' <<<"${current_meta:-[]}" 2>/dev/null || true)"
    if [[ "$current_sha" == "$expected_binary_sha" && "$current_image_digest" == "$expected_image_digest" ]]; then
      echo "SKIP shard=$shard_id service=$service_name reason=already-current image_digest=$current_image_digest neo_sha=$current_sha"
      return 0
    fi
  fi

  status_json="$($RAILWAY_BIN service status \
    --project "$project_id" \
    --environment "$environment_id" \
    --service "$service_id" \
    --json)"
  before_id="$(jq -r '.deploymentId // ""' <<<"$status_json")"
  [[ -n "$before_id" ]] || {
    echo "service has no current deployment" >&2
    return 1
  }

  $RAILWAY_BIN redeploy \
    --project "$project_id" \
    --environment "$environment_id" \
    --service "$service_id" \
    --from-source \
    --yes \
    --json >/dev/null

  new_deployment="$(wait_for_deployment "$project_id" "$environment_id" "$service_id" "$before_id")" || return 1
  deployment_meta="$($RAILWAY_BIN deployment list \
    --project "$project_id" \
    --environment "$environment_id" \
    --service "$service_id" \
    --limit 1 \
    --json)"
  [[ "$(jq -r '.[0].id // ""' <<<"$deployment_meta")" == "$new_deployment" ]] || {
    echo "latest deployment changed during verification" >&2
    return 1
  }
  [[ "$(jq -r '.[0].meta.image // ""' <<<"$deployment_meta")" == "$source_image" ]] || {
    echo "deployed image source does not match $source_image" >&2
    return 1
  }
  image_digest="$(jq -r '.[0].meta.imageDigest // "unknown"' <<<"$deployment_meta")"
  [[ "$image_digest" == "$expected_image_digest" ]] || {
    echo "image digest mismatch: expected=$expected_image_digest actual=$image_digest" >&2
    return 1
  }
  verify_remote "$project_id" "$environment_id" "$service_id" || return 1
  echo "PASS shard=$shard_id service=$service_name deployment=$new_deployment image_digest=$image_digest neo_sha=$expected_binary_sha"
}

run_rows() {
  local input_file="$1" phase="$2" parallelism="$3"
  local -a rows=() pids=() logs=()
  local row index batch_end pid log_file failed
  mapfile -t rows <"$input_file"
  (( ${#rows[@]} > 0 )) || return 0
  echo
  echo "Starting $phase phase: ${#rows[@]} service(s), concurrency=$parallelism"
  index=0
  while (( index < ${#rows[@]} )); do
    batch_end=$((index + parallelism))
    (( batch_end > ${#rows[@]} )) && batch_end=${#rows[@]}
    pids=()
    logs=()
    while (( index < batch_end )); do
      row="${rows[$index]}"
      log_file="$work_dir/${phase}-${index}.log"
      rollout_one "$row" >"$log_file" 2>&1 &
      pids+=("$!")
      logs+=("$log_file")
      ((index += 1))
    done
    failed=0
    for pid in "${pids[@]}"; do
      wait "$pid" || failed=1
    done
    for log_file in "${logs[@]}"; do
      sed "s/^/[$phase] /" "$log_file"
    done
    (( failed == 0 )) || {
      echo "$phase batch failed; no additional services will be started" >&2
      return 1
    }
  done
}

canary_file="$work_dir/canaries.tsv"
remainder_file="$work_dir/remainder.tsv"
if (( canaries_per_shard > 0 )); then
  awk -F $'\t' -v count="$canaries_per_shard" '{seen[$1]++; if (seen[$1] <= count) print}' "$fleet_file" >"$canary_file"
  awk -F $'\t' -v count="$canaries_per_shard" '{seen[$1]++; if (seen[$1] > count) print}' "$fleet_file" >"$remainder_file"
else
  : >"$canary_file"
  cp "$fleet_file" "$remainder_file"
fi

echo
echo "Executing approved fleet rollout"
echo "  expected image digest: $expected_image_digest"
echo "  expected Neo SHA-256: $expected_binary_sha"
echo "  canaries per shard: $canaries_per_shard"
echo "  batch concurrency: $concurrency"

run_rows "$canary_file" "canary" 1
run_rows "$remainder_file" "fleet" "$concurrency"

echo
echo "Fleet rollout complete: $fleet_count daemon service(s) are healthy on image $expected_image_digest and Neo $expected_binary_sha"
