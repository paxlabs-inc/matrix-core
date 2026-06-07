package types

// SimulateRequest dry-runs a call without broadcasting.
type SimulateRequest struct {
	ChainID string `json:"chain_id,omitempty"`
	RPCURL  string `json:"rpc_url,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Data    string `json:"data,omitempty"`
	Value   string `json:"value,omitempty"`
	Block   string `json:"block,omitempty"`
	Trace   bool   `json:"trace,omitempty"`
}

// SimulateResponse holds eth_call results.
type SimulateResponse struct {
	Result      string `json:"result,omitempty"`
	GasEstimate uint64 `json:"gas_estimate,omitempty"`
	Revert      string `json:"revert,omitempty"`
	Trace       any    `json:"trace,omitempty"`
}
