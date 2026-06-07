package types

import "encoding/json"

// Create2Config optional CREATE2 deployment parameters.
type Create2Config struct {
	Salt     string `json:"salt"`
	Deployer string `json:"deployer"`
}

// DeployRequest intent-based contract deployment.
type DeployRequest struct {
	Intent         string          `json:"intent,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	ChainID        string          `json:"chain_id"`
	ProjectID      string          `json:"project_id,omitempty"`
	Contract       string          `json:"contract"`
	ConstructorArgs json.RawMessage `json:"constructor_args,omitempty"`
	Create2        *Create2Config  `json:"create2,omitempty"`
	From           string          `json:"from,omitempty"`
	CapabilityToken string         `json:"capability_token,omitempty"`
	SpendCapWei    string          `json:"spend_cap_wei,omitempty"`
	// WalletToken is a forwarded embedded-wallet bearer (the agent's
	// short-lived did:matrix token). When set, the shared engine signs +
	// broadcasts as that agent server-side without holding any seed.
	WalletToken string `json:"wallet_token,omitempty"`
}

// DeployResponse records an on-chain deployment.
type DeployResponse struct {
	Address        string `json:"address"`
	TxHash         string `json:"tx_hash,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	ChainID        string `json:"chain_id"`
	Contract       string `json:"contract"`
	Existing       bool   `json:"existing,omitempty"`
}
