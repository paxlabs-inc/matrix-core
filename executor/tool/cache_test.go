// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package tool

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/executor/mcp"
)

// cacheTestManifest builds a manifest with one MCP server exposing a
// cacheable read tool (web_search) and a non-cacheable write tool, both
// declared with the given side-effect classes so the cache's double
// gate (name allow-list + SideEffectRead) is exercised.
func cacheTestManifest() *AgentManifest {
	dig := "sha256:" + strings.Repeat("a", 64)
	return &AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/test",
		Servers: []ServerEntry{
			{
				Alias:         "web",
				Transport:     "stdio",
				Command:       "fake",
				Version:       "0.1.0",
				PackageDigest: dig,
				Tools: []ToolEntry{
					{Name: "web_search", SideEffectClass: SideEffectRead, TimeoutMs: 5000},
					{Name: "write_file", SideEffectClass: SideEffectWrite},
					{Name: "send_value", SideEffectClass: SideEffectChain},
				},
			},
		},
	}
}

// countingHandler returns a CallHandler that counts how many times the
// underlying server actually fielded a tools/call — so tests can assert
// cache hits (count stays at 1) vs misses (count increments).
func countingHandler() (func(name string, args map[string]interface{}) (*mcp.CallToolResult, error), *int64) {
	var calls int64
	h := func(name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
		atomic.AddInt64(&calls, 1)
		// Echo the query back so the result is deterministic per-arg.
		q, _ := args["query"].(string)
		return &mcp.CallToolResult{
			Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "result:" + q}},
		}, nil
	}
	return h, &calls
}

