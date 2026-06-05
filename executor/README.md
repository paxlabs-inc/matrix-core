# matrix/executor — Plan walker, lifecycle, tool dispatch

The fourth top-level Go module after `matrix/cortex`, `matrix/mcl`, and
`matrix/bridge`. Owns everything downstream of `intent.accept`: lifecycle
state machine, plan-tree walker, MCP-backed tool dispatch, materiality
classification, and the closing `intent.attest` / `intent.fail` envelope.

## Scope (Sessions 21–24)

| Session | Surface | Status |
|---|---|---|
| 21 | `lifecycle/` state machine; depends on `MCL/ir/plan.go` + `MCL/envelope/` | **in progress** |
| 22 | `tool/` registry + Tool interface; `mcp/` JSON-RPC 2.0 client (stdio + streamable HTTP); default agent manifest with filesystem-mcp + fetch + git pinned; `cmd/mcl-tools` | pending |
| 23 | `runtime/` plan walker (depth-first + parallel + gate); `skill_loader/`; `materiality/` D9 classifier; `cmd/mcl-execute` end-to-end CLI | pending |
| 24 | Replay-invariant validation + salience-EMA closed loop + `journal/plan/00-roadmap.md` | pending |

## Design locks

See `matrix.kvx` § `executor_locked_design` (lands in v1.20 update after Session 21).

| # | Decision |
|---|---|
| Q1 | Fourth top-level Go module with `replace` directives to sibling working trees; mirrors `bridge/` posture |
| Q2 | `PlanTree` IR lives in `MCL/ir/plan.go` — shared by skill (producer) and executor (consumer + auditor) |
| Q3 | Envelope codec lives in `MCL/envelope/` — all 15 message kinds, ed25519 sign/verify, canonical CBOR for signing, JSON for on-disk storage |
| Q4 | Off-chain tools via Anthropic MCP — drop custom fs/shell/http, ship MCP client (stdio + streamable HTTP) + register filesystem-mcp + fetch-mcp + git-mcp in default agent manifest |
| Q5 | Two-layer sandbox: Matrix capability check + MCP server's own jail + subprocess rlimits |
| Q6 | Sub-dispatch v1: in-process under same agent only; cross-agent + CortexScope Merkle proof handoff deferred to v1.1 |
| Q7 | Replay determinism: cortex state only (already byte-deterministic per Phase 11). Tool outputs captured as cortex Fact memories at moment of execution; replay reads from cortex, never re-runs tools |
| Q8 | Intent IR + envelopes live at `journal/logs/<intent_id>/<seq>.envelope.json` (workspace-relative, cross-actor) |
| Q9 | Failure taxonomy: full `research/02-protocol.md` §13 set + SKILL.mtx §FAILURE_MODES |
| Q10 | Policy gates (`policy.gate` / `policy.gate.resolve`) in v1 (synchronous, no async/timeout) |
| Q11 | Materiality classification enforced live during plan walk; material mods halt execution until re-accept |
| Q12 | Integration tests against real Fireworks executor LLM in CI-skippable mode |
| Q13 | Executor model default: `DefaultExecutorModel()` from `MCL/llm/model.go` (DeepSeek-V4-Pro on Fireworks) |
| Q14 | Streaming progress: JSONL per-event to stdout/stderr; cortex Event memory per step; SSE deferred to v1.1 |
| Q15 | MCP transports v1: stdio + streamable HTTP; SSE legacy, skip |
| Q16 | MCP server lifecycle: per-agent persistent processes; spawn on agent boot; health-pinged; auto-reconnect; graceful drain |
| Q17 | Tool URI scheme: `matrix://tool/mcp/<local-server-alias>/<tool-name>@<server-version>` |
| Q18 | MCP server credentials via env-var refs in agent manifest; never journaled |
| Q19 | Native chain-tool framework slot kept; no chain tool ships in v1 |
| Q20 | MCP tool args journaled in full (modulo redacted env vars); results as Fact memory or filesystem pointer |
| Q21 | Static manifest pinning — executor verifies MCP `tools/list` matches manifest at startup |
| Q22 | MCP server version pinning via package digest (sha256); S4 hard rule |

## Module layout

```
executor/
  go.mod                    # module matrix/executor; replace cortex, mcl, bridge
  README.md                 # this file
  lifecycle/                # session 21
    state.go                # State enum + Transition + Allowed transitions
    machine.go              # Machine{actor, intent_id, current} + Apply
    *_test.go
  tool/                     # session 22
    tool.go                 # Tool interface + ToolCall + ToolResult
    registry.go             # Registry + NativeTool/MCPTool providers
    manifest.go             # ToolManifest schema (version, args, side-effects)
  mcp/                      # session 22
    jsonrpc.go              # JSON-RPC 2.0 codec
    client.go               # initialize / tools.list / tools.call / ping
    stdio.go                # stdio transport + subprocess lifecycle
    http.go                 # streamable HTTP transport
    manager.go              # per-agent server pool: spawn/health/reconnect
    mock_server.go          # test harness
  materiality/              # session 23
    classify.go             # D9 material vs non-material plan modifications
  runtime/                  # session 23
    walker.go               # PlanTree DFS walk with parallel branches + gates
    skill_loader.go         # matrix://skill/... → SKILL.mtx
    progress.go             # cortex Event memory per step + JSONL transcript
  cmd/
    mcl-execute/            # session 23: end-to-end CLI
    mcl-tools/              # session 22: tool registry inspection
```

## Dependencies on sibling modules

| Sibling | What we import | Why |
|---|---|---|
| `MCL/ir`        | `Intent`, `PlanTree`, canonical JSON helpers | Typed source of truth + content addressing |
| `MCL/envelope`  | All 15 message kinds + sign/verify         | Lifecycle messages on the wire |
| `MCL/llm`       | `APIClient` (executor model)               | In-skill prompts during plan walk |
| `cortex/`       | `Cortex`, `Attest`, `Context`              | Plan-walk citation + outcome attestation |
| `bridge/`       | `Adapter`                                  | Compile-time D13 reuse if executor also re-resolves at run-time |

## Intentional non-dependencies

- **No direct chain integration.** `tools/chain`, `tools/argus`, etc. are
  framework architectural slots; v1 ships no chain tools. Deferred per
  Andrew's 2026-05-24 scope reframe.
- **No web framework.** CLI-only at v1; UI/RPC layer is v1.1.

## Verification posture

Every Session ends with `go test -count=1 ./...` green across all 4
modules + `go vet ./...` clean across all 4. The replay invariant
(Phase 11) extends to executor: drop derived → rebuild → byte-identical
`OverallRoot`. Executor adds no new SMT namespace; only journals
cortex Event memories per step.
