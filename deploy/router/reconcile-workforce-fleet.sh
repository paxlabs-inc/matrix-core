#!/bin/sh
set -eu

router_admin_url="${ROUTER_ADMIN_URL:-http://127.0.0.1:${ROUTER_HEALTHCHECK_PORT:-8088}}"
concurrency="${WORKFORCE_RECONCILE_CONCURRENCY:-3}"

if [ -z "${ROUTER_ADMIN_TOKEN:-}" ]; then
    echo "reconcile-workforce-fleet: ROUTER_ADMIN_TOKEN is required" >&2
    exit 2
fi

case "$concurrency" in
    ''|*[!0-9]*)
        echo "reconcile-workforce-fleet: WORKFORCE_RECONCILE_CONCURRENCY must be an integer from 1 to 8" >&2
        exit 2
        ;;
esac
if [ "$concurrency" -lt 1 ] || [ "$concurrency" -gt 8 ]; then
    echo "reconcile-workforce-fleet: WORKFORCE_RECONCILE_CONCURRENCY must be an integer from 1 to 8" >&2
    exit 2
fi

response="$(curl --fail-with-body --silent --show-error \
    --request POST \
    --header "Authorization: Bearer ${ROUTER_ADMIN_TOKEN}" \
    --header "Content-Type: application/json" \
    --data "{\"concurrency\":${concurrency}}" \
    "${router_admin_url%/}/admin/workforce/reconcile")"

printf '%s\n' "$response"
