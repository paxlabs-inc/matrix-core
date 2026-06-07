#!/usr/bin/env bash
# run_flow.sh — submit ONE prose intent to the LIVE Matrix daemon, wait for the
# async job to reach a terminal state, then dump the full signed transcript via
# /events/replay/<intent_id>.
#
# Auth: pass the bearer in $MATRIX_JWT (Supabase OAuth token). The daemon URL
# defaults to the public gateway; override with $MATRIX_DAEMON_URL.
#
# Usage:
#   MATRIX_JWT="<token>" tools/e2e/run_flow.sh \
#     -s matrix://skill/tachyon-engineer@0.1.0 \
#     -p "Build and deploy ..."            # prose inline
#   MATRIX_JWT="<token>" tools/e2e/run_flow.sh -s <skill> -f prompt.txt   # prose from file
#
# Output: prints intent_id, polls status, writes the SSE replay to
#   /tmp/matrix-flow-<intent_id>.sse  and echoes a compact event summary.

set -uo pipefail

URL="${MATRIX_DAEMON_URL:-https://matrix.paxeer.app}"
SKILL="matrix://skill/tachyon-engineer@0.1.0"
PROSE=""
PROSE_FILE=""
POLL_SEC=5
MAX_WAIT=600

while [[ $# -gt 0 ]]; do
  case "$1" in
    -s) SKILL="$2"; shift 2 ;;
    -p) PROSE="$2"; shift 2 ;;
    -f) PROSE_FILE="$2"; shift 2 ;;
    -u) URL="$2"; shift 2 ;;
    -t) MAX_WAIT="$2"; shift 2 ;;
    -h|--help) sed -n '1,20p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "${MATRIX_JWT:-}" ]]; then
  echo "[flow] MATRIX_JWT not set (export the bearer token)" >&2; exit 2
fi
if [[ -n "$PROSE_FILE" ]]; then PROSE="$(cat "$PROSE_FILE")"; fi
if [[ -z "$PROSE" ]]; then echo "[flow] no prose (-p or -f required)" >&2; exit 2; fi

AUTH=(-H "Authorization: Bearer $MATRIX_JWT")

# Build request body with jq so prose is safely JSON-encoded.
BODY="$(jq -nc --arg prose "$PROSE" --arg skill "$SKILL" '{prose:$prose, skill:$skill}')"

echo "[flow] URL:   $URL"
echo "[flow] skill: $SKILL"
echo "[flow] prose: ${PROSE:0:120}..."
echo "[flow] submitting POST /messages/async ..."

RESP="$(curl -s -m 30 -X POST "$URL/messages/async" "${AUTH[@]}" \
  -H 'Content-Type: application/json' -d "$BODY")"
INTENT_ID="$(printf '%s' "$RESP" | jq -r '.intent_id // empty')"

if [[ -z "$INTENT_ID" ]]; then
  echo "[flow] submit failed; raw response:" >&2
  printf '%s\n' "$RESP" >&2
  exit 1
fi
echo "[flow] intent_id: $INTENT_ID"

# ── poll for terminal status ──
deadline=$(( $(date +%s) + MAX_WAIT ))
status=""
while :; do
  J="$(curl -s -m 30 "$URL/messages/async/$INTENT_ID" "${AUTH[@]}")"
  status="$(printf '%s' "$J" | jq -r '.status // empty')"
  echo "[flow] $(date -u +%H:%M:%S) status=$status"
  case "$status" in
    completed|failed|cancelled) break ;;
  esac
  if [[ $(date +%s) -ge $deadline ]]; then echo "[flow] timed out after ${MAX_WAIT}s" >&2; break; fi
  sleep "$POLL_SEC"
done

# ── dump the signed transcript ──
SSE="/tmp/matrix-flow-${INTENT_ID}.sse"
curl -s -m 60 "$URL/events/replay/$INTENT_ID" "${AUTH[@]}" > "$SSE"
echo "[flow] replay written: $SSE ($(wc -l < "$SSE") lines)"

echo "[flow] ── tool results ──"
grep -o '"type":"plan.tool.result"[^}]*' "$SSE" 2>/dev/null | head -40 || true
echo "[flow] ── terminal ──"
grep -oE '"(reason|status)":"[^"]*"' "$SSE" | tail -6 || true
echo "[flow] final status: $status"
