# Registry

**Source file:** `internal/registry/registry.go`

The registry is a JSON-file-backed index of compiled artifacts and on-chain deployments. It provides durable, cross-session storage so that a compile today can be referenced by a deploy tomorrow, and an idempotent deploy yesterday can be looked up today.

---

## Design decisions

### JSON file store, not a database

The registry uses a single JSON file (`registry.json` by default) with a simple in-memory map + mutex pattern. This avoids database dependencies, keeps the daemon self-contained, and makes the registry human-readable and git-friendly. The tradeoff is no concurrent access across multiple daemon instances.

### Atomic writes

Every mutating operation (`PutArtifact`, `PutDeployment`, `SetActiveChain`) acquires a write lock, marshals the entire store to JSON, and writes it to disk. The read lock is held only during marshaling. This is simple and correct for a single-process daemon.

### Key schemes

Artifacts: `projectID:name` → `ArtifactRecord`
Deployments: `idempotencyKey:chainID` → `DeploymentRecord`

These flat key spaces are collision-free and easy to debug.

### Types conversion

The registry stores raw JSON for compiler metadata (`json.RawMessage`) and provides `ToTypesArtifact` to convert to the public `types.Artifact` shape. This decouples the storage schema from the API schema.

---

## Data structures

```go
type ArtifactRecord struct {
    ProjectID        string          `json:"project_id"`
    Name             string          `json:"name"`
    Path             string          `json:"path,omitempty"`
    ABI              json.RawMessage `json:"abi"`
    Bytecode         string          `json:"bytecode"`
    DeployedBytecode string          `json:"deployedBytecode,omitempty"`
    Compiler         json.RawMessage `json:"compiler,omitempty"`
    UpdatedAt        string          `json:"updated_at,omitempty"`
}

type DeploymentRecord struct {
    IdempotencyKey string `json:"idempotency_key"`
    ChainID        string `json:"chain_id"`
    Contract       string `json:"contract"`
    Address        string `json:"address"`
    TxHash         string `json:"tx_hash,omitempty"`
    Confirmed      bool   `json:"confirmed"`
    ProjectID      string `json:"project_id,omitempty"`
}
```

---

## Registry operations

```go
func Open(path string) (*Registry, error)
func (r *Registry) PutArtifact(rec ArtifactRecord) error
func (r *Registry) GetArtifact(projectID, name string) (ArtifactRecord, bool)
func (r *Registry) ListArtifacts(projectID string) []ArtifactRecord
func (r *Registry) PutDeployment(rec DeploymentRecord) error
func (r *Registry) GetDeployment(idempotencyKey, chainID string) (DeploymentRecord, bool)
func (r *Registry) ActiveChainID() string
func (r *Registry) SetActiveChain(chainID string) error
```

---

## Modifying the registry

| What to change | Where |
|---|---|
| Add index field | `internal/registry/registry.go` — add to `ArtifactRecord`/`DeploymentRecord` |
| Add query method | `internal/registry/registry.go` — new getter |
| Change storage backend | Replace `fileStore` with SQLite/BoltDB |
| Add versioning | `internal/registry/registry.go` — add schema version field |
