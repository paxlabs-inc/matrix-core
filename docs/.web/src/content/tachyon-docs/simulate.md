# Simulate

**Source file:** `internal/simulate/simulate.go`

The simulator performs `eth_call` dry runs without broadcasting. It is a first-class verb in the engine, reflecting the simulation-first design principle: agents dry-run before they commit.

---

## Design decisions

### No wallet required

Simulation is a read-only operation. No signer is needed. This means the simulator works even when the wallet is unconfigured, making it safe for exploration and debugging.

### Timeout bounded

Every simulation runs with a 30-second context timeout. This prevents runaway calls on slow or unresponsive RPCs. The timeout is hardcoded; a future enhancement could make it configurable per-chain.

### Revert capture

If `eth_call` reverts, the error is captured in `SimulateResponse.Revert` and the envelope is `ok: false` with `code: SIMULATE_FAILED`. The gas estimate is still returned (from a separate `eth_estimateGas` call) so the agent knows what the call would have cost.

### Optional debug trace

When `SimulateRequest.Trace` is true, the simulator runs `debug_traceCall` after the `eth_call`. The trace is returned as raw JSON (`any`) in `SimulateResponse.Trace`. If tracing fails (e.g., RPC doesn't support it), the error is silently ignored — the primary `eth_call` result is still returned.

### Chain resolution

The simulator uses the same chain resolution as deploy/call: `chain_id` → registry lookup, `rpc_url` → inline RPC, or active chain from registry. This ensures consistency across all chain-facing verbs.

---

## Request/response types

```go
type SimulateRequest struct {
    ChainID string `json:"chain_id,omitempty"`
    RPCURL  string `json:"rpc_url,omitempty"`
    From    string `json:"from,omitempty"`
    To      string `json:"to"`
    Data    string `json:"data,omitempty"`
    Value   string `json:"value,omitempty"`
    Block   string `json:"block,omitempty"`
    Trace   bool   `json:"trace,omitempty"`
}

type SimulateResponse struct {
    Result      string `json:"result,omitempty"`
    GasEstimate uint64 `json:"gas_estimate,omitempty"`
    Revert      string `json:"revert,omitempty"`
    Trace       any    `json:"trace,omitempty"`
}
```

---

## Simulation flow

```
SimulateRequest
    │
    ▼
Resolve chain profile (chain_id / rpc_url / active)
    │
    ▼
Dial RPC client
    │
    ▼
eth_call (30s timeout)
    │
    ├── Success → result hex
    │
    └── Revert → capture reason, return error envelope
    │
    ▼
eth_estimateGas (for gas estimate, even on revert)
    │
    ▼
Optional debug_traceCall (if Trace=true)
    │
    ▼
Return SimulateResponse
```

---

## Error codes

| Code | Retry | Meaning |
|---|---|---|
| `SIMULATE_FAILED` | no | `eth_call` reverted or RPC error |
| `CHAIN_NOT_FOUND` | no | Unknown chain_id |
| `CHAIN_RPC_FAILED` | yes | RPC dial or transport error |
| `INVALID_REQUEST` | no | Missing `to` address |

---

## Modifying the simulator

| What to change | Where |
|---|---|
| Add simulation state override | `internal/simulate/simulate.go` — pass state override to `eth_call` |
| Make timeout configurable | `internal/simulate/simulate.go` — add field to `SimulateRequest` |
| Add trace formatting | `internal/simulate/simulate.go` — parse trace into structured format |
| Add block override | `internal/simulate/simulate.go` — pass block number to `CallMessage` |
