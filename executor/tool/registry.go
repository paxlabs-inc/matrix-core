// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/executor/mcp"
)

// resultCache is an OPT-IN TTL cache for idempotent read-only tool
// results (P2-5). It is keyed by (toolURI, canonicalArgsJSON) and only
// ever stores results for tools explicitly listed in cacheableTools
// (start: web_search, web_news). Side-effecting, shell, chain, and
// money tools are NEVER cached regardless of args.
//
// The cache lives in the Registry (the layer that owns MCPTool.Call and
// knows each tool's declared side-effect class), NOT in mcp.Manager —
// the server-pool lifecycle stays untouched (Q16 acceptance).
//
// Thread-safe. A zero CacheTTL disables caching entirely (the default
// for tests and for any production path that hasn't opted in via
// NEO_MCP_CACHE_TTL_SECONDS).
type resultCache struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	reqs map[string]cacheEntry
}

// cacheEntry stores one cached Call result with its expiry instant.
type cacheEntry struct {
	result *Result
	expiry time.Time
}

// cacheableTools is the closed allow-list of tool NAMES (server-local,
// i.e. the ToolEntry.Name, NOT the function name) whose results are
// safe to cache: pure read-only idempotent lookups. web_search /
// web_news return ranked results for a fixed query and never mutate
// state. Anything that writes, shells out, touches the chain, or moves
// funds is excluded by omission — "NEVER cache side-effecting or money
// tools" (P2-5 mandate).
var cacheableTools = map[string]bool{
	"web_search": true,
	"web_news":   true,
}

// isCacheable reports whether a tool's results may be cached, given its
// server-local name and declared side-effect class. Both conditions must
// hold: the name must be in the explicit allow-list AND the side-effect
// class must be a read-class (SideEffectRead). The double gate means a
// tool named web_search that someone later retags as "write" stops
// being cached automatically — defence in depth against manifest drift.
func isCacheable(name, sideEffect string) bool {
	if sideEffect != SideEffectRead {
		return false
	}
	return cacheableTools[name]
}

// cacheKey builds the canonical cache key for one (uri, args) pair. The
// args map is JSON-encoded with sorted keys so two calls with the same
// arguments in different map-iteration order produce the same key
// (determinism, i6).
func cacheKey(uri string, args map[string]interface{}) (string, bool) {
	if len(args) == 0 {
		return uri + "|{}", true
	}
	b, err := canonicalArgs(args)
	if err != nil {
		// Un-encodable args → don't cache (safe fallback).
		return "", false
	}
	return uri + "|" + string(b), true
}

// canonicalArgs marshals args to JSON with sorted object keys so the
// cache key is deterministic regardless of map iteration order.
func canonicalArgs(args map[string]interface{}) ([]byte, error) {
	if len(args) == 0 {
		return []byte("{}"), nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		vb, err := json.Marshal(args[k])
		if err != nil {
			return nil, err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

// get returns a cached result if present and not expired.
func (c *resultCache) get(key string) (*Result, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.reqs[key]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiry) {
		delete(c.reqs, key)
		return nil, false
	}
	// Return a shallow copy so callers can't mutate the cached entry.
	cp := *e.result
	return &cp, true
}

// put stores a result with the configured TTL. No-op when caching is
// disabled (ttl<=0) or the result is an in-band error (we don't cache
// failures — a transient IsError shouldn't poison subsequent calls).
func (c *resultCache) put(key string, res *Result) {
	if c == nil || c.ttl <= 0 || res == nil || res.IsError {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reqs == nil {
		c.reqs = make(map[string]cacheEntry)
	}
	// Store a shallow copy so the caller's later mutation of res can't
	// corrupt the cached value.
	cp := *res
	c.reqs[key] = cacheEntry{result: &cp, expiry: c.now().Add(c.ttl)}
}

// newResultCache constructs a cache. ttl<=0 returns a disabled cache
// (all get/put are no-ops), which is the default.
func newResultCache(ttl time.Duration, now func() time.Time) *resultCache {
	if ttl <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &resultCache{ttl: ttl, now: now, reqs: make(map[string]cacheEntry)}
}

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

	// cache is the OPT-IN TTL result cache for idempotent read-only
	// tools (P2-5). nil when CacheTTL<=0 (caching disabled). Only tools
	// in the cacheableTools allow-list with SideEffectRead are cached.
	cache *resultCache

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

	// CacheTTL enables the idempotent-read result cache (P2-5) when > 0.
	// Results for tools in the cacheableTools allow-list (web_search,
	// web_news) are served from cache within the TTL; side-effecting and
	// money tools are never cached. Zero (the default) disables caching.
	CacheTTL time.Duration
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
		cache:    newResultCache(p.CacheTTL, clock),
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
				cache:      r.cache,
				cacheable:  isCacheable(te.Name, te.SideEffectClass),
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

	// cache is the shared registry-level result cache (P2-5). nil when
	// caching is disabled (CacheTTL<=0). Only consulted when cacheable.
	cache *resultCache
	// cacheable is precomputed at build time from isCacheable(name,
	// sideEffect) so the hot path is a single bool check.
	cacheable bool
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
//
// P2-5: for tools in the cacheableTools allow-list (web_search, web_news)
// with SideEffectRead, a successful result is served from the TTL cache
// on a hit and stored on a miss. Side-effecting / money tools never hit
// the cache (cacheable is precomputed false for them). Cache misses and
// errors always go to the wire.
func (t *MCPTool) Call(ctx context.Context, args map[string]interface{}) (*Result, error) {
	if t.mgr == nil {
		return nil, errors.New("tool: MCP manager not configured")
	}

	// P2-5: cache lookup (only for idempotent read tools). A hit returns
	// a copy of the cached result immediately — no MCP round-trip.
	if t.cacheable && t.cache != nil {
		if key, ok := cacheKey(t.uri, args); ok {
			if cached, hit := t.cache.get(key); hit {
				return cached, nil
			}
		}
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

	// P2-5: store the successful result in the cache for idempotent
	// read tools. In-band errors (IsError) are never cached so a
	// transient failure can't poison subsequent calls.
	if t.cacheable && t.cache != nil && !out.IsError {
		if key, ok := cacheKey(t.uri, args); ok {
			t.cache.put(key, out)
		}
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
