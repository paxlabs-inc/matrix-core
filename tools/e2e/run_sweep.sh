#!/usr/bin/env bash
# run_sweep.sh — drive executor/cmd/mcl-e2e N times back-to-back to build
# a live-model test dataset.
#
# Each iteration creates an isolated artefact directory containing its
# TOPLEVEL.jsonl + per-sub-run transcripts + envelope journals + cortex
# snapshots. Iterations are serial (MCP subprocesses + LLM rate limits
# make parallelism counterproductive). A single iteration failure does
# NOT abort the sweep; the runner aggregates pass/fail at the end.
#
# Usage:
#   tools/e2e/run_sweep.sh [-n N] [-o OUTDIR] [-s SKILL] [-p PROSE] [-v VERB]
#                          [-t TIMEOUT_SEC] [--skip-together] [--skip-determinism]
#
# Defaults:
#   N=50, OUTDIR=/root/matrix/runs/sweep-<ts>, skill/prose/verb from mcl-e2e
#   per-iteration timeout=900s (15 min), full A+B+C sub-runs.
#
# Output layout:
#   <OUTDIR>/
#     sweep.log              one-line per-iteration status
#     sweep.config.json      runner configuration
#     iterations/iter-NN/    full mcl-e2e -root artefacts (one TS subdir inside)
#     summary.md             post-sweep aggregate (written by aggregate_sweep.py)
#     summary.csv            machine-readable per-iteration roll-up

set -uo pipefail

# ── defaults ──
N=50
OUTDIR=""
SKILL="/root/matrix/skills/writing-plans/SKILL.mtx"
PROSE="Build a concise launch checklist for Matrix v1 covering compiler, cortex, executor, and bridge readiness."
VERB="build"
TIMEOUT_SEC=900
SKIP_TOGETHER=""
SKIP_DETERMINISM=""
SEED=42
FIREWORKS_MODEL="accounts/fireworks/models/deepseek-v4-pro"
TOGETHER_MODEL="deepseek-ai/DeepSeek-V4-Pro"
E2E_BIN="/tmp/mcl-e2e"

# ── parse args ──
while [[ $# -gt 0 ]]; do
  case "$1" in
    -n) N="$2"; shift 2 ;;
    -o) OUTDIR="$2"; shift 2 ;;
    -s) SKILL="$2"; shift 2 ;;
    -p) PROSE="$2"; shift 2 ;;
    -v) VERB="$2"; shift 2 ;;
    -t) TIMEOUT_SEC="$2"; shift 2 ;;
    --skip-together) SKIP_TOGETHER="-skip-together"; shift ;;
    --skip-determinism) SKIP_DETERMINISM="-skip-determinism"; shift ;;
    --bin) E2E_BIN="$2"; shift 2 ;;
    --seed) SEED="$2"; shift 2 ;;
    -h|--help)
      sed -n '1,30p' "$0"
      exit 0 ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2 ;;
  esac
done

# ── load .env if present (FIREWORKS_API_KEY / TOGETHER_API_KEY) ──
if [[ -f /root/matrix/.env ]]; then
  # shellcheck disable=SC1091
  set -a; source /root/matrix/.env; set +a
fi

# ── preflight ──
if [[ ! -x "$E2E_BIN" ]]; then
  echo "[sweep] building mcl-e2e binary..."
  (cd /root/matrix/executor && go build -o "$E2E_BIN" ./cmd/mcl-e2e) || {
    echo "[sweep] build failed" >&2; exit 2
  }
fi
if [[ -z "${FIREWORKS_API_KEY:-}" ]]; then
  echo "[sweep] FIREWORKS_API_KEY not set" >&2
  exit 2
fi
if [[ -z "$SKIP_TOGETHER" && -z "${TOGETHER_API_KEY:-}" ]]; then
  echo "[sweep] TOGETHER_API_KEY not set (pass --skip-together or export it)" >&2
  exit 2
fi

# ── resolve OUTDIR ──
if [[ -z "$OUTDIR" ]]; then
  TS=$(date -u +%Y%m%d-%H%M%S)
  OUTDIR="/root/matrix/runs/sweep-${TS}"
fi
mkdir -p "$OUTDIR/iterations"

