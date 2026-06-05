# Agent manifests

JSON-on-disk descriptions of what tools a Matrix agent has access to.
Loaded by `executor/tool` at agent boot; the registry resolves every
`matrix://tool/...` URI emitted by a plan against this file.

## Files

| File | Purpose |
|---|---|
| `default.json` | Baseline agent for v1: filesystem + fetch + git over MCP. Starting point for fork-and-customise per agent. |

## Schema

See `executor/tool/manifest.go` for the canonical Go types
(`AgentManifest`, `ServerEntry`, `ToolEntry`, `NativeToolEntry`).

Locked decisions (matrix.kvx `executor_locked_design`):

| # | Decision |
|---|---|
| Q4  | Off-chain tools dispatch through Anthropic MCP — drop custom fs/shell/http |
| Q15 | Transports = `stdio` + `http` (streamable HTTP); SSE-only deferred |
| Q17 | Tool URI scheme: `matrix://tool/mcp/<server-alias>/<tool-name>@<version>` |
| Q18 | Server credentials via `$env:NAME` refs in `env` / `headers`; never journaled |
| Q19 | `native_tools` is the placeholder slot for chain tools (v1.1) |
| Q21 | `tools` field MUST exhaustively enumerate what the server advertises; manager rejects drift at boot |
| Q22 | `package_digest` MUST be sha256 of the published server package (`sha256:<64-hex>`) |

## Production checklist

The placeholder digests in `default.json` are zero-filled (
`sha256:0000…`) for **bootstrap testing only**. Before any production
or chain-anchoring deployment:

1. Install the MCP server packages locally with the version pinned in
   the manifest.
2. Compute the actual sha256 of the distribution package
   (e.g. `sha256sum $(npm pack --dry-run --json @modelcontextprotocol/server-filesystem@2024.11.1 | jq -r .[0].filename)`
   or the equivalent for `uvx`).
3. Replace the zero-filled `package_digest` with the real value.
4. Commit the manifest update; the `cortex_snapshot_hash` of any plan
   references the manifest indirectly through the tool URI version
   pin, so digest changes ARE auditable.

## Credential refs

Servers requiring credentials (e.g. github-mcp, postgres-mcp) embed
references in the `env` or `headers` lists:

```json
"env": ["GITHUB_TOKEN=$env:GITHUB_TOKEN"]
```

The executor resolves the `$env:` token from its own process
environment at spawn time. Literal credentials in the manifest file
are **forbidden** — the manifest is content-addressed and may be
shared across operators / journaled into cortex Event memories.