// spawnCacheRegistry wires a mock MCP server + manager + registry with
// the cache enabled at the given TTL. Returns the registry and a count
// of how many times the mock server's call handler actually fired.
func spawnCacheRegistry(t *testing.T, ttl time.Duration) (*Registry, *int64) {
	t.Helper()
	handler, calls := countingHandler()
	mock := mcp.NewMockServer(mcp.MockServerParams{
		Tools:       []mcp.Tool{{Name: "web_search"}, {Name: "write_file"}, {Name: "send_value"}},
		CallHandler: handler,
	})
	mgr := mcp.NewManager(mcp.ManagerParams{
		TransportBuilder: func(spec mcp.ServerSpec) (mcp.Transport, error) {
			return mcp.PipeMock(mock), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Spawn(context.Background(), mcp.ServerSpec{
		Alias: "web", Transport: "stdio",
		ExpectedTools: []string{"web_search", "write_file", "send_value"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	r, err := NewRegistry(RegistryParams{
		Manifest: cacheTestManifest(),
		MCP:      mgr,
		CacheTTL: ttl,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, calls
}

// TestMCPCache_HitWithinTTL: a second identical call to an idempotent
// read tool within the TTL MUST be served from cache — the underlying
// server's call handler fires exactly once, not twice.
func TestMCPCache_HitWithinTTL(t *testing.T) {
	r, calls := spawnCacheRegistry(t, 5*time.Minute)

	tl, err := r.Get("matrix://tool/mcp/web/web_search@0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	args := map[string]interface{}{"query": "golang mcp"}

	res1, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 1: %v", err)
	}
	if got := ExtractText(res1); got != "result:golang mcp" {
		t.Fatalf("call 1 text=%q", got)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("after call 1, server should have been hit once, got %d", n)
	}

	// Second identical call within TTL → cache hit, server NOT hit again.
	res2, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 2: %v", err)
	}
	if got := ExtractText(res2); got != "result:golang mcp" {
		t.Fatalf("call 2 text=%q (cache should return identical result)", got)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("after cache hit, server should STILL have been hit once, got %d (cache missed)", n)
	}
}

// TestMCPCache_MissAfterExpiry: once the TTL elapses, the next identical
// call MUST go back to the wire (cache miss), proving the TTL is honored.
// Uses a fake clock + a counting handler so expiry is deterministic and
// the second server hit is observable without sleeping.
func TestMCPCache_MissAfterExpiry(t *testing.T) {
	handler, calls := countingHandler()
	mock := mcp.NewMockServer(mcp.MockServerParams{
		Tools:       []mcp.Tool{{Name: "web_search"}},
		CallHandler: handler,
	})
	mgr := mcp.NewManager(mcp.ManagerParams{
		TransportBuilder: func(spec mcp.ServerSpec) (mcp.Transport, error) {
			return mcp.PipeMock(mock), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Spawn(context.Background(), mcp.ServerSpec{
		Alias: "web", Transport: "stdio", ExpectedTools: []string{"web_search"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Use a fake clock so we can advance time past the TTL without sleeping.
	t0 := time.Now()
	clock := &fakeClock{t: t0}
	r, err := NewRegistry(RegistryParams{
		Manifest: cacheTestManifest(),
		MCP:      mgr,
		CacheTTL: 50 * time.Millisecond,
		Clock:    clock.now,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tl, err := r.Get("matrix://tool/mcp/web/web_search@0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	args := map[string]interface{}{"query": "expiry-test"}

	if _, err := tl.Call(context.Background(), args); err != nil {
		t.Fatalf("Call 1: %v", err)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("after call 1, server hit count = %d, want 1", n)
	}

	// A second identical call WITHIN the TTL is a cache hit (no new hit).
	if _, err := tl.Call(context.Background(), args); err != nil {
		t.Fatalf("Call 2 (within TTL): %v", err)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("within TTL, server hit count = %d, want 1 (cache hit)", n)
	}

	// Advance the clock PAST the TTL.
	clock.advance(51 * time.Millisecond)

	// Next call must miss the cache and hit the server again.
	res3, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 3 (post-expiry): %v", err)
	}
	if got := ExtractText(res3); got != "result:expiry-test" {
		t.Fatalf("post-expiry call text=%q", got)
	}
	if n := atomic.LoadInt64(calls); n != 2 {
		t.Fatalf("post-expiry, server hit count = %d, want 2 (cache miss)", n)
	}
}

// TestMCPCache_NeverCachesSideEffecting: a write tool and a chain
// (money) tool must NEVER be cached — every call hits the server, even
// with the cache enabled and an identical arg set. This is the safety
// invariant (P2-5 mandate: NEVER cache side-effecting or money tools).
func TestMCPCache_NeverCachesSideEffecting(t *testing.T) {
	r, calls := spawnCacheRegistry(t, 5*time.Minute)

	// write tool (SideEffectWrite) — must not be cached.
	wt, err := r.Get("matrix://tool/mcp/web/write_file@0.1.0")
	if err != nil {
		t.Fatalf("Get write: %v", err)
	}
	wArgs := map[string]interface{}{"path": "/x"}
	for i := 0; i < 3; i++ {
		if _, err := wt.Call(context.Background(), wArgs); err != nil {
			t.Fatalf("write call %d: %v", i, err)
		}
	}
	if n := atomic.LoadInt64(calls); n != 3 {
		t.Fatalf("write tool: expected 3 server hits (never cached), got %d", n)
	}

	// chain (money) tool (SideEffectChain) — must not be cached.
	ct, err := r.Get("matrix://tool/mcp/web/send_value@0.1.0")
	if err != nil {
		t.Fatalf("Get chain: %v", err)
	}
	cArgs := map[string]interface{}{"to": "0xabc"}
	for i := 0; i < 3; i++ {
		if _, err := ct.Call(context.Background(), cArgs); err != nil {
			t.Fatalf("chain call %d: %v", i, err)
		}
	}
	// 3 write + 3 chain = 6 total server hits.
	if n := atomic.LoadInt64(calls); n != 6 {
		t.Fatalf("chain tool: expected 6 total server hits (never cached), got %d", n)
	}
}

// fakeClock is a test-only clock that lets tests advance time without
// sleeping, so TTL expiry is deterministic and instant.
type fakeClock struct {
	t time.Time
}

func (f *fakeClock) now() time.Time {
	return f.t
}
func (f *fakeClock) advance(d time.Duration) {
	f.t = f.t.Add(d)
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
