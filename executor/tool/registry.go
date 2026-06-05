// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"matrix/executor/mcp"
)

// Registry resolves matrix://tool/... URIs to live Tool implementations.
//
// Wires NativeTool slots (Q19 placeholders) and MCPTool entries
// (Q4 + Q21) against a backing mcp.Manager. Built once per agent at
// boot from an AgentManifest; immutable afterwards (manifest reloads
// rebuild the registry).
type Registry struct {
	manifest *AgentManifest

	// gate enforces SideEffectClass against the manifest's allowed set.
	gate CapabilityGate

	// mu guards the lookup tables. Tables are populated at Build() time
	// and never modified afterwards in v1; mutex is here so a future
	// hot-reload can flip the contents under a lock.
	mu      sync.RWMutex
	mcps    map[string]*MCPTool    // canonical URI → tool
	natives map[string]*NativeTool // canonical URI → tool

	// mgr is the MCP server pool. nil = registry has no MCP backends
	// (still useful for native-only tests).
	mgr *mcp.Manager

	// clock is overridable for tests.
	clock func() time.Time
}

// RegistryParams configures a Registry.
type RegistryParams struct {
	Manifest *AgentManifest
	MCP      *mcp.Manager

	// Gate optionally overrides the manifest-derived gate. nil = use
	// AllowAllSideEffects narrowed by manifest.AllowedSideEffects.
	Gate CapabilityGate

	// Clock for tests; defaults to time.Now.
	Clock func() time.Time
}

// NewRegistry builds a registry from a manifest. MCP servers are
// expected to already be Spawned in the supplied Manager — that's the
// boot-order responsibility of the caller (typically cmd/mcl-execute).
func NewRegistry(p RegistryParams) (*Registry, error) {
	if p.Manifest == nil {
		return nil, errors.New("tool: registry requires manifest")
	}
	if err := p.Manifest.Validate(); err != nil {
		return nil, err
	}

	gate := p.Gate
	if gate == nil {
		if len(p.Manifest.AllowedSideEffects) == 0 {
			gate = AllowAllSideEffects
		} else {
			gate = AllowOnlySideEffects(p.Manifest.AllowedSideEffects...)
		}
	}

	clock := p.Clock
	if clock == nil {
		clock = time.Now
	}

	r := &Registry{
		manifest: p.Manifest,
		gate:     gate,
		mcps:     make(map[string]*MCPTool),
		natives:  make(map[string]*NativeTool),
		mgr:      p.MCP,
		clock:    clock,
	}

	for i := range p.Manifest.Servers {
		s := &p.Manifest.Servers[i]
		for j := range s.Tools {
			te := &s.Tools[j]
			uri := ToolURI{
				Provider: "mcp",
				Server:   s.Alias,
				Name:     te.Name,
				Version:  s.Version,
			}.String()
			r.mcps[uri] = &MCPTool{
				uri:        uri,
				server:     s.Alias,
				name:       te.Name,
				desc:       te.Description,
				sideEffect: te.SideEffectClass,
				timeout:    teTimeout(te.TimeoutMs),
				mgr:        p.MCP,
				clock:      clock,
			}
		}
	}

	for i := range p.Manifest.NativeTools {
		nt := &p.Manifest.NativeTools[i]
		side := nt.SideEffectClass
		if side == "" {
			side = SideEffectChain
		}
		uri := ToolURI{
			Provider: nt.Namespace,
			Name:     nt.Name,
			Version:  nt.Version,
		}.String()
		r.natives[uri] = &NativeTool{
			uri:        uri,
			namespace:  nt.Namespace,
			name:       nt.Name,
			version:    nt.Version,
			digest:     nt.Digest,
			sideEffect: side,
		}
	}

	return r, nil
}

// teTimeout maps the manifest TimeoutMs (0 = default) to a duration
// used by MCPTool.Call. 30s default mirrors http.DefaultClient.
func teTimeout(ms int) time.Duration {
	if ms <= 0 {
		return 30 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// Get resolves a tool URI to a Tool. Returns ErrUnknownTool when the
// URI doesn't appear in the agent manifest.
func (r *Registry) Get(uri string) (Tool, error) {
	parsed, err := ParseToolURI(uri)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if parsed.IsMCP() {
		t, ok := r.mcps[parsed.String()]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownTool, parsed.String())
		}
		if !r.gate(t.sideEffect) {
			return nil, fmt.Errorf("%w: tool %s requires side-effect %q", ErrSideEffectDenied, parsed.String(), t.sideEffect)
		}
		return t, nil
	}
	if parsed.IsNative() {
		t, ok := r.natives[parsed.String()]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownTool, parsed.String())
		}
		if !r.gate(t.sideEffect) {
			return nil, fmt.Errorf("%w: tool %s requires side-effect %q", ErrSideEffectDenied, parsed.String(), t.sideEffect)
		}
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownTool, uri)
}

