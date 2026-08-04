#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

fail=0
report_matches() {
    local label="$1"
    shift
    local matches
    matches="$({ rg -n "$@" || true; })"
    if [[ -n "${matches}" ]]; then
        echo "${label}" >&2
        echo "${matches}" >&2
        fail=1
    fi
}

report_matches "Neo imports the retired memory module:" \
    '^\s*([[:alnum:]_]+\s+)?"matrix/cortex(/[^" ]*)?"' neo --glob '*.go'
report_matches "Neo module graph contains the retired memory module:" \
    '(^|[[:space:]])matrix/cortex([[:space:]]|$)|replace[[:space:]]+matrix/cortex[[:space:]]' neo/go.mod
report_matches "Neo contains a retired substrate selector or adapter:" \
    'MemorySubstrate|SubstrateCortex|SubstrateNeocortex|NewCortexAdapter|CortexToolJournal|NEO_MEMORY_SUBSTRATE' neo
report_matches "Neo deployment accepts retired memory configuration:" \
    'NEO_MEMORY_SUBSTRATE|NEO_CORTEX_ROOT|NEO_CORTEX_ACTOR|NEO_CORTEXD_(SOCKET|TOKEN)' deploy/railway/entrypoint.sh

if (( fail != 0 )); then
    exit 1
fi

echo "Neo Neocortex-only structural gate passed"
