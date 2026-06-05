#!/usr/bin/env bash
# gateway/deploy/install.sh — idempotent installer for matrix-gateway.
#
# Run as root on the box. Performs:
#   1. Create the matrix system user/group if missing.
#   2. Install the matrix-gateway binary into /opt/matrix-gateway.
#   3. Drop the systemd unit file at /etc/systemd/system/matrix-gateway.service.
#   4. Run the credit_ledger Postgres migration.
#   5. Enable + start the service.
#
# Usage:
#   ./install.sh \
#     --binary /path/to/matrix-gateway \
#     --postgres-uri "postgres://matrix@localhost/matrix?sslmode=disable" \
#     --gateway-token "<random-32-bytes-base64>"
#
# Re-running is safe: every step is idempotent. Fails fast on any
# error (set -euo pipefail). The script does NOT manage TLS — that's
# the responsibility of the box-level certbot --nginx flow already in
# place.

set -euo pipefail

BINARY=""
POSTGRES_URI=""
GATEWAY_TOKEN=""
FIREWORKS_KEY=""
TOGETHER_KEY=""
INSTALL_DIR="/opt/matrix-gateway"
ENV_FILE="/etc/matrix/gateway.env"
SERVICE_FILE="/etc/systemd/system/matrix-gateway.service"

usage() {
    cat <<EOF
Usage: $0 --binary PATH --postgres-uri URI --gateway-token TOKEN \\
       [--fireworks-key KEY] [--together-key KEY]

  --binary         Path to the compiled matrix-gateway binary.
  --postgres-uri   Postgres URI for credit_ledger.
  --gateway-token  Shared bearer secret (MATRIX_GATEWAY_TOKEN).
  --fireworks-key  Optional FIREWORKS_API_KEY (gateway's own upstream key).
  --together-key   Optional TOGETHER_API_KEY (gateway's own upstream key).
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --binary)        BINARY="$2"; shift 2 ;;
        --postgres-uri)  POSTGRES_URI="$2"; shift 2 ;;
        --gateway-token) GATEWAY_TOKEN="$2"; shift 2 ;;
        --fireworks-key) FIREWORKS_KEY="$2"; shift 2 ;;
        --together-key)  TOGETHER_KEY="$2"; shift 2 ;;
        -h|--help)       usage ;;
        *)               echo "unknown flag $1"; usage ;;
    esac
done

[[ -z "$BINARY" || -z "$POSTGRES_URI" || -z "$GATEWAY_TOKEN" ]] && usage
[[ "$EUID" -eq 0 ]] || { echo "must run as root"; exit 1; }
[[ -x "$BINARY" ]] || { echo "binary not executable: $BINARY"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATION_PRIMARY="$SCRIPT_DIR/../migrations/001_credit_ledger.sql"

# 1. user/group
if ! getent group matrix >/dev/null; then
    groupadd --system matrix
fi
if ! id -u matrix >/dev/null 2>&1; then
    useradd --system --gid matrix --home-dir "$INSTALL_DIR" \
        --shell /usr/sbin/nologin matrix
fi

# 2. binary
install -d -o matrix -g matrix -m 0755 "$INSTALL_DIR"
install -o matrix -g matrix -m 0755 "$BINARY" "$INSTALL_DIR/matrix-gateway"

# 3. env file
install -d -m 0750 /etc/matrix
chown root:matrix /etc/matrix
umask 0027
cat > "$ENV_FILE" <<EOF
# Matrix gateway environment — managed by gateway/deploy/install.sh
MATRIX_GATEWAY_TOKEN=$GATEWAY_TOKEN
MATRIX_GATEWAY_POSTGRES_URI=$POSTGRES_URI
EOF
[[ -n "$FIREWORKS_KEY" ]] && echo "FIREWORKS_API_KEY=$FIREWORKS_KEY" >> "$ENV_FILE"
[[ -n "$TOGETHER_KEY" ]]  && echo "TOGETHER_API_KEY=$TOGETHER_KEY"   >> "$ENV_FILE"
chown root:matrix "$ENV_FILE"
chmod 0640 "$ENV_FILE"

# 4. systemd unit
install -m 0644 "$SCRIPT_DIR/matrix-gateway.service" "$SERVICE_FILE"
systemctl daemon-reload

# 5. migration
if [[ -f "$MIGRATION_PRIMARY" ]]; then
    echo "running credit_ledger migration ($MIGRATION_PRIMARY)..."
    psql "$POSTGRES_URI" -v ON_ERROR_STOP=1 -f "$MIGRATION_PRIMARY"
else
    echo "warning: $MIGRATION_PRIMARY not found; skipping migration."
    echo "         apply gateway/migrations/001_credit_ledger.sql manually."
fi

# 6. service
systemctl enable matrix-gateway.service
systemctl restart matrix-gateway.service
sleep 1
systemctl --no-pager status matrix-gateway.service || true

echo "matrix-gateway installed; reachable at http://127.0.0.1:9090"
echo "remember to add the gateway/deploy/nginx-snippet.conf block to"
echo "/etc/nginx/sites-available/matrix.paxeer.app and reload nginx."
