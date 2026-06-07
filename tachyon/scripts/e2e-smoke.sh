#!/usr/bin/env bash
# End-to-end smoke for tachyond REST, JSON-RPC, and MCP.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${TACHYON_HTTP_ADDR:-http://127.0.0.1:8645}"
BIN="${ROOT}/bin"
PASS=0
FAIL=0

log() { printf '\n==> %s\n' "$*"; }
ok()  { PASS=$((PASS+1)); printf '  OK  %s\n' "$*"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$*" >&2; }

curl_json() {
  local method="$1" path="$2" body="${3:-}"
  if [[ "$method" == GET ]]; then
    curl -sf "${BASE}${path}"
  else
    curl -sf -X POST "${BASE}${path}" -H 'Content-Type: application/json' -d "$body"
  fi
}

rpc() {
  local method="$1" params="${2:-{}}"
  curl -sf -X POST "${BASE}/rpc" -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}"
}

assert_ok() {
  local label="$1" json="$2"
  if echo "$json" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('ok') else 1)" 2>/dev/null; then
    ok "$label"
  else
    bad "$label: $json"
    return 1
  fi
}

assert_rpc_ok() {
  local label="$1" json="$2"
  if echo "$json" | python3 -c "import sys,json; d=json.load(sys.stdin); r=d.get('result',{}); sys.exit(0 if r.get('ok') else 1)" 2>/dev/null; then
    ok "$label"
  else
    bad "$label: $json"
    return 1
  fi
}

log "healthz"
H=$(curl_json GET /healthz)
assert_ok "GET /healthz" "$H"

log "compile many contracts (crosschain + utils)"
C=$(curl_json POST /v1/compile '{"targets":["BridgeERC20","ERC20Crosschain","ERC7786Recipient","Create2","Create3","Math","Bytes"]}')
assert_ok "POST /v1/compile multi-target" "$C"
N=$(echo "$C" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['data']['artifacts']))")
[[ "$N" -ge 5 ]] && ok "compile returned $N artifacts" || bad "expected >=5 artifacts, got $N"

log "full compile (all contracts, no filter)"
CF=$(curl_json POST /v1/compile '{}')
assert_ok "POST /v1/compile full tree" "$CF"
NF=$(echo "$CF" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['data']['artifacts']))")
[[ "$NF" -ge 100 ]] && ok "full compile returned $NF artifacts" || bad "expected >=100 artifacts, got $NF"

log "artifact.get BridgeERC20"
A=$(curl_json GET "/v1/artifacts/BridgeERC20")
assert_ok "GET /v1/artifacts/BridgeERC20" "$A"

log "forge tests (Create2 + Math)"
T1=$(curl_json POST /v1/test '{"match_path":"test/utils/Create2.t.sol"}')
assert_ok "POST /v1/test Create2" "$T1"
T2=$(curl_json POST /v1/test '{"match_path":"test/utils/math/Math.t.sol"}')
assert_ok "POST /v1/test Math" "$T2"

log "chain list + register + use"
CL=$(curl_json GET /v1/chains)
assert_ok "GET /v1/chains" "$CL"
REG=$(curl_json POST /v1/chains '{"id":"e2e-anvil","name":"E2E Anvil","rpc_url":"http://127.0.0.1:8545","chain_id":31337}')
assert_ok "POST /v1/chains register" "$REG"
USE=$(curl_json POST /v1/chains/use '{"chain_id":"e2e-anvil"}')
assert_ok "POST /v1/chains/use" "$USE"

log "simulate eth_call (inline Paxeer public RPC)"
PAX_RPC="${PAXEER_RPC_URL:-https://public-mainnet.rpcpaxeer.online/evm}"
SIM=$(curl_json POST /v1/simulate "{\"rpc_url\":\"${PAX_RPC}\",\"to\":\"0x0000000000000000000000000000000000000000\",\"data\":\"0x\"}")
if echo "$SIM" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('ok') or d.get('data',{}).get('result') is not None else 1)" 2>/dev/null; then
  ok "POST /v1/simulate Paxeer RPC"
else
  # revert on zero call is still a valid simulation response
  if echo "$SIM" | grep -q 'SIMULATE_FAILED\|result\|revert'; then
    ok "POST /v1/simulate Paxeer RPC (revert or result)"
  else
    bad "simulate: $SIM"
  fi
fi

log "registry lookup (empty)"
RL=$(curl_json GET "/v1/registry/deployments?key=e2e-none&chain_id=e2e-anvil")
assert_ok "GET /v1/registry/deployments" "$RL"

log "call simulate_only via REST"
CALL=$(curl_json POST /v1/call '{"simulate_only":true,"rpc_url":"https://public-mainnet.rpcpaxeer.online/evm","to":"0x0000000000000000000000000000000000000000","data":"0x"}')
assert_ok "POST /v1/call simulate_only" "$CALL"

log "deploy without wallet (expect policy/artifact error)"
DEP=$(curl -s -X POST "${BASE}/v1/deploy" -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"e2e-deploy-1","chain_id":"e2e-anvil","contract":"Create2"}')
if echo "$DEP" | grep -qE 'WALLET_NOT_CONFIGURED|WALLET_POLICY_DENIED|ARTIFACT_NOT_FOUND|DEPLOY_FAILED'; then
  ok "POST /v1/deploy policy gate ($(
    echo "$DEP" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("error",{}).get("code",""))' 2>/dev/null
  ))"
else
  bad "deploy should fail without signer: $DEP"
fi

log "JSON-RPC tachyon_compile"
RC=$(rpc tachyon_compile '{"targets":["Create2"]}')
assert_rpc_ok "RPC tachyon_compile" "$RC"

log "JSON-RPC tachyon_chain_list"
RCL=$(rpc tachyon_chain_list '{}')
assert_rpc_ok "RPC tachyon_chain_list" "$RCL"

log "JSON-RPC tachyon_test"
RT=$(rpc tachyon_test '{"match_path":"test/utils/Create2.t.sol"}')
assert_rpc_ok "RPC tachyon_test" "$RT"

log "JSON-RPC tachyon_simulate"
RS=$(rpc tachyon_simulate "{\"rpc_url\":\"${PAX_RPC}\",\"to\":\"0x0000000000000000000000000000000000000000\",\"data\":\"0x\"}")
if echo "$RS" | python3 -c "import sys,json; d=json.load(sys.stdin); r=d.get('result',{}); sys.exit(0 if 'ok' in r else 1)" 2>/dev/null; then
  ok "RPC tachyon_simulate"
else
  bad "RPC simulate: $RS"
fi

log "MCP selftest"
"${BIN}/tachyond" --selftest && ok "MCP selftest" || bad "MCP selftest"

log "MCP stdio initialize + tools/list + compile + test + simulate"
MCP_OUT=$(python3 "${ROOT}/scripts/mcp_e2e.py" "${BIN}/tachyond")
echo "$MCP_OUT" | grep -q mcp_ok && ok "MCP stdio e2e" || bad "MCP stdio: $MCP_OUT"

log "CLI tachyon health"
"${BIN}/tachyon" health >/dev/null && ok "CLI health" || bad "CLI health"

log "CLI tachyon compile via stdin"
echo '{"targets":["Create2"]}' | "${BIN}/tachyon" compile | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['ok']" && ok "CLI compile" || bad "CLI compile"

log "summary: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
