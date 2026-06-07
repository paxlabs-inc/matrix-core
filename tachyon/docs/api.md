# tachyon-tools HTTP API

Base URL: `http://127.0.0.1:8645` (override with `TACHYON_API_ADDR`).

Every response uses the shared envelope:

```json
{ "ok": true, "data": { ... } }
{ "ok": false, "error": { "code": "...", "message": "...", "retry": false } }
```

## Endpoints

| Method | Path | Verb |
|--------|------|------|
| GET | `/healthz` | Health |
| POST | `/rpc` | JSON-RPC 2.0 (`tachyon_*` methods) |
| POST | `/v1/compile` | compile |
| POST | `/v1/test` | test |
| POST | `/v1/simulate` | simulate |
| POST | `/v1/deploy` | deploy |
| POST | `/v1/call` | call |
| GET | `/v1/chains` | chain.list |
| POST | `/v1/chains` | chain.register |
| POST | `/v1/chains/use` | chain.use |
| GET | `/v1/artifacts/{name}` | artifact.get |
| GET | `/v1/registry/deployments?key=&chain_id=` | registry.lookup |

## Error catalog

| Code | Retry | Meaning |
|------|-------|---------|
| `COMPILER_FORGE_FAILED` | yes | `forge build` subprocess failed |
| `COMPILER_SOLC_FAILED` | yes | solc version or compile error |
| `TEST_FORGE_FAILED` | yes | `forge test` subprocess failed |
| `TEST_ASSERTION_FAILED` | no | tests ran but assertions failed |
| `CHAIN_NOT_FOUND` | no | unknown `chain_id` or missing RPC |
| `CHAIN_RPC_FAILED` | yes | RPC dial or transport error |
| `SIMULATE_FAILED` | no | `eth_call` reverted or RPC error |
| `DEPLOY_FAILED` | varies | deployment planning or broadcast failed |
| `ARTIFACT_NOT_FOUND` | no | compile first or wrong `project_id` |
| `REGISTRY_NOT_FOUND` | no | no deployment for idempotency key |
| `WALLET_NOT_CONFIGURED` | no | no dev/Paxeer signer configured |
| `WALLET_POLICY_DENIED` | no | spend cap or allow-list rejected |
| `INVALID_REQUEST` | no | malformed JSON or missing fields |
| `INTERNAL_ERROR` | no | unexpected engine failure |

## Examples

### Compile

```bash
curl -s -X POST localhost:8645/v1/compile \
  -H 'Content-Type: application/json' \
  -d '{"targets":["Create2"]}' | jq .
```

### Test

```bash
curl -s -X POST localhost:8645/v1/test \
  -H 'Content-Type: application/json' \
  -d '{"match_path":"test/utils/Create2.t.sol"}' | jq .
```

### Simulate (inline RPC)

```bash
curl -s -X POST localhost:8645/v1/simulate \
  -H 'Content-Type: application/json' \
  -d '{"rpc_url":"https://public-mainnet.rpcpaxeer.online/evm","to":"0x0000000000000000000000000000000000000000","data":"0x"}' | jq .
```

### JSON-RPC

```bash
curl -s -X POST localhost:8645/rpc \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tachyon_chain_list","params":{}}' | jq .
```
