#!/usr/bin/env bash
set -Eeuo pipefail

data_root="${KEITH_DATA_ROOT:-/var/lib/keith}"
workspace_root="${KEITH_WORKSPACE_ROOT:-/workspace}"
asset_root="${KEITH_ASSET_ROOT:-/opt/keith/web}"
port="${PORT:-7341}"
provider_catalog="${KEITH_PROVIDER_CATALOG:-/opt/keith/providers/providers.json}"

normalize_runtime_directory() {
  local path="$1"
  local label="$2"
  if [[ "$path" != /* || "$path" == "/" ]]; then
    echo "${label} must be an absolute, non-root path" >&2
    exit 64
  fi
  case "$path" in
    /bin|/bin/*|/boot|/boot/*|/dev|/dev/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/root|/root/*|/run|/run/*|/sbin|/sbin/*|/sys|/sys/*|/usr|/usr/*|/var)
      echo "${label} cannot use protected system path ${path}" >&2
      exit 64
      ;;
  esac
  if [[ -L "$path" ]]; then
    echo "${label} cannot be a symbolic link" >&2
    exit 64
  fi
  path="$(realpath -m -- "$path")"
  case "$path" in
    /|/bin|/bin/*|/boot|/boot/*|/dev|/dev/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/root|/root/*|/run|/run/*|/sbin|/sbin/*|/sys|/sys/*|/usr|/usr/*|/var)
      echo "${label} resolves to protected system path ${path}" >&2
      exit 64
      ;;
  esac
  printf '%s\n' "$path"
}

data_root="$(normalize_runtime_directory "$data_root" "KEITH_DATA_ROOT")"
workspace_root="$(normalize_runtime_directory "$workspace_root" "KEITH_WORKSPACE_ROOT")"
socket="${KEITH_DAEMON_SOCKET:-${data_root}/agentd.sock}"
if [[ "$data_root" == "$workspace_root" \
  || "$data_root" == "$workspace_root/"* \
  || "$workspace_root" == "$data_root/"* ]]; then
  echo "KEITH_DATA_ROOT and KEITH_WORKSPACE_ROOT cannot overlap" >&2
  exit 64
fi

if [[ "$(id -u)" == "0" ]]; then
  keith_uid="$(id -u keith)"
  keith_gid="$(id -g keith)"
  mkdir -p "$data_root" "$workspace_root"
  chown --no-dereference keith:keith "$data_root" "$workspace_root"
  chmod 0700 "$data_root"
  chmod 0750 "$workspace_root"
  exec setpriv \
    --reuid="$keith_uid" \
    --regid="$keith_gid" \
    --init-groups \
    --no-new-privs \
    env HOME=/home/keith USER=keith LOGNAME=keith KEITH_ENTRYPOINT_PRIVILEGES_DROPPED=1 \
    "$0" "$@"
fi

mkdir -p "$data_root" "$workspace_root"
chmod 0700 "$data_root"
rm -f "$socket"

origin="${KEITH_PUBLIC_ORIGIN:-}"
if [[ -z "$origin" && -n "${RAILWAY_PUBLIC_DOMAIN:-}" ]]; then
  origin="https://${RAILWAY_PUBLIC_DOMAIN}"
elif [[ -z "$origin" && -n "${FLY_APP_NAME:-}" ]]; then
  origin="https://${FLY_APP_NAME}.fly.dev"
elif [[ -z "$origin" ]]; then
  origin="http://localhost:${port}"
fi

if [[ -z "${KEITH_WEB_LOGIN_SECRET:-}" ]]; then
  echo "KEITH_WEB_LOGIN_SECRET is required" >&2
  exit 64
fi

credential_args=()
if [[ -n "${KEITH_CREDENTIAL_KEY:-}" ]]; then
  credential_args=(--credential-key-env KEITH_CREDENTIAL_KEY)
fi

if [[ -f "$provider_catalog" ]]; then
  while IFS=$'\t' read -r provider credential_env; do
    [[ -n "$provider" && -n "$credential_env" ]] || continue
    # GITHUB_TOKEN is commonly a CI/registry token and is never imported implicitly.
    [[ "$credential_env" == "GITHUB_TOKEN" ]] && continue
    if [[ -n "${!credential_env:-}" ]]; then
      echo "Importing ${provider} credentials into Keith's encrypted store"
      agent-cli provider set \
        --provider "$provider" \
        --secret-env "$credential_env" \
        --data-root "$data_root" \
        "${credential_args[@]}"
      unset "$credential_env"
    fi
  done < <(jq -r '.providers[] | select(.credential_environment != "") | [.id, .credential_environment] | @tsv' "$provider_catalog")
fi

daemon_args=(
  --data-root "$data_root"
  --socket "$socket"
  --worker-executable /usr/local/bin/agent-worker
  --workspace-root "$workspace_root"
)
daemon_args+=("${credential_args[@]}")

IFS=',' read -r -a services <<< "${KEITH_SERVICES:-}"
for service in "${services[@]}"; do
  service="${service//[[:space:]]/}"
  [[ -n "$service" ]] && daemon_args+=(--enable-service "$service")
done

web_args=(
  --bind "0.0.0.0:${port}"
  --origin "$origin"
  --socket "$socket"
  --asset-root "$asset_root"
  --credential-root "$data_root/credentials"
)
web_args+=("${credential_args[@]}")

if [[ -n "${KEITH_OPENAI_COMPAT_API_KEY:-}" ]]; then
  web_args+=(--openai-allow-non-loopback true)
fi
if [[ -n "${KEITH_PLATFORM_API_KEY:-}" ]]; then
  export KEITH_PLATFORM_ALLOW_NON_LOOPBACK=true
fi

agentd "${daemon_args[@]}" &
daemon_pid=$!
web_pid=""

shutdown() {
  trap - TERM INT EXIT
  if [[ -n "$web_pid" ]]; then
    kill -TERM "$web_pid" 2>/dev/null || true
  fi
  kill -TERM "$daemon_pid" 2>/dev/null || true
  wait "$web_pid" 2>/dev/null || true
  wait "$daemon_pid" 2>/dev/null || true
}
trap shutdown TERM INT EXIT

for _ in $(seq 1 300); do
  kill -0 "$daemon_pid" 2>/dev/null || {
    wait "$daemon_pid"
    exit $?
  }
  [[ -S "$socket" ]] && break
  sleep 0.1
done
if [[ ! -S "$socket" ]]; then
  echo "agentd did not create ${socket} within 30 seconds" >&2
  exit 70
fi

agent-web "${web_args[@]}" &
web_pid=$!

while kill -0 "$daemon_pid" 2>/dev/null && kill -0 "$web_pid" 2>/dev/null; do
  sleep 1
done

if ! kill -0 "$daemon_pid" 2>/dev/null; then
  wait "$daemon_pid"
else
  wait "$web_pid"
fi
