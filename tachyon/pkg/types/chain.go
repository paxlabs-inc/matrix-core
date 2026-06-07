package types

// ChainProfile describes an RPC endpoint agents can target.
type ChainProfile struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	RPCURL     string   `json:"rpc_url,omitempty"`
	RPCURLEnv  string   `json:"rpc_url_env,omitempty"`
	ChainID    uint64   `json:"chain_id"`
	Preset     string   `json:"preset,omitempty"`
	Explorer   string   `json:"explorer,omitempty"`
	Features   []string `json:"features,omitempty"`
	Active     bool     `json:"active,omitempty"`
}

// ChainListResponse lists known chain profiles.
type ChainListResponse struct {
	Chains       []ChainProfile `json:"chains"`
	ActiveChainID string        `json:"active_chain_id,omitempty"`
}

// ChainRegisterRequest adds or updates a custom chain profile.
type ChainRegisterRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RPCURL   string `json:"rpc_url"`
	ChainID  uint64 `json:"chain_id"`
	Preset   string `json:"preset,omitempty"`
	Explorer string `json:"explorer,omitempty"`
}

// ChainUseRequest selects the active chain for subsequent ops.
type ChainUseRequest struct {
	ChainID string `json:"chain_id"`
}
