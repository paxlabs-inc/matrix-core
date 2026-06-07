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
