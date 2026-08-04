#!/bin/sh
set -eu

router_admin_url="${ROUTER_ADMIN_URL:-http://127.0.0.1:${ROUTER_HEALTHCHECK_PORT:-8088}}"
concurrency="${NEO_RECONCILE_CONCURRENCY:-3}"
neocortex_token="${NEO_NEOCORTEX_TOKEN:-}"

if [ -z "${ROUTER_ADMIN_TOKEN:-}" ]; then
    echo "reconcile-neo-fleet: ROUTER_ADMIN_TOKEN is required" >&2
    exit 2
fi

case "$concurrency" in
    ''|*[!0-9]*)
        echo "reconcile-neo-fleet: NEO_RECONCILE_CONCURRENCY must be an integer from 1 to 8" >&2
        exit 2
        ;;
esac
if [ "$concurrency" -lt 1 ] || [ "$concurrency" -gt 8 ]; then
    echo "reconcile-neo-fleet: NEO_RECONCILE_CONCURRENCY must be an integer from 1 to 8" >&2
    exit 2
fi

case "$neocortex_token" in
    ''|*[!0-9a-fA-F]*)
        echo "reconcile-neo-fleet: NEO_NEOCORTEX_TOKEN must be exactly 64 hexadecimal characters" >&2
        exit 2
        ;;
esac
if [ "${#neocortex_token}" -ne 64 ]; then
    echo "reconcile-neo-fleet: NEO_NEOCORTEX_TOKEN must be exactly 64 hexadecimal characters" >&2
    exit 2
fi

response="$(
    printf '{"concurrency":%s,"neocortex_token":"%s"}' "$concurrency" "$neocortex_token" |
        curl --fail-with-body --silent --show-error \
            --request POST \
            --header "Authorization: Bearer ${ROUTER_ADMIN_TOKEN}" \
            --header "Content-Type: application/json" \
            --data-binary @- \
            "${router_admin_url%/}/admin/neo/reconcile"
)"

printf '%s\n' "$response"
