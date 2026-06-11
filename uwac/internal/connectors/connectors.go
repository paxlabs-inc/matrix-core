// Package connectors indexes the declarative connector specs and their Go
// handlers. A connector is the {oauth_spec, mcp_tools, event_sources} triple
// from connections.frozen.kvx; the Registry maps each tool name to the
// connector that owns it plus the handler that performs the provider call.
package connectors

import (
	"context"
	"fmt"
	"sort"

	"github.com/paxlabs-inc/uwac/internal/vault"
	"github.com/paxlabs-inc/uwac/pkg/types"
)

// Handler executes one tool's provider API call. rec carries a FRESH provider
// access token (the engine refreshes before dispatch). Handlers must never
// return a fabricated success; provider errors propagate up.
type Handler func(ctx context.Context, rec *vault.Record, args map[string]any) (any, error)

// Connector bundles a declarative spec with its per-tool handlers.
type Connector struct {
	Spec     types.ConnectorSpec
	Handlers map[string]Handler
}

// bound links a tool to its owning connector.
type bound struct {
	Connector *Connector
	Tool      types.ToolSpec
	Handler   Handler
}

// Registry indexes connectors by id and tools by name.
type Registry struct {
	connectors map[string]*Connector
	byTool     map[string]bound
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{connectors: map[string]*Connector{}, byTool: map[string]bound{}}
}

// Register indexes a connector. It errors on a duplicate connector id, a
// duplicate tool name (would crash daemon boot via Manager.verifyTools), or a
// tool advertised without a handler.
func (r *Registry) Register(c *Connector) error {
	if c == nil || c.Spec.ID == "" {
		return fmt.Errorf("connectors: connector requires an id")
	}
	if _, dup := r.connectors[c.Spec.ID]; dup {
		return fmt.Errorf("connectors: duplicate connector id %q", c.Spec.ID)
	}
	for _, t := range c.Spec.Tools {
		if _, dup := r.byTool[t.Name]; dup {
			return fmt.Errorf("connectors: duplicate tool name %q", t.Name)
		}
		h := c.Handlers[t.Name]
		if h == nil {
			return fmt.Errorf("connectors: tool %q has no handler", t.Name)
		}
	}
	r.connectors[c.Spec.ID] = c
	for _, t := range c.Spec.Tools {
		r.byTool[t.Name] = bound{Connector: c, Tool: t, Handler: c.Handlers[t.Name]}
	}
	return nil
}

// Lookup resolves a tool name to its connector, spec, and handler.
func (r *Registry) Lookup(tool string) (*Connector, types.ToolSpec, Handler, bool) {
	b, ok := r.byTool[tool]
	if !ok {
		return nil, types.ToolSpec{}, nil, false
	}
	return b.Connector, b.Tool, b.Handler, true
}

// Tools returns every advertised tool, sorted by name (stable manifest order).
func (r *Registry) Tools() []types.ToolSpec {
	out := make([]types.ToolSpec, 0, len(r.byTool))
	for _, b := range r.byTool {
		out = append(out, b.Tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolNames returns the sorted advertised tool names (for selftest/bijection).
func (r *Registry) ToolNames() []string {
	names := make([]string, 0, len(r.byTool))
	for name := range r.byTool {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Specs returns the connector specs, sorted by id.
func (r *Registry) Specs() []types.ConnectorSpec {
	out := make([]types.ConnectorSpec, 0, len(r.connectors))
	for _, c := range r.connectors {
		out = append(out, c.Spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
