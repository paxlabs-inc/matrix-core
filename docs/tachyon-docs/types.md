# Types

**Source files:** `pkg/types/types.go`, `pkg/types/compile.go`, `pkg/types/deploy.go`, `pkg/types/call.go`, `pkg/types/simulate.go`, `pkg/types/test.go`, `pkg/types/chain.go`, `pkg/types/artifact.go`, `pkg/types/errors.go`

The `pkg/types` package defines the shared request/response types used by all transport layers (REST, JSON-RPC, MCP). It is the API contract between the engine and the outside world.

---

## Design decisions

### Generic envelope

```go
type Envelope[T any] struct {
    Ok    bool   `json:"ok"`
    Data  T      `json:"data,omitempty"`
    Error *Error `json:"error,omitempty"`
}
```

The generic envelope is the only response shape. It is used by REST, JSON-RPC, and MCP. The `Data` field is omitted when empty; the `Error` field is omitted on success.

### Machine-stable error codes

```go
const (
    CodeCompilerForgeFailed = "COMPILER_FORGE_FAILED"
    CodeCompilerSolcFailed  = "COMPILER_SOLC_FAILED"
    CodeTestForgeFailed     = "TEST_FORGE_FAILED"
    CodeTestAssertionFailed = "TEST_ASSERTION_FAILED"
    CodeChainNotFound       = "CHAIN_NOT_FOUND"
    CodeChainRPCFailed      = "CHAIN_RPC_FAILED"
    CodeSimulateFailed      = "SIMULATE_FAILED"
    CodeDeployFailed        = "DEPLOY_FAILED"
    CodeDeployIdempotent    = "DEPLOY_IDEMPOTENT_HIT"
    CodeCallFailed          = "CALL_FAILED"
    CodeArtifactNotFound    = "ARTIFACT_NOT_FOUND"
    CodeRegistryNotFound    = "REGISTRY_NOT_FOUND"
    CodeWalletDenied        = "WALLET_POLICY_DENIED"
    CodeWalletNotConfigured = "WALLET_NOT_CONFIGURED"
    CodeInvalidRequest      = "INVALID_REQUEST"
    CodeInternal            = "INTERNAL_ERROR"
)
```

Every error carries a `retry` boolean. Agents use this for automatic retry logic without parsing human-readable messages.

### Constructor helpers

```go
func OK[T any](data T) Envelope[T]
func Fail[T any](err *Error) Envelope[T]
func NewError(code, message string, retry bool, details any) *Error
```

These are the only ways to construct envelopes and errors. They ensure consistency across all engine methods.

### Separate files per domain

Types are organized by domain:

| File | Types |
|---|---|
| `types.go` | `Envelope[T]`, `Error`, `HealthData` |
| `compile.go` | `CompileRequest`, `CompileResponse`, `Artifact`, `CompilerSettings` |
| `deploy.go` | `DeployRequest`, `DeployResponse`, `Create2Config` |
| `call.go` | `CallRequest`, `CallResponse` |
| `simulate.go` | `SimulateRequest`, `SimulateResponse` |
| `test.go` | `TestRequest`, `TestResponse`, `TestCaseResult`, `TestSuiteResult` |
| `chain.go` | `ChainProfile`, `ChainListResponse`, `ChainRegisterRequest`, `ChainUseRequest` |
| `artifact.go` | `ArtifactGetRequest`, `ArtifactGetResponse`, `RegistryLookupRequest`, `RegistryLookupResponse` |
| `errors.go` | Error codes, constructors |

---

## Modifying types

| What to change | Where |
|---|---|
| Add request/response field | Domain file (e.g., `deploy.go`) |
| Add error code | `errors.go` — add constant + update docs |
| Change envelope shape | `types.go` — update all transports |
| Add new domain | New file `pkg/types/<domain>.go` |
