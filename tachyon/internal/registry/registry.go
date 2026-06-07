package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// ArtifactRecord is a persisted build artifact.
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

// DeploymentRecord tracks an idempotent deploy.
type DeploymentRecord struct {
	IdempotencyKey string `json:"idempotency_key"`
	ChainID        string `json:"chain_id"`
	Contract       string `json:"contract"`
	Address        string `json:"address"`
	TxHash         string `json:"tx_hash,omitempty"`
	Confirmed      bool   `json:"confirmed"`
	ProjectID      string `json:"project_id,omitempty"`
}

type fileStore struct {
	Artifacts   map[string]ArtifactRecord   `json:"artifacts"`
	Deployments map[string]DeploymentRecord `json:"deployments"`
	ActiveChain string                      `json:"active_chain_id,omitempty"`
}

// Registry is a JSON-file backed artifact + deployment index.
type Registry struct {
	path string
	mu   sync.RWMutex
	data fileStore
}

// Open loads or creates a registry at path.
func Open(path string) (*Registry, error) {
	r := &Registry{
		path: path,
		data: fileStore{
			Artifacts:   map[string]ArtifactRecord{},
			Deployments: map[string]DeploymentRecord{},
		},
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func artifactKey(projectID, name string) string {
	return projectID + ":" + name
}

func deploymentKey(idempotencyKey, chainID string) string {
	return idempotencyKey + ":" + chainID
}

func (r *Registry) load() error {
	b, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return r.save()
		}
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.Unmarshal(b, &r.data)
}

func (r *Registry) save() error {
	if dir := filepath.Dir(r.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	r.mu.RLock()
	b, err := json.MarshalIndent(r.data, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, b, 0o644)
}

// PutArtifact indexes a compiled contract.
func (r *Registry) PutArtifact(rec ArtifactRecord) error {
	r.mu.Lock()
	r.data.Artifacts[artifactKey(rec.ProjectID, rec.Name)] = rec
	r.mu.Unlock()
	return r.save()
}

// GetArtifact fetches by project and contract name.
func (r *Registry) GetArtifact(projectID, name string) (ArtifactRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.data.Artifacts[artifactKey(projectID, name)]
	return rec, ok
}

// ListArtifacts returns all artifacts for a project.
func (r *Registry) ListArtifacts(projectID string) []ArtifactRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ArtifactRecord
	for k, rec := range r.data.Artifacts {
		if projectID == "" || rec.ProjectID == projectID {
			_ = k
			out = append(out, rec)
		}
	}
	return out
}

// PutDeployment stores a deployment record.
func (r *Registry) PutDeployment(rec DeploymentRecord) error {
	r.mu.Lock()
	r.data.Deployments[deploymentKey(rec.IdempotencyKey, rec.ChainID)] = rec
	r.mu.Unlock()
	return r.save()
}

// GetDeployment looks up by idempotency key and chain.
func (r *Registry) GetDeployment(idempotencyKey, chainID string) (DeploymentRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.data.Deployments[deploymentKey(idempotencyKey, chainID)]
	return rec, ok
}

// ActiveChainID returns the selected chain profile id.
func (r *Registry) ActiveChainID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.data.ActiveChain
}

// SetActiveChain stores the active chain id.
func (r *Registry) SetActiveChain(chainID string) error {
	r.mu.Lock()
	r.data.ActiveChain = chainID
	r.mu.Unlock()
	return r.save()
}

// ToTypesArtifact converts a registry record to API type.
func ToTypesArtifact(rec ArtifactRecord) types.Artifact {
	var compiler *types.CompilerSettings
	if len(rec.Compiler) > 0 {
		var c types.CompilerSettings
		if err := json.Unmarshal(rec.Compiler, &c); err == nil {
			compiler = &c
		}
	}
	return types.Artifact{
		Name:             rec.Name,
		Path:             rec.Path,
		ABI:              rec.ABI,
		Bytecode:         rec.Bytecode,
		DeployedBytecode: rec.DeployedBytecode,
		Compiler:         compiler,
	}
}
