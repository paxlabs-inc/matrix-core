# MCP tools (v1)

Run stdio MCP: `./bin/tachyond --mcp`

Selftest (CI): `./bin/tachyond --selftest`

## Tool list

| Tool | REST equivalent | Description |
|------|-----------------|-------------|
| `tachyon_compile` | POST `/v1/compile` | Build contracts via forge |
| `tachyon_test` | POST `/v1/test` | Run forge tests |
| `tachyon_simulate` | POST `/v1/simulate` | eth_call dry run |
| `tachyon_deploy` | POST `/v1/deploy` | Idempotent deploy |
| `tachyon_call` | POST `/v1/call` | Contract call |
| `tachyon_chain_list` | GET `/v1/chains` | List chain profiles |
| `tachyon_chain_register` | POST `/v1/chains` | Register custom chain |
| `tachyon_artifact_get` | GET `/v1/artifacts/{name}` | Fetch cached artifact |
| `tachyon_registry_lookup` | GET `/v1/registry/deployments` | Lookup deployment |

Tool errors return MCP results with `isError: true` and JSON text:

```json
{"ok":false,"tool":"tachyon_deploy","error":{"code":"WALLET_NOT_CONFIGURED",...}}
```

## Example: compile

```json
{
  "name": "tachyon_compile",
  "arguments": {
    "targets": ["Create2"]
  }
}
```

## Example: test

```json
{
  "name": "tachyon_test",
  "arguments": {
    "match_path": "test/utils/Create2.t.sol"
  }
}
```

## Example: call

Calldata is resolved one of two ways:

- **`method` + `args`** — ABI-encoded by the engine. Provide the ABI inline via
  `abi`, or resolve it from a compiled artifact via `contract` (and optional
  `project_id`). `args` is a JSON array; integers may be decimal or `0x` hex
  strings (use strings for values beyond 2^53), addresses are hex strings, and
  tuples accept either an ordered array or an object keyed by field name.
- **`data`** — a pre-encoded hex calldata string (used when `method` is empty).

Set `simulate_only: true` for a read-only `eth_call`; omit it to broadcast a
signed transaction (requires a configured wallet).

```json
{
  "name": "tachyon_call",
  "arguments": {
    "chain_id": "paxeer",
    "to": "0xToken...",
    "contract": "BridgeERC20",
    "method": "transfer",
    "args": ["0xRecipient...", "1000000000000000000"]
  }
}
```
