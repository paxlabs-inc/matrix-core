# Compiler

**Source files:** `internal/compiler/compiler.go`, `internal/forgeutil/run.go`

The compiler wraps `forge build` and normalizes Foundry's JSON artifact output into a stable, agent-friendly shape. It supports both in-place compiles (the daemon's own project root) and self-contained source uploads (ephemeral workdirs with baked dependencies).

---

## Design decisions

### Forge as the backend, not solc directly

The compiler shells out to `forge build` rather than invoking `solc` directly. This lets the engine leverage Foundry's dependency resolution, remappings, and optimizer settings without reimplementing them. The tradeoff is a subprocess dependency; `forge` must be on `PATH` or configured via `ForgePath`.

### Artifact normalization

Foundry emits one JSON file per contract under `out/<ContractName>/<ContractName>.json`. The compiler parses these into a normalized `types.Artifact`:

```go
type Artifact struct {
    Name             string            `json:"name"`
    Path             string            `json:"path,omitempty"`
    ABI              json.RawMessage   `json:"abi"`
    Bytecode         string            `json:"bytecode"`
    DeployedBytecode string            `json:"deployedBytecode,omitempty"`
    Compiler         *CompilerSettings `json:"compiler,omitempty"`
}
```

The `Compiler` field is extracted from the artifact metadata (solc version, optimizer enabled, runs). This lets agents know exactly what was built.

### Target filtering

`CompileRequest.Targets` is a list of contract names. When non-empty, only artifacts matching those names are collected. When empty, all artifacts are collected. This avoids returning the entire `out/` directory when the agent only needs one contract.

### Registry indexing

Every artifact is indexed in the registry under `projectID:name`. The registry key is `artifactKey(projectID, name) = projectID + ":" + name`. This flat key space is simple and collision-free across projects.

### Artifact mirroring

When `ArtifactsDir` is configured, artifacts are also written as individual JSON files to that directory (e.g., `artifacts/Create2.json`). This is a convenience for human inspection and external tooling.

---

## Compilation flow

```
CompileRequest
    ├── Sources non-empty? → prepareSourceWorkdir → ephemeral foundry project
    └── Sources empty? → use ProjectRoot (or engine default)

forge build --skip test
    └── subprocess with 15-minute timeout

parse out/ directory
    └── one JSON per contract → normalize → filter by Targets

index in registry
    └── ArtifactRecord{ProjectID, Name, Path, ABI, Bytecode, DeployedBytecode, Compiler, UpdatedAt}

return CompileResponse{ProjectID, Artifacts, Warnings}
```

---

## Error handling

Forge failures are classified into two codes:

- `COMPILER_FORGE_FAILED` — `forge build` subprocess failed (non-zero exit, timeout)
- `COMPILER_SOLC_FAILED` — solc version mismatch or compile error (detected by scanning stderr/stdout for "solc")

Both are retryable (`retry: true`) because they may be transient (RPC issues, file locks, solc download).

The error details include `stdout` and `stderr` for agent debugging.

---

## Project ID derivation

For in-place compiles, the project ID is the first 8 bytes of SHA-256 of the absolute project root path:

```go
func ProjectID(root string) string {
    sum := sha256.Sum256([]byte(root))
    return hex.EncodeToString(sum[:8])
}
```

For uploaded-source compiles, the project ID is derived from the sorted source set (see `engine.md` — `sourcesProjectID`).

---

## Forge subprocess runner

`forgeutil/run.go` provides:

```go
func Run(ctx context.Context, forgePath, root string, args ...string) (stdout, stderr string, err error)
func RunWithTimeout(forgePath, root string, timeout time.Duration, args ...string) (string, string, error)
func FormatForgeError(stdout, stderr string, err error) string
func EnsureForge(forgePath string) error
```

- `Run` appends `--root <root>` to the args (forge v1.7+ convention)
- `RunWithTimeout` wraps `Run` with a context timeout
- `FormatForgeError` prefers stderr, falls back to stdout, then to the error message
- `EnsureForge` probes `forge --version` as a health check

---

## Modifying the compiler

| What to change | Where |
|---|---|
| Add artifact field | `pkg/types/compile.go` — `Artifact` struct |
| Change forge args | `internal/compiler/compiler.go` — `Compile` method |
| Change timeout | `internal/compiler/compiler.go` — `RunWithTimeout` call |
| Add solc direct mode | New package `internal/solc/` — alternative to `Compiler` |
| Change artifact path | `internal/compiler/compiler.go` — `collectArtifacts` |
