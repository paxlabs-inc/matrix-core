#!/usr/bin/env bash
set -euo pipefail

SRC=/root/matrix/development/skills
DST=/root/matrix/skills

mkdir -p "$DST"

# DROP set — off-wedge, vendor-coupled, or Matrix-replaced
DROP=(
  using-superpowers skill-comply
  kotlin-coroutines-flows kotlin-exposed-patterns kotlin-ktor-patterns kotlin-patterns kotlin-testing
  java-coding-standards jpa-patterns
  springboot-patterns springboot-security springboot-tdd springboot-verification
  swift-actor-persistence swift-concurrency-6-2 swift-protocol-di-testing swiftui-patterns foundation-models-on-device
  cpp-coding-standards cpp-testing patching-native-modules
  dotnet-patterns csharp-testing
  laravel-patterns laravel-security laravel-tdd laravel-verification laravel-plugin-discovery
  perl-patterns perl-security perl-testing
  dart-flutter-patterns flutter-dart-code-review
  compose-multiplatform-patterns android-clean-architecture
  embedded-systems cross-platform
  orderly-api-authentication orderly-deposit-withdraw orderly-onboarding orderly-one-dex
  orderly-positions-tpsl orderly-sdk-debugging orderly-sdk-dex-architecture
  orderly-sdk-install-dependency orderly-sdk-page-components orderly-sdk-react-hooks
  orderly-sdk-theming orderly-sdk-trading-workflows orderly-sdk-wallet-connection
  orderly-trading-orders orderly-ui-components orderly-websocket-streaming
  healthcare-cdss-patterns healthcare-emr-patterns healthcare-eval-harness
  healthcare-phi-compliance hipaa-compliance
  inventory-demand-planning logistics-exception-management returns-reverse-logistics
  carrier-relationship-management customs-trade-compliance production-scheduling
  quality-nonconformance energy-procurement visa-doc-translate
  nft-standards
  claude-api claude-devfleet configure-ecc nanoclaw-repl openclaw-persona-forge
  opensource-pipeline storage-basicmemory
  opennote-vault
)

# DEFER set — possibly useful, decide later. Not copied, just tracked.
DEFER=(
  executing-tasks-from-any-source
  postgres-patterns database-migrations
  defi-amm-security defi-protocol-templates
  customer-billing-ops finance-billing-ops lead-intelligence
  agent-sort ai-first-engineering ralphinho-rfc-pipeline enterprise-agent-ops
  liquid-glass-design frontend-slides slides gan-style-harness
  manim-video remotion-video-creation video-editing videodb fal-ai-media
  nutrient-document-processing
  jira-integration google-workspace-ops email-ops messages-ops unified-notifications-ops
  clickhouse-io connections-optimizer quant-analyst risk-manager social-graph-ranker
)

# ADAPT set — copied but flagged for MCL+cortex rewrite later
ADAPT=(
  search-first context-budget
  subagent-driven-development dispatching-parallel-agents council qa-discussion
  blockchain-developer llm-trading-agent-security security-bounty-hunter
  agent-harness-construction autonomous-agent-harness agent-payment-x402
  agentic-engineering continuous-agent-loop continuous-learning continuous-learning-v2
  ecc-tools-cost-audit token-budget-advisor gateguard hookify-rules
  skill-stocktake new-skill brand brand-voice strategic-compact
  payment-integration ck santa-method
)

in_arr() {
  local needle=$1; shift
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

declare -a PORTED_KEEP=()
declare -a PORTED_ADAPT=()
declare -a SKIPPED_DROP=()
declare -a SKIPPED_DEFER=()

for dir in "$SRC"/*/; do
  slug=$(basename "$dir")
  if in_arr "$slug" "${DROP[@]}"; then
    SKIPPED_DROP+=("$slug")
    continue
  fi
  if in_arr "$slug" "${DEFER[@]}"; then
    SKIPPED_DEFER+=("$slug")
    continue
  fi
  # copy entire dir (preserves examples/, tests/, sub-files)
  cp -r "$dir" "$DST/"
  if in_arr "$slug" "${ADAPT[@]}"; then
    PORTED_ADAPT+=("$slug")
  else
    PORTED_KEEP+=("$slug")
  fi
done

# Report
echo "=== PORT REPORT ==="
echo "KEEP ported:  ${#PORTED_KEEP[@]}"
echo "ADAPT ported: ${#PORTED_ADAPT[@]}"
echo "DROP skipped: ${#SKIPPED_DROP[@]}"
echo "DEFER skipped: ${#SKIPPED_DEFER[@]}"
echo "Total processed: $(( ${#PORTED_KEEP[@]} + ${#PORTED_ADAPT[@]} + ${#SKIPPED_DROP[@]} + ${#SKIPPED_DEFER[@]} ))"
echo ""
echo "=== sample KEEP ==="
printf '  %s\n' "${PORTED_KEEP[@]:0:8}"
echo "=== sample ADAPT ==="
printf '  %s\n' "${PORTED_ADAPT[@]:0:8}"

# Write port manifest JSON
cat > "$DST/PORT_MANIFEST.json" <<JSON
{
  "v": "matrix/port-manifest/0.1",
  "ported_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source": "$SRC",
  "counts": {
    "keep": ${#PORTED_KEEP[@]},
    "adapt": ${#PORTED_ADAPT[@]},
    "dropped": ${#SKIPPED_DROP[@]},
    "deferred": ${#SKIPPED_DEFER[@]}
  },
  "keep": $(printf '%s\n' "${PORTED_KEEP[@]}" | jq -R . | jq -s .),
  "adapt": $(printf '%s\n' "${PORTED_ADAPT[@]}" | jq -R . | jq -s .),
  "dropped": $(printf '%s\n' "${SKIPPED_DROP[@]}" | jq -R . | jq -s .),
  "deferred": $(printf '%s\n' "${SKIPPED_DEFER[@]}" | jq -R . | jq -s .)
}
JSON

echo ""
echo "PORT_MANIFEST.json written to $DST/"
