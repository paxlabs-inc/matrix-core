# MCP Server

**Source files:** `pkg/mcp/server.go`, `pkg/mcp/tools.go`

The MCP (Model Context Protocol) server exposes tachyon verbs as MCP tools over stdio newline-delimited JSON-RPC. This is the primary transport for LLM agent integration when the daemon runs as a subprocess.

---

## Design decisions

### Stdio NDJSON-RPC

MCP uses stdin/stdout with newline-delimited JSON-RPC 2.0 messages. The server reads one line at a time, parses the request, dispatches to the engine, and writes the response. stdout is flushed after every message so clients block until the response is ready.

```go
scanner := bufio.NewScanner(os.Stdin)
scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
for scanner.Scan() {
    line := scanner.Text()
    resp := handle(eng, req.Method, req.Params, req.ID)
    send(resp)  // marshal + write + sync
}
```

### Log redirection

In MCP mode, logs are written to stderr so they don't corrupt the NDJSON-RPC stream on stdout. The daemon detects MCP mode via the `--mcp` flag and reconfigures the slog handler accordingly.

### Tool registry

The MCP tool list is a static registry in `tools.go`:

```go
var ToolNames = []string{
    "tachyon_compile",
    "tachyon_test",
    "tachyon_simulate",
    "tachyon_deploy",
    "tachyon_call",
    "tachyon_chain_list",
    "tachyon_chain_register",
    "tachyon_artifact_get",
    "tachyon_registry_lookup",
}
```

Each tool has a name, description, and input schema. The schema is permissive (`additionalProperties: true`) because the actual validation happens in the engine.

### Selftest

The `--selftest` flag verifies that the tool registry matches the canonical list:

```go
func Selftest() error {
    tools := Tools()
    if len(tools) != len(ToolNames) { ... }
    // verify every ToolNames entry exists in Tools()
}
```

This is used in CI to catch tool drift (adding a tool to the engine but not to the MCP registry).

### Error formatting

MCP tool errors return `isError: true` with JSON text content:

```json
{
  "content": [{"type": "text", "text": "{\"ok\":false,\"tool\":\"tachyon_deploy\",\"error\":{...}}"}],
  "isError": true
}
```

The `FormatToolError` helper builds this structure from a `types.Error`.

### Method dispatch

| MCP Method | Handler |
|---|---|
| `initialize` | Return protocol version, server info, capabilities |
| `tools/list` | Return tool descriptors |
| `tools/call` | Parse tool name + arguments, dispatch to engine |
| `notifications/initialized` | No-op (ack) |
| `ping` | No-op (ack) |

The `tools/call` handler routes to the engine via the JSON-RPC dispatcher (`pkg/rpc`).

---

## Modifying the MCP server

| What to change | Where |
|---|---|
| Add MCP tool | `pkg/mcp/tools.go` — add to `ToolNames` and `Tools()` |
| Change protocol version | `pkg/mcp/server.go` — `initialize` handler |
| Add MCP capability | `pkg/mcp/server.go` — `initialize` capabilities map |
| Change error format | `pkg/mcp/server.go` — `FormatToolError` |
