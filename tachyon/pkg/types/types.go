package types

// Envelope is the versioned JSON wrapper shared by REST, JSON-RPC, and MCP.
type Envelope[T any] struct {
	Ok    bool   `json:"ok"`
	Data  T      `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error carries machine-stable codes for agent retry logic.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Retry   bool   `json:"retry"`
	Details any    `json:"details,omitempty"`
}

// HealthData is returned by GET /healthz.
type HealthData struct {
	Version string   `json:"version"`
	Forge   string   `json:"forge,omitempty"`
	Chains  []string `json:"chains"`
	Project string   `json:"project_root"`
}