// List returns every tool URI in the registry, sorted alphabetically.
// Used by mcl-tools CLI and audit log paths.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.mcps)+len(r.natives))
	for uri := range r.mcps {
		out = append(out, uri)
	}
	for uri := range r.natives {
		out = append(out, uri)
	}
	sortStrings(out)
	return out
}

// Manifest returns the underlying agent manifest. Read-only.
func (r *Registry) Manifest() *AgentManifest { return r.manifest }

// MCPTool is the Tool implementation backed by an MCP server (Q4).
type MCPTool struct {
	uri        string
	server     string
	name       string
	desc       string
	sideEffect string
	timeout    time.Duration

	mgr   *mcp.Manager
	clock func() time.Time
}

// URI implements Tool.
func (t *MCPTool) URI() string { return t.uri }

// Description implements Tool.
func (t *MCPTool) Description() string { return t.desc }

// SideEffectClass implements Tool.
func (t *MCPTool) SideEffectClass() string { return t.sideEffect }

// Server returns the alias of the MCP server backing this tool.
// Exposed so the cmd/mcl-tools CLI can show resolution.
func (t *MCPTool) Server() string { return t.server }

// Name returns the server-local tool name.
func (t *MCPTool) Name() string { return t.name }

// Call invokes the tool through the manager-managed MCP client.
//
// Surfaces transport / RPC errors as Go errors; in-band tool failures
// (the tool ran but reports IsError) are returned via Result.IsError.
func (t *MCPTool) Call(ctx context.Context, args map[string]interface{}) (*Result, error) {
	if t.mgr == nil {
		return nil, errors.New("tool: MCP manager not configured")
	}
	c := t.mgr.Client(t.server)
	if c == nil {
		return nil, fmt.Errorf("tool: MCP server %q not running", t.server)
	}

	// Apply per-call timeout if not already bounded by ctx.
	callCtx := ctx
	if t.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	callID := newCallID()
	start := t.clock()

	mcpResult, err := c.ToolsCall(callCtx, t.name, args)
	dur := t.clock().Sub(start).Milliseconds()
	if err != nil {
		return nil, err
	}

	out := &Result{
		IsError:    mcpResult.IsError,
		CallID:     callID,
		DurationMs: dur,
	}
	for _, c := range mcpResult.Content {
		out.Content = append(out.Content, Content{
			Type:     c.Type,
			Text:     c.Text,
			Data:     c.Data,
			MimeType: c.MimeType,
			URI:      embeddedURI(c.Resource),
		})
	}
	return out, nil
}

func embeddedURI(r *mcp.EmbeddedResource) string {
	if r == nil {
		return ""
	}
	return r.URI
}

// NativeTool is the architectural slot for chain-touching tools (Q19).
// v1 ships no implementations; Call returns a not-implemented error so
// any plan that references a native tool in v1 fails clearly.
type NativeTool struct {
	uri        string
	namespace  string
	name       string
	version    string
	digest     string
	sideEffect string
}

// URI implements Tool.
func (t *NativeTool) URI() string { return t.uri }

// Description implements Tool.
func (t *NativeTool) Description() string {
	return fmt.Sprintf("native chain tool %s/%s@%s (v1 placeholder)", t.namespace, t.name, t.version)
}

// SideEffectClass implements Tool.
func (t *NativeTool) SideEffectClass() string { return t.sideEffect }

// Namespace returns the S6 namespace.
func (t *NativeTool) Namespace() string { return t.namespace }

// Digest returns the contract/abi digest pinned at manifest time.
func (t *NativeTool) Digest() string { return t.digest }

// Call always returns ErrNativeToolNotImplemented in v1.
func (t *NativeTool) Call(ctx context.Context, args map[string]interface{}) (*Result, error) {
	return nil, fmt.Errorf("%w: %s/%s@%s", ErrNativeToolNotImplemented, t.namespace, t.name, t.version)
}

// ErrNativeToolNotImplemented signals the v1 placeholder behaviour.
// Wraps cleanly with errors.Is for plan-walker error routing.
var ErrNativeToolNotImplemented = errors.New("tool: native chain tool not implemented in v1")

// newCallID generates a 16-byte random hex id for tool invocations.
// Not a ULID (tool layer doesn't depend on cortex's oklog/ulid); we
// use crypto/rand so the id is collision-free even under high concurrency.
func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sortStrings is a small in-package sort to avoid importing sort from
// every consumer.
func sortStrings(s []string) {
	// Insertion sort — registries are small (tens of tools per agent).
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
