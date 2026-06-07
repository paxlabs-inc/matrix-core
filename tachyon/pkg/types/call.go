package types

// CallRequest invokes a contract (simulate or broadcast).
type CallRequest struct {
	ChainID         string `json:"chain_id,omitempty"`
	RPCURL          string `json:"rpc_url,omitempty"`
	From            string `json:"from,omitempty"`
	To              string `json:"to"`
	Data            string `json:"data,omitempty"`
	Method          string `json:"method,omitempty"`
	Args            any    `json:"args,omitempty"`
	Value           string `json:"value,omitempty"`
	SimulateOnly    bool   `json:"simulate_only,omitempty"`
	CapabilityToken string `json:"capability_token,omitempty"`
	SpendCapWei     string `json:"spend_cap_wei,omitempty"`
}

// CallResponse is the result of call verb.
type CallResponse struct {
	Result string `json:"result,omitempty"`
	TxHash string `json:"tx_hash,omitempty"`
	Revert string `json:"revert,omitempty"`
}
