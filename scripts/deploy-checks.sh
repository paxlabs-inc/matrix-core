#!/usr/bin/env bash
# ============================================================================
# Matrix Deploy Gates — deterministic check runner
# Location in repo: /root/matrix/scripts/deploy-checks.sh
#
# Semantics:
#   make build  → HARD GATE. Failure aborts immediately (exit 1).
#   make test / vet / lint / fmt → SOFT GATES. All four always run to
#     completion; failures never interrupt the sequence. Each failure log is
#     saved to /root/matrix/temp/deploy-fails/<timestamp>-<target>.log.
#   Exit 0 → all gates green, release phase may proceed.
#   Exit 2 → build passed but ≥1 soft gate failed; logs saved, release blocked.
#
# Machine-readable summary is written to:
#   /root/matrix/temp/deploy-fails/<timestamp>-summary.kv
# ============================================================================
set -uo pipefail

REPO="/root/matrix"
FAIL_DIR="$REPO/temp/deploy-fails"
TS="$(date +%Y%m%d-%H%M%S)"
SUMMARY="$FAIL_DIR/$TS-summary.kv"

mkdir -p "$FAIL_DIR"
cd "$REPO" || { echo "FATAL: cannot cd $REPO"; exit 1; }

echo "run.ts=$TS"                          | tee    "$SUMMARY"
echo "run.branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)" | tee -a "$SUMMARY"
echo "run.head=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"        | tee -a "$SUMMARY"

# ---------------------------------------------------------------- hard gate
BUILD_LOG="$FAIL_DIR/$TS-build.log"
echo "── make build (hard gate)"
if make build >"$BUILD_LOG" 2>&1; then
    echo "gate.build=pass" | tee -a "$SUMMARY"
    rm -f "$BUILD_LOG"
else
    echo "gate.build=fail"          | tee -a "$SUMMARY"
    echo "gate.build.log=$BUILD_LOG" | tee -a "$SUMMARY"
    echo "BUILD FAILED — aborting deploy flow. Log: $BUILD_LOG"
    tail -n 40 "$BUILD_LOG"
    exit 1
fi

# ---------------------------------------------------------------- soft gates
FAILED=()
for TARGET in test vet lint fmt; do
    LOG="$FAIL_DIR/$TS-$TARGET.log"
    echo "── make $TARGET (soft gate)"
    if make "$TARGET" >"$LOG" 2>&1; then
        echo "gate.$TARGET=pass" | tee -a "$SUMMARY"
        rm -f "$LOG"
    else
        echo "gate.$TARGET=fail"      | tee -a "$SUMMARY"
        echo "gate.$TARGET.log=$LOG"  | tee -a "$SUMMARY"
        FAILED+=("$TARGET")
        # continue — soft failures never stop the sequence
    fi
done

# fmt may rewrite files; surface that so the agent stages/reviews the diff
if ! git diff --quiet; then
    echo "fmt.dirty=true" | tee -a "$SUMMARY"
else
    echo "fmt.dirty=false" | tee -a "$SUMMARY"
fi

# ---------------------------------------------------------------- verdict
if [ "${#FAILED[@]}" -eq 0 ]; then
    echo "verdict=green" | tee -a "$SUMMARY"
    echo "ALL GATES GREEN — release phase may proceed."
    exit 0
else
    echo "verdict=soft-fail"                    | tee -a "$SUMMARY"
    echo "verdict.failed=${FAILED[*]}"          | tee -a "$SUMMARY"
    echo "SOFT GATE FAILURES: ${FAILED[*]} — logs in $FAIL_DIR (prefix $TS-)."
    echo "Release phase blocked until green (or explicitly forced)."
    exit 2
fi
