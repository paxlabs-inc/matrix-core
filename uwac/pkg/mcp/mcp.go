// Package mcp builds the MCP tool advertisement from the connector registry.
// This is the single source of truth for the tool set; tools/uwac/uwac-tools.json
// (the stdio proxy's static registry) is generated from `uwacd -dump-tools`, and
// the proxy's --selftest enforces bijection with agents/*.json so any drift
// fails at build/CI time instead of crashing the daemon at boot.
package mcp

import (
	"encoding/json"

	"github.com/paxlabs-inc/uwac/internal/connectors"
)

// Tool is the MCP tools/list advertisement shape.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Tools builds the advertisement from the registry (sorted by name).
func Tools(reg *connectors.Registry) []Tool {
	specs := reg.Tools()
	out := make([]Tool, 0, len(specs))
	for _, t := range specs {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "additionalProperties": true, "properties": map[string]any{}}
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

// DumpJSON renders the tool advertisement as indented JSON (for generating
// tools/uwac/uwac-tools.json).
func DumpJSON(reg *connectors.Registry) ([]byte, error) {
	return json.MarshalIndent(Tools(reg), "", "  ")
}
