package mcp

// Tool describes one MCP tool (mirrors docs/mcp-tools.md).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolNames is the canonical v1 tool list for selftest.
var ToolNames = []string{
	"tachyon_compile",
	"tachyon_test",
	"tachyon_simulate",
	"tachyon_deploy",
	"tachyon_call",
	"tachyon_chain_list",
	"tachyon_chain_register",
	"tachyon_artifact_get",
	"tachyon_registry_lookup",
}

// Tools returns MCP tool descriptors.
func Tools() []Tool {
	obj := map[string]any{"type": "object", "additionalProperties": true}
	return []Tool{
		{Name: "tachyon_compile", Description: "Build Solidity contracts via forge", InputSchema: obj},
		{Name: "tachyon_test", Description: "Run Forge tests with structured JSON results", InputSchema: obj},
		{Name: "tachyon_simulate", Description: "Dry-run eth_call without broadcasting", InputSchema: obj},
		{Name: "tachyon_deploy", Description: "Intent-based deploy with idempotency key", InputSchema: obj},
		{Name: "tachyon_call", Description: "Contract call (simulate_only or broadcast)", InputSchema: obj},
		{Name: "tachyon_chain_list", Description: "List configured chain RPC profiles", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "tachyon_chain_register", Description: "Register a custom chain profile", InputSchema: obj},
		{Name: "tachyon_artifact_get", Description: "Fetch cached ABI/bytecode by contract name", InputSchema: obj},
		{Name: "tachyon_registry_lookup", Description: "Resolve prior deployment by idempotency key", InputSchema: obj},
	}
}
