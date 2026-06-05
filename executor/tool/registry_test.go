// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"matrix/executor/mcp"
)

// helperManifest builds a manifest with one fs MCP server and two tools.
func helperManifest(allowedSideEffects []string) *AgentManifest {
	dig := "sha256:" + strings.Repeat("a", 64)
	m := &AgentManifest{
		SchemaVersion:      1,
		Agent:              "matrix://agent/test",
		AllowedSideEffects: append([]string(nil), allowedSideEffects...),
		Servers: []ServerEntry{
			{
				Alias:         "fs",
				Transport:     "stdio",
				Command:       "fake",
				Version:       "2024.11.1",
				PackageDigest: dig,
				Tools: []ToolEntry{
					{Name: "read", SideEffectClass: SideEffectRead, TimeoutMs: 1000},
					{Name: "write", SideEffectClass: SideEffectWrite},
				},
			},
		},
	}
	return m
}

// pipeMgr builds a Manager wired to a MockServer for tests.
func pipeMgr(t *testing.T, mock *mcp.MockServer) *mcp.Manager {
	t.Helper()
	mgr := mcp.NewManager(mcp.ManagerParams{
		TransportBuilder: func(spec mcp.ServerSpec) (mcp.Transport, error) {
			return mcp.PipeMock(mock), nil
		},
	})
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

func TestRegistryGetMCPTool(t *testing.T) {
	mock := mcp.NewMockServer(mcp.MockServerParams{
		Tools: []mcp.Tool{{Name: "read"}, {Name: "write"}},
		CallHandler: func(name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: name + " ok"}},
			}, nil
		},
	})
	mgr := pipeMgr(t, mock)
	if _, err := mgr.Spawn(context.Background(), mcp.ServerSpec{
		Alias: "fs", Transport: "stdio", ExpectedTools: []string{"read", "write"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	r, err := NewRegistry(RegistryParams{
		Manifest: helperManifest(nil),
		MCP:      mgr,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tl, err := r.Get("matrix://tool/mcp/fs/read@2024.11.1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tl.SideEffectClass() != SideEffectRead {
		t.Fatalf("side-effect: %q", tl.SideEffectClass())
	}

	res, err := tl.Call(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if ExtractText(res) != "read ok" {
		t.Fatalf("text=%q", ExtractText(res))
	}
	if res.CallID == "" {
		t.Fatal("expected CallID")
	}
}

func TestRegistryGatesSideEffects(t *testing.T) {
	mgr := mcp.NewManager(mcp.ManagerParams{}) // no servers spawned
	defer mgr.Close()

	r, err := NewRegistry(RegistryParams{
		Manifest: helperManifest([]string{SideEffectRead}),
		MCP:      mgr,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, err := r.Get("matrix://tool/mcp/fs/read@2024.11.1"); err != nil {
		t.Fatalf("read should be allowed: %v", err)
	}
	_, err = r.Get("matrix://tool/mcp/fs/write@2024.11.1")
	if !errors.Is(err, ErrSideEffectDenied) {
		t.Fatalf("expected ErrSideEffectDenied, got %v", err)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	mgr := mcp.NewManager(mcp.ManagerParams{})
	defer mgr.Close()
	r, err := NewRegistry(RegistryParams{Manifest: helperManifest(nil), MCP: mgr})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = r.Get("matrix://tool/mcp/fs/exec@2024.11.1")
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
}

func TestRegistryUnpinnedURI(t *testing.T) {
	mgr := mcp.NewManager(mcp.ManagerParams{})
	defer mgr.Close()
	r, _ := NewRegistry(RegistryParams{Manifest: helperManifest(nil), MCP: mgr})
	_, err := r.Get("matrix://tool/mcp/fs/read")
	if !errors.Is(err, ErrUnpinnedTool) {
		t.Fatalf("expected ErrUnpinnedTool, got %v", err)
	}
}

func TestRegistryListSorted(t *testing.T) {
	mgr := mcp.NewManager(mcp.ManagerParams{})
	defer mgr.Close()
	r, _ := NewRegistry(RegistryParams{Manifest: helperManifest(nil), MCP: mgr})
	got := r.List()
	want := []string{
		"matrix://tool/mcp/fs/read@2024.11.1",
		"matrix://tool/mcp/fs/write@2024.11.1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d]=%q want %q", i, got[i], w)
		}
	}
}

func TestRegistryNativeToolPlaceholder(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	m := &AgentManifest{
		SchemaVersion: 1,
		Agent:         "x",
		NativeTools: []NativeToolEntry{
			{
				Namespace:       "argus",
				Name:            "place_order",
				Version:         "v0.1.0",
				Digest:          dig,
				SideEffectClass: SideEffectChain,
			},
		},
	}
	r, err := NewRegistry(RegistryParams{Manifest: m})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tl, err := r.Get("matrix://tool/argus/place_order@v0.1.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tl.SideEffectClass() != SideEffectChain {
		t.Fatalf("side-effect=%q", tl.SideEffectClass())
	}
	_, err = tl.Call(context.Background(), nil)
	if !errors.Is(err, ErrNativeToolNotImplemented) {
		t.Fatalf("expected ErrNativeToolNotImplemented, got %v", err)
	}
}

func TestRegistryRequiresManifest(t *testing.T) {
	if _, err := NewRegistry(RegistryParams{}); err == nil {
		t.Fatal("expected error for nil manifest")
	}
}

func TestRegistryMCPCallTimesOut(t *testing.T) {
	mock := mcp.NewMockServer(mcp.MockServerParams{
		Tools: []mcp.Tool{{Name: "read"}, {Name: "write"}},
		CallHandler: func(name string, _ map[string]interface{}) (*mcp.CallToolResult, error) {
			time.Sleep(200 * time.Millisecond)
			return &mcp.CallToolResult{}, nil
		},
	})
	mgr := pipeMgr(t, mock)
	if _, err := mgr.Spawn(context.Background(), mcp.ServerSpec{
		Alias: "fs", Transport: "stdio", ExpectedTools: []string{"read", "write"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Override the manifest with a 50ms tool timeout so the call gets
	// chopped before the mock returns.
	dig := "sha256:" + strings.Repeat("a", 64)
	m := &AgentManifest{
		SchemaVersion: 1,
		Agent:         "x",
		Servers: []ServerEntry{{
			Alias: "fs", Transport: "stdio", Command: "fake",
			Version: "2024.11.1", PackageDigest: dig,
			Tools: []ToolEntry{{Name: "read", SideEffectClass: SideEffectRead, TimeoutMs: 50}},
		}},
	}
	r, err := NewRegistry(RegistryParams{Manifest: m, MCP: mgr})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tl, _ := r.Get("matrix://tool/mcp/fs/read@2024.11.1")
	_, err = tl.Call(context.Background(), nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
