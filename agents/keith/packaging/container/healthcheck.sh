#!/usr/bin/env bash
set -Eeuo pipefail

data_root="${KEITH_DATA_ROOT:-/var/lib/keith}"
port="${PORT:-7341}"
socket="${KEITH_DAEMON_SOCKET:-${data_root}/agentd.sock}"

[[ -S "$socket" ]]
curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:${port}/login" >/dev/null

