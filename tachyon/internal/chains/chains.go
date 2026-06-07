package chains

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Manager loads presets and custom chain profiles.
type Manager struct {
	mu          sync.RWMutex
	presets     []types.ChainProfile
	custom      map[string]types.ChainProfile
	presetsPath string
	projectRoot string
}

// New loads chain presets from presetsPath (JSON file).
func New(presetsPath string) (*Manager, error) {
	m := &Manager{
		custom:      map[string]types.ChainProfile{},
		presetsPath: presetsPath,
	}
	if err := m.reloadPresets(); err != nil {
		return nil, err
	}
	return m, nil
}

// SetProjectRoot stores the default project root for relative lookups.
func (m *Manager) SetProjectRoot(root string) {
	m.mu.Lock()
	m.projectRoot = root
	m.mu.Unlock()
}

// ProjectRoot returns the configured project root.
func (m *Manager) ProjectRoot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.projectRoot
}

type presetsFile struct {
	Chains []types.ChainProfile `json:"chains"`
}

func (m *Manager) reloadPresets() error {
	b, err := os.ReadFile(m.presetsPath)
	if err != nil {
		if os.IsNotExist(err) {
			m.mu.Lock()
			m.presets = nil
			m.mu.Unlock()
			return nil
		}
		return err
	}
	var pf presetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return err
	}
	for i := range pf.Chains {
		m.resolveEnv(&pf.Chains[i])
	}
	m.mu.Lock()
	m.presets = pf.Chains
	m.mu.Unlock()
	return nil
}

func (m *Manager) resolveEnv(p *types.ChainProfile) {
	if p.RPCURL == "" && p.RPCURLEnv != "" {
		p.RPCURL = strings.TrimSpace(os.Getenv(p.RPCURLEnv))
	}
}

// List returns all chains; activeID marks the active profile.
func (m *Manager) List(activeID string) types.ChainListResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]types.ChainProfile, 0, len(m.presets)+len(m.custom))
	seen := map[string]struct{}{}
	for _, p := range m.presets {
		cp := p
		cp.Active = cp.ID == activeID
		out = append(out, cp)
		seen[cp.ID] = struct{}{}
	}
	for id, p := range m.custom {
		cp := p
		cp.Active = id == activeID
		out = append(out, cp)
	}
	return types.ChainListResponse{Chains: out, ActiveChainID: activeID}
}

// Get resolves a chain by id.
func (m *Manager) Get(id string) (types.ChainProfile, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.presets {
		if p.ID == id {
			cp := p
			m.resolveEnv(&cp)
			if cp.RPCURL != "" {
				return cp, true
			}
			return cp, cp.RPCURLEnv == "" // preset without env unset is not usable
		}
	}
	p, ok := m.custom[id]
	return p, ok
}

// Register adds or updates a custom chain.
func (m *Manager) Register(req types.ChainRegisterRequest) (types.ChainProfile, *types.Error) {
	id := strings.TrimSpace(req.ID)
	if id == "" || req.ChainID == 0 || strings.TrimSpace(req.RPCURL) == "" {
		return types.ChainProfile{}, types.NewError(types.CodeInvalidRequest, "id, chain_id, rpc_url required", false, nil)
	}
	p := types.ChainProfile{
		ID:       id,
		Name:     req.Name,
		RPCURL:   strings.TrimSpace(req.RPCURL),
		ChainID:  req.ChainID,
		Preset:   req.Preset,
		Explorer: req.Explorer,
		Features: []string{"debug_trace"},
	}
	m.mu.Lock()
	m.custom[id] = p
	m.mu.Unlock()
	return p, nil
}

// Resolve picks chain from id, inline rpc, or active registry id.
func (m *Manager) Resolve(chainID, rpcURL string, activeID string) (types.ChainProfile, *types.Error) {
	if rpcURL != "" {
		return types.ChainProfile{
			ID:      "inline",
			Name:    "inline",
			RPCURL:  rpcURL,
			ChainID: 0,
		}, nil
	}
	id := strings.TrimSpace(chainID)
	if id == "" {
		id = activeID
	}
	if id == "" {
		return types.ChainProfile{}, types.NewError(types.CodeChainNotFound, "chain_id required", false, nil)
	}
	p, ok := m.Get(id)
	if !ok || p.RPCURL == "" {
		return types.ChainProfile{}, types.NewError(types.CodeChainNotFound, "chain not found or rpc unset: "+id, false, nil)
	}
	return p, nil
}

// DefaultPresetsPath returns chains/presets.json relative to repo root.
func DefaultPresetsPath(projectRoot string) string {
	return filepath.Join(projectRoot, "chains", "presets.json")
}

// AvailableIDs returns chain ids with configured RPC.
func (m *Manager) AvailableIDs() []string {
	list := m.List("")
	var ids []string
	for _, c := range list.Chains {
		if c.RPCURL != "" || c.RPCURLEnv != "" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}
