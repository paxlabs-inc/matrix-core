# Engine

**Source files:** `internal/engine/engine.go`, `internal/engine/workdir.go`

The engine is the single implementation of all tachyon verbs. It is a flat struct that wires together the compiler, tester, simulator, deployer, wallet gate, chain manager, and registry. Every verb (compile, test, simulate, deploy, call, chain_list, chain_register, chain_use, artifact_get, registry_lookup) is a method on `Engine` that returns a typed `Envelope[T]`.

---

## Design decisions

### Flat verb dispatch, not a plugin system

The engine does not use reflection or a plugin registry to route verbs. Each verb is a concrete method with a concrete request and response type. This makes the API contract explicit, enables compile-time checking, and keeps the JSON-RPC / REST / MCP dispatch layers thin.

```go
type Engine struct {
    Cfg       config.Config
    Reg       *registry.Registry
    Chains    *chains.Manager
    Compiler  *compiler.Compiler
    Tester    *tester.Tester
    Simulator *simulate.Simulator
    Deployer  *deployer.Deployer
    Wallet    *wallet.Gate
}
```

### Envelope wrapping

Every verb returns `types.Envelope[T]` — a uniform JSON shape:

```json
{ "ok": true, "data": { ... } }
{ "ok": false, "error": { "code": "...", "message": "...", "retry": false } }
```

The envelope is constructed at the engine boundary, not in the transport layer. This means REST, JSON-RPC, and MCP all see the same shape. `types.OK(data)` and `types.Fail(err)` are the only constructors.

### Partial results on failure

Some verbs return partial data even when the overall operation fails:

- **Test:** If tests run but assertions fail, the envelope is `ok: false` but `data` contains the suite results with pass/fail counts.
- **Simulate:** If `eth_call` reverts, the envelope is `ok: false` but `data` contains the gas estimate and revert reason.

This lets agents inspect failure details without a second round-trip.

### Self-contained source uploads

When `CompileRequest.Sources` or `TestRequest.Sources` is non-empty, the engine materializes the source set into an ephemeral Foundry project:

1. Creates a temp directory
2. Writes uploaded files (with path sanitization via `safeRel`)
3. Symlinks the baked dependency tree (`lib/` → box lib, `.oz/` → box contracts)
4. Generates `foundry.toml` and `remappings.txt` if not provided
5. Derives a deterministic `ProjectID` from the source set (SHA-256 of sorted paths+contents)
6. Runs forge in the temp dir
7. Cleans up via deferred `cleanup()`

The deterministic `ProjectID` means a compile and a later deploy/call resolve the same registry entries without the caller threading a path-derived id.

### Conservative EVM version default

`defaultEVMVersion = "shanghai"` is used for uploaded-source compiles when the caller does not pin one. This is deliberate: Paxeer mainnet (chain 125) and many other EVM chains are pre-Cancun. solc 0.8.27's default `"cancun"` emits the `MCOPY` opcode (`0x5E`), which causes `"invalid opcode: opcode 0x5e not defined"` reverts on pre-Cancun chains. `"shanghai"` still emits `PUSH0` (which Paxeer supports) but no Cancun-only opcodes.

Callers can override per-chain via `CompileRequest.EVMVersion` (e.g., `"cancun"` for a chain that supports it, or `"paris"` for a PUSH0-less node).

---

## Verb implementations

### Compile

```go
func (e *Engine) Compile(ctx context.Context, req types.CompileRequest) types.Envelope[types.CompileResponse]
```

- If `req.Sources` is non-empty: materializes ephemeral workdir, derives `ProjectID`, runs forge there
- Otherwise: uses `req.ProjectRoot` (or engine default), runs forge in place
- Collects artifacts from `out/`, normalizes ABI/bytecode/compiler metadata
- Indexes artifacts in registry
- Returns `ProjectID` + artifact list

### Test

```go
func (e *Engine) Test(ctx context.Context, req types.TestRequest) types.Envelope[types.TestResponse]
```

