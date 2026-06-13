# tachyon-tools Developer Docs

**Source:** `github.com/paxlabs-inc/matrix-core/tachyon`  
**Language:** Go 1.22+ (daemon), Solidity (contracts corpus), JavaScript (test suite)  
**One-sentence contract:** An agent-native Solidity / EVM toolbox that exposes compile, test, simulate, deploy, and call verbs over HTTP API, JSON-RPC, and MCP stdio — with structured I/O, simulation-first safety, idempotent deploys, and policy-gated signing.

---

## Contents

| Doc | Subsystem | Source files |
|---|---|---|
| [index.md](index.md) | This file | — |
| [engine.md](engine.md) | Core engine — verb dispatch, envelope types, error taxonomy | `internal/engine/engine.go`, `internal/engine/workdir.go` |
| [compiler.md](compiler.md) | Solidity compiler — forge build, artifact normalization, ephemeral source workdirs | `internal/compiler/compiler.go`, `internal/forgeutil/run.go` |
| [deployer.md](deployer.md) | Intent-based deployment — idempotency, CREATE2, on-chain confirmation, registry write-back | `internal/deployer/deployer.go` |
| [wallet.md](wallet.md) | Signing backends — self-hosted (raw/keystore/env) and embedded (Paxeer DID lane) with policy gate | `internal/wallet/wallet.go`, `internal/wallet/embedded.go` |
| [evm-client.md](evm-client.md) | go-ethereum RPC wrapper — tx building, gas estimation, EIP-1559, raw broadcast, receipt polling | `internal/evm/client.go`, `internal/evm/evm.go` |
| [simulate.md](simulate.md) | Dry-run simulation — eth_call + optional debug trace, revert capture | `internal/simulate/simulate.go` |
| [tester.md](tester.md) | Forge test runner — JSON parsing, suite aggregation, partial failure handling | `internal/tester/tester.go` |
| [abi-encoder.md](abi-encoder.md) | ABI encoding bridge — JSON-to-Go type coercion for method calls and constructors | `internal/abienc/abienc.go` |
| [registry.md](registry.md) | Artifact + deployment index — JSON-file backed, mutex-protected, atomic saves | `internal/registry/registry.go` |
| [chains.md](chains.md) | Chain profile manager — presets, custom registration, numeric chain-id fallback, inline RPC | `internal/chains/chains.go` |
| [config.md](config.md) | Runtime configuration — `.kvx` format, env precedence, wallet mode selection, policy profiles | `internal/config/config.go`, `internal/config/kvx.go` |
| [api-server.md](api-server.md) | HTTP API — REST routes, JSON-RPC passthrough, Bearer auth middleware, structured logging | `pkg/api/server.go` |
| [mcp-server.md](mcp-server.md) | MCP stdio server — NDJSON-RPC, tool registry, selftest, error formatting | `pkg/mcp/server.go`, `pkg/mcp/tools.go` |
| [rpc-server.md](rpc-server.md) | JSON-RPC dispatcher — `tachyon_*` method routing, envelope wrapping | `pkg/rpc/server.go` |
| [types.md](types.md) | Shared request/response types — envelopes, errors, chain profiles, artifacts | `pkg/types/*.go` |
| [daemon.md](daemon.md) | Entry point — flag parsing, engine wiring, signal handling, MCP vs HTTP mode | `cmd/tachyond/main.go` |

---

## Repo layout

```
tachyon/
├── cmd/
│   ├── tachyon/          # thin CLI client
│   └── tachyond/         # daemon entry point (main.go)
├── pkg/
│   ├── api/              # HTTP REST server
│   ├── mcp/              # MCP stdio server + tool registry
│   ├── rpc/              # JSON-RPC dispatcher
│   └── types/            # shared request/response types
├── internal/
│   ├── abienc/           # ABI encoding bridge
│   ├── chains/           # chain profile manager
│   ├── compiler/         # forge build + artifact normalization
│   ├── config/           # .kvx config parser + loader
│   ├── deployer/         # intent-based deployment
│   ├── engine/           # core verb dispatch + workdir helpers
│   ├── evm/              # go-ethereum RPC wrapper
│   ├── forgeutil/        # forge subprocess runner
│   ├── registry/         # artifact + deployment index
│   ├── simulate/         # eth_call dry-run
│   ├── tester/           # forge test runner
│   └── wallet/           # signing backends + policy gate
├── contracts/            # OpenZeppelin contracts corpus (baked dependency)
├── chains/               # chain presets (presets.json)
├── test/                 # Hardhat/Foundry test suite
└── docs/                 # API docs, architecture docs
```

---

## Key locked decisions

1. **Agent-native, not human-native.** The CLI is a thin client; the product is the API/MCP/JSON-RPC surfaces. Every response is machine-parseable JSON with stable error codes.
2. **Simulation-first.** `simulate` is a first-class verb. Agents dry-run before they broadcast. Revert data is captured and returned structurally.
3. **Idempotent by key.** Deploys carry an `idempotency_key` (+ optional CREATE2 salt) so retries don't double-deploy. The registry tracks confirmed deployments.
4. **Keys behind policy.** Agents hold scoped capability tokens, never raw keys. Every signature passes a policy gate (spend cap, allow-list, chain allow-list).
5. **Self-contained source uploads.** A non-empty `sources` map on compile/test makes the request self-contained: the engine materializes an ephemeral Foundry project with the baked dependency tree linked in.
6. **Conservative EVM version.** Uploaded-source compiles default to `shanghai` (not `cancun`) so artifacts don't emit MCOPY, which pre-Cancun chains reject.
7. **Structured errors everywhere.** Every failure carries a machine-stable `code`, human `message`, and boolean `retry` flag. Partial results (e.g., test failures with pass counts) are returned in the envelope `data` field even when `ok: false`.
