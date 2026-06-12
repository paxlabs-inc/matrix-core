#!/usr/bin/env bash
# deploy/chronos/install.sh — idempotent installer for chronosd (the Matrix
# centralized agent scheduler / wake control plane).
#
# Run as root on the box. Performs:
#   1. Create the matrix system user/group if missing.
#   2. Install the chronosd binary + migrations into /opt/matrix-chronos.
#   3. Write /etc/matrix/chronos.env (mode 0640 root:matrix).
#   4. Drop the systemd unit at /etc/systemd/system/chronosd.service.
#   5. Enable + start the service (chronosd runs its own DB migrations at boot).
#
# Usage:
#   ./install.sh \
#     --binary /path/to/chronosd \
#     --postgres-uri "postgres://matrix@localhost/matrix?sslmode=disable" \
#     --token        "<random-32-bytes-base64>"  \  # CHRONOS_TOKEN (transport)
#     --agent-secret "<random-32-bytes-base64>"  \  # CHRONOS_AGENT_AUTH_SECRET
#     --wake-token   "<must match ROUTER_WAKE_TOKEN>" \
#     [--router-wake-url http://127.0.0.1:8088/internal/wake]
#
# Re-running is safe: every step is idempotent. Fails fast (set -euo pipefail).
# TLS is out of scope (handled by the box-level certbot --nginx flow).

set -euo pipefail

BINARY=""
POSTGRES_URI=""
TOKEN=""
AGENT_SECRET=""
WAKE_TOKEN=""
ROUTER_WAKE_URL="http://127.0.0.1:8088/internal/wake"
INSTALL_DIR="/opt/matrix-chronos"
ENV_FILE="/etc/matrix/chronos.env"
SERVICE_FILE="/etc/systemd/system/chronosd.service"

usage() {
    cat <<EOF
Usage: $0 --binary PATH --postgres-uri URI --token TOKEN \\
       --agent-secret SECRET --wake-token TOKEN [--router-wake-url URL]

  --binary          Path to the compiled chronosd binary.
  --postgres-uri    Postgres URI (shared matrix DB).        -> CHRONOS_POSTGRES_URI
  --token           Shared transport bearer.                -> CHRONOS_TOKEN
  --agent-secret    HMAC secret for agent-DID principal.    -> CHRONOS_AGENT_AUTH_SECRET
  --wake-token      Shared secret matching the router.      -> CHRONOS_WAKE_TOKEN
  --router-wake-url Router internal wake endpoint.          -> CHRONOS_ROUTER_WAKE_URL
                    (default: $ROUTER_WAKE_URL)
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --binary)          BINARY="$2"; shift 2 ;;
        --postgres-uri)    POSTGRES_URI="$2"; shift 2 ;;
        --token)           TOKEN="$2"; shift 2 ;;
        --agent-secret)    AGENT_SECRET="$2"; shift 2 ;;
        --wake-token)      WAKE_TOKEN="$2"; shift 2 ;;
        --router-wake-url) ROUTER_WAKE_URL="$2"; shift 2 ;;
        -h|--help)         usage ;;
        *)                 echo "unknown flag $1"; usage ;;
    esac
done

[[ -z "$BINARY" || -z "$POSTGRES_URI" || -z "$TOKEN" || -z "$AGENT_SECRET" || -z "$WAKE_TOKEN" ]] && usage
[[ "$EUID" -eq 0 ]] || { echo "must run as root"; exit 1; }
[[ -x "$BINARY" ]] || { echo "binary not executable: $BINARY"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS_SRC="$SCRIPT_DIR/../../chronos/migrations"

# 1. user/group
if ! getent group matrix >/dev/null; then
    groupadd --system matrix
fi
if ! id -u matrix >/dev/null 2>&1; then
    useradd --system --gid matrix --home-dir "$INSTALL_DIR" \
        --shell /usr/sbin/nologin matrix
fi

# 2. binary + migrations
install -d -o matrix -g matrix -m 0755 "$INSTALL_DIR"
install -o matrix -g matrix -m 0755 "$BINARY" "$INSTALL_DIR/chronosd"
if [[ -d "$MIGRATIONS_SRC" ]]; then
    install -d -o matrix -g matrix -m 0755 "$INSTALL_DIR/migrations"
    install -o matrix -g matrix -m 0644 "$MIGRATIONS_SRC"/*.sql "$INSTALL_DIR/migrations/"
else
    echo "warning: $MIGRATIONS_SRC not found; chronosd will fail to migrate at boot."
fi

# 3. env file
install -d -m 0750 /etc/matrix
chown root:matrix /etc/matrix
umask 0027
cat > "$ENV_FILE" <<EOF
# Matrix chronos environment — managed by deploy/chronos/install.sh
CHRONOS_PORT=9096
CHRONOS_POSTGRES_URI=$POSTGRES_URI
CHRONOS_TOKEN=$TOKEN
CHRONOS_AGENT_AUTH_SECRET=$AGENT_SECRET
CHRONOS_WAKE_TOKEN=$WAKE_TOKEN
CHRONOS_ROUTER_WAKE_URL=$ROUTER_WAKE_URL
EOF
chown root:matrix "$ENV_FILE"
chmod 0640 "$ENV_FILE"

# 4. systemd unit
install -m 0644 "$SCRIPT_DIR/chronosd.service" "$SERVICE_FILE"
systemctl daemon-reload

# 5. service (chronosd runs migrations against CHRONOS_POSTGRES_URI at boot)
systemctl enable chronosd.service
systemctl restart chronosd.service
sleep 1
systemctl --no-pager status chronosd.service || true

echo "chronosd installed; reachable at http://127.0.0.1:9096 (healthz: /healthz)"
echo "router side: set ROUTER_WAKE_TOKEN=<same wake token> in /etc/matrix/router.env"
echo "daemon side: set MATRIX_CHRONOS_URL + MATRIX_CHRONOS_TOKEN on each Machine"
echo "optional public surface: add deploy/chronos/nginx-snippet.conf + reload nginx."