# ── persist config ──
cat > "$OUTDIR/sweep.config.json" <<EOF
{
  "n": $N,
  "skill": "$SKILL",
  "prose": $(printf '%s' "$PROSE" | jq -Rs . 2>/dev/null || printf '"%s"' "$PROSE"),
  "verb": "$VERB",
  "seed": $SEED,
  "fireworks_model": "$FIREWORKS_MODEL",
  "together_model": "$TOGETHER_MODEL",
  "skip_together": $([[ -n "$SKIP_TOGETHER" ]] && echo true || echo false),
  "skip_determinism": $([[ -n "$SKIP_DETERMINISM" ]] && echo true || echo false),
  "timeout_sec": $TIMEOUT_SEC,
  "started_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "host": "$(hostname)"
}
EOF

echo "[sweep] outdir:  $OUTDIR"
echo "[sweep] N:       $N"
echo "[sweep] timeout: ${TIMEOUT_SEC}s per iteration"
echo "[sweep] skill:   $SKILL"
echo "[sweep] prose:   ${PROSE:0:100}..."
echo "[sweep] together: $([[ -n "$SKIP_TOGETHER" ]] && echo SKIPPED || echo enabled)"
echo "[sweep] determ:   $([[ -n "$SKIP_DETERMINISM" ]] && echo SKIPPED || echo enabled)"
echo

# ── trap SIGINT so a Ctrl-C still leaves us with aggregate ──
INTERRUPTED=0
trap 'INTERRUPTED=1; echo "[sweep] interrupted, finishing current iter then aggregating..."' INT TERM

PASS=0
FAIL=0
TIMEOUT_COUNT=0
START_TS=$(date +%s)

LOG="$OUTDIR/sweep.log"
: > "$LOG"

# ── main loop ──
for ((i=1; i<=N; i++)); do
  if [[ $INTERRUPTED -eq 1 ]]; then
    echo "[sweep] aborting before iter $i due to SIGINT" | tee -a "$LOG"
    break
  fi
  ITER_NAME=$(printf "iter-%03d" "$i")
  ITER_DIR="$OUTDIR/iterations/$ITER_NAME"
  mkdir -p "$ITER_DIR"
  ITER_LOG="$ITER_DIR/stderr.log"
  ITER_START=$(date +%s)

  echo "[sweep] iter $i/$N starting at $(date -u +%H:%M:%S)" | tee -a "$LOG"

  # We let mcl-e2e write its own timestamped subdir inside ITER_DIR.
  set +e
  timeout --foreground "${TIMEOUT_SEC}s" \
    "$E2E_BIN" \
      -root    "$ITER_DIR" \
      -skill   "$SKILL" \
      -prose   "$PROSE" \
      -verb    "$VERB" \
      -seed    "$SEED" \
      -fireworks-model "$FIREWORKS_MODEL" \
      -together-model  "$TOGETHER_MODEL" \
      $SKIP_TOGETHER \
      $SKIP_DETERMINISM \
      > "$ITER_DIR/stdout.log" 2> "$ITER_LOG"
  RC=$?
  set -e

  ITER_END=$(date +%s)
  ITER_DUR=$((ITER_END - ITER_START))

  case "$RC" in
    0)
      PASS=$((PASS+1))
      STATUS="pass"
      ;;
    124)
      TIMEOUT_COUNT=$((TIMEOUT_COUNT+1))
      FAIL=$((FAIL+1))
      STATUS="timeout"
      ;;
    *)
      FAIL=$((FAIL+1))
      STATUS="fail(rc=$RC)"
      ;;
  esac

  echo "[sweep] iter $i/$N done status=$STATUS dur=${ITER_DUR}s" | tee -a "$LOG"
done

END_TS=$(date +%s)
TOTAL=$((END_TS - START_TS))

echo
echo "[sweep] ── finished ──"
echo "[sweep] pass:     $PASS"
echo "[sweep] fail:     $FAIL"
echo "[sweep] timeout:  $TIMEOUT_COUNT"
echo "[sweep] elapsed:  ${TOTAL}s"
echo "[sweep] outdir:   $OUTDIR"

# ── kick off aggregator if present ──
AGG="/root/matrix/tools/e2e/aggregate_sweep.py"
if [[ -x "$AGG" || -f "$AGG" ]]; then
  echo "[sweep] aggregating..."
  python3 "$AGG" "$OUTDIR" || echo "[sweep] aggregator returned non-zero (artefacts still intact)"
fi
