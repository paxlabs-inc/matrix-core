# RPC Server

**Source file:** `pkg/rpc/server.go`

The JSON-RPC dispatcher routes `tachyon_*` methods to the engine. It is used by both the HTTP API (`POST /rpc`) and the MCP server (`tools/call`).

---

## Design decisions

### Method-per-verb mapping

Each JSON-RPC method maps 1:1 to an engine verb:

| Method | Engine call | Request type |
|---|---|---|
| `tachyon_compile` | `eng.Compile` | `types.CompileRequest` |
| `tachyon_test` | `eng.Test` | `types.TestRequest` |
| `tachyon_simulate` | `eng.Simulate` | `types.SimulateRequest` |
| `tachyon_deploy` | `eng.Deploy` | `types.DeployRequest` |
| `tachyon_call` | `eng.Call` | `types.CallRequest` |
| `tachyon_chain_list` | `eng.ChainList` | (none) |
| `tachyon_chain_register` | `eng.ChainRegister` | `types.ChainRegisterRequest` |
| `tachyon_chain_use` | `eng.ChainUse` | `types.ChainUseRequest` |
| `tachyon_artifact_get` | `eng.ArtifactGet` | `types.ArtifactGetRequest` |
| `tachyon_registry_lookup` | `eng.RegistryLookup` | `types.RegistryLookupRequest` |
| `tachyon_health` | `eng.Health` | (none) |

### Envelope passthrough

The engine returns `types.Envelope[T]`. The JSON-RPC layer passes this through as the `result` field. This means JSON-RPC responses have the same shape as REST responses:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "ok": true,
    "data": { ... }
  }
}
```

### Standard JSON-RPC error codes

- `-32700` — Parse error (malformed JSON)
- `-32602` — Invalid params (type mismatch, missing field)
- `-32601` — Method not found

Engine errors are not JSON-RPC errors. They are returned in the envelope's `error` field with `ok: false`. This preserves the engine's structured error taxonomy across all transports.

### Context propagation

The JSON-RPC dispatcher accepts a `context.Context` and passes it to engine methods. This enables request cancellation and timeout propagation from the HTTP server or MCP caller.

---

## Modifying the RPC server

| What to change | Where |
|---|---|
| Add JSON-RPC method | `pkg/rpc/server.go` — add case in `Dispatch` |
| Change error codes | `pkg/rpc/server.go` — `rpcError` struct |
| Add batch support | `pkg/rpc/server.go` — parse array requests |