- Same source-upload logic as Compile
- Runs `forge test --json`
- Parses NDJSON or single JSON output into `TestResponse` with suite/case granularity
- Returns partial results even when tests fail

### Simulate

```go
func (e *Engine) Simulate(ctx context.Context, req types.SimulateRequest) types.Envelope[types.SimulateResponse]
```

- Resolves chain profile (from `chain_id`, `rpc_url`, or active registry entry)
- Dials RPC, runs `eth_call` with 30s timeout
- If `req.Trace`: runs `debug_traceCall` when supported
- Returns result hex, gas estimate, revert reason, optional trace

### Deploy

```go
func (e *Engine) Deploy(ctx context.Context, req types.DeployRequest) types.Envelope[types.DeployResponse]
```

- Checks registry for existing deployment by `idempotency_key` + `chain_id`
- If found and confirmed on-chain: returns existing address (`Existing: true`)
- Resolves artifact from registry by `contract` + `ProjectID`
- Packs constructor args via `abienc.PackConstructorArgs`
- Builds `TxIntent` (plain creation or CREATE2 factory call)
- Authorizes via wallet policy gate
- Signs and broadcasts, waits for receipt
- Records deployment in registry

### Call

```go
func (e *Engine) Call(ctx context.Context, req types.CallRequest) types.Envelope[types.CallResponse]
```

- Resolves calldata: ABI-encoded from `Method` + `Args` (with ABI from inline or registry), or pre-encoded hex `Data`
- If `SimulateOnly`: runs `eth_call` via simulator, returns result/revert
- Otherwise: builds tx, authorizes policy, signs, broadcasts raw tx, returns tx hash

### Chain management

```go
func (e *Engine) ChainList() types.Envelope[types.ChainListResponse]
func (e *Engine) ChainRegister(req types.ChainRegisterRequest) types.Envelope[types.ChainProfile]
func (e *Engine) ChainUse(req types.ChainUseRequest) types.Envelope[types.ChainListResponse]
```

- `ChainList`: returns all presets + custom profiles, marks active
- `ChainRegister`: adds custom chain to in-memory map (not persisted)
- `ChainUse`: sets active chain in registry, returns updated list

### Registry lookups

```go
func (e *Engine) ArtifactGet(req types.ArtifactGetRequest) types.Envelope[types.ArtifactGetResponse]
func (e *Engine) RegistryLookup(req types.RegistryLookupRequest) types.Envelope[types.RegistryLookupResponse]
```

- `ArtifactGet`: fetches cached ABI/bytecode by contract name + project ID
- `RegistryLookup`: resolves prior deployment by idempotency key + chain

---

## Workdir helpers

`workdir.go` contains the ephemeral source workdir logic:

```go
func (e *Engine) prepareSourceWorkdir(sources map[string]string, evmVersion string) (string, func(), *types.Error)
func sourcesProjectID(sources map[string]string) string
func foundryTomlFor(evmVersion string) string
func safeRel(p string) (string, bool)
```

- `safeRel` rejects absolute paths, backslashes, and parent-directory escapes (`../`)
- `maxSourceBytes = 8 << 20` (8 MiB) bounds individual file size
- The dependency tree is linked via symlinks, not copied, so ephemeral workdirs are cheap

---

## Modifying the engine

| What to change | Where |
|---|---|
| Add a new verb | `internal/engine/engine.go` — add method + type in `pkg/types/` |
| Change envelope shape | `pkg/types/types.go` — `Envelope[T]` and `Error` |
| Add error code | `pkg/types/errors.go` — add constant + update docs |
| Change EVM default | `internal/engine/workdir.go` — `defaultEVMVersion` |
| Add source upload limit | `internal/engine/workdir.go` — `maxSourceBytes` |
| Change dependency linking | `internal/engine/workdir.go` — `prepareSourceWorkdir` symlink logic |
