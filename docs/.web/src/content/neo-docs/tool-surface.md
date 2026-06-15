# Tool Surface

Package `matrix/neo/internal/tools` is Neo's tool surface. It reuses the executor's MCP manager + tool registry so Neo's tools are byte-identical to the daemon's (fs, web_search, browser, git, shell, fetch, …), advertises each tool's real JSON schema to the model as a function, and dispatches calls.

Source files: `neo/internal/tools/tools.go`, `neo/internal/tools/surface.go`.

---

## Design decisions

**Execution-surface split.** Reversible actions stay "Natural" (Neo performs them directly, fully permissive). Actions that move or commit the user's on-chain funds — or need a wallet signature — "Escalate" across the wall into the MCL pipeline. Neo holds no signing key, so escalate-class tools are never advertised as directly-callable functions.

**Synthetic `core_execute` tool.** The only way to reach escalate-class actions is through the `core_execute` function, which delegates to the MCL pipeline over HTTP. The user approves any spend inline before it happens.

**Synthetic `memory_recall` tool.** Ambient page-faulting injects memory automatically each turn, but when the user asks "what do you remember?" the model needs a deliberate lookup it can actually perform. This tool searches the durable cortex store.

**Graceful degradation.** An MCP server that fails to start is recorded as a warning and skipped — Neo degrades rather than refusing to boot.

---

## Surface classification

```go
type Surface int

const (
    Natural  Surface = iota  // reversible, no wallet signature
    Escalate Surface = iota  // moves/commits funds or needs signature
)
```

Default escalate patterns (case-insensitive substring match on tool name):

```
send, transfer, swap, approve, deploy, settle, fund, mint, withdraw, stake, invoke, bridge
```

Heuristic + tunable via `Options.EscalatePatterns`. On-chain READS and dry-runs (compile/test/simulate/lookup/list) deliberately do NOT match.

---

## Manager

```go
type Manager struct {
    manifest   *tool.AgentManifest
    mcp        *mcp.Manager
    registry   *tool.Registry
    classifier *Classifier
    delegate   DelegateFunc    // core_execute bridge
    recall     RecallFunc      // memory_recall bridge
    byFunc     map[string]*boundTool
    order      []string        // natural tool names (advertised)
    escalated  []string        // escalate tool names (hidden)
    warnings   []string        // non-fatal spawn failures
}
```

### Spawn

```go
func Spawn(ctx context.Context, opts Options) (*Manager, error)
```

1. Load the agent manifest (`agents/default.json`)
2. Start every declared MCP server (with timeout, env resolution)
3. Build the tool registry from live MCP schemas
4. Classify each tool as Natural or Escalate
5. Bind function names

A server that fails to start is logged as a warning and skipped. The remaining tools are still available.

### Schemas

```go
func (m *Manager) Schemas() []llm.Tool
```

Returns the function schemas advertised to the model:
- Every Natural tool (sorted, deterministic order)
- The synthetic `core_execute` tool
- The synthetic `memory_recall` tool (when memory store is wired)

Escalate-class tools are NOT included — they are reachable only via `core_execute`.

### Dispatch

```go
func (m *Manager) Dispatch(ctx context.Context, funcName string, args map[string]interface{}) (string, bool, error)
```

Returns `(content, isError, err)`:
- `err != nil` → transport/invocation failure (feeds recovery ladder: retry/adapt)
- `isError=true, err=nil` → in-band failure the model should see and adapt to
- Both empty → the tool ran successfully

Special handling:
- `core_execute` → delegates to `DelegateFunc`
- `memory_recall` → delegates to `RecallFunc`
- Unknown name → `"unknown tool %q — it is not available"`, isError=true
- Escalate-class tool called directly → `"%q moves funds... use core_execute"`, isError=true

---

## Synthetic tools

### core_execute

```go
const CoreExecuteTool = "core_execute"
```

Schema:
```json
{
  "name": "core_execute",
  "description": "Delegate a rigorous or money-moving task to Matrix's secure execution pipeline...",
  "parameters": {
    "type": "object",
    "properties": {
      "intent": {
        "type": "string",
        "description": "A clear, self-contained description of the task..."
      }
    },
    "required": ["intent"]
  }
}
```

The `intent` argument is a prose description passed to the MCL daemon's async API. The daemon compiles it into an Intent IR, plans it, executes it, and returns the verifiable outcome.

### memory_recall

```go
const MemoryRecallTool = "memory_recall"
```

Schema:
```json
{
  "name": "memory_recall",
  "description": "Search your own durable memory (the cortex) for what you know...",
  "parameters": {
    "type": "object",
    "properties": {
      "query": {
        "type": "string",
        "description": "What to look for..."
      }
    }
  }
}
```

Returns a rendered digest of relevant memories + user profile. If the memory store is not wired, the tool is not advertised.

---

## Function naming

Tools are advertised as `alias__name` (e.g. `fs__read_file`, `paxeer-net__get_balance`). The `sanitizeFuncName` function coerces into the OpenAI function name charset (`^[A-Za-z0-9_-]{1,64}$`), replacing illegal chars with `_` and truncating to 64 chars.

---

## Modifying the tool surface

| What to change | Where |
|---|---|
| Escalate patterns | `tools/surface.go` — `DefaultEscalatePatterns` |
| core_execute schema | `tools/tools.go` — `coreExecuteSchema()` |
| memory_recall schema | `tools/tools.go` — `memoryRecallSchema()` |
| Function naming | `tools/tools.go` — `funcName()`, `sanitizeFuncName()` |
| Spawn timeout | `tools/tools.go` — `Options.SpawnTimeout` |
| Agent manifest path | `config/config.go` — `ManifestPath` |
