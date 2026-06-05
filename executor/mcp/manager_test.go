// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pipeBuilder constructs a transport hook for manager tests that maps
// each spec.Alias to a corresponding MockServer.
func pipeBuilder(servers map[string]*MockServer) func(spec ServerSpec) (Transport, error) {
	return func(spec ServerSpec) (Transport, error) {
		s, ok := servers[spec.Alias]
		if !ok {
			return nil, errors.New("no mock for alias " + spec.Alias)
		}
		return PipeMock(s), nil
	}
}

func TestManagerSpawnAndUse(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "fs/read"}, {Name: "fs/write"}},
		CallHandler: func(name string, _ map[string]interface{}) (*CallToolResult, error) {
			return &CallToolResult{Content: []Content{{Type: ContentTypeText, Text: name}}}, nil
		},
		ServerName: "fs-mcp",
	})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"fs": srv}),
	})
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	spec := ServerSpec{
		Alias:         "fs",
		Transport:     "stdio",
		ExpectedTools: []string{"fs/read", "fs/write"},
	}
	c, err := mgr.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}

	// Idempotent re-spawn returns the same client.
	c2, err := mgr.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("re-Spawn: %v", err)
	}
	if c != c2 {
		t.Fatal("re-Spawn returned different client")
	}

	if got := mgr.Aliases(); len(got) != 1 || got[0] != "fs" {
		t.Fatalf("Aliases: %v", got)
	}

	r, err := c.ToolsCall(ctx, "fs/read", nil)
	if err != nil {
		t.Fatalf("ToolsCall: %v", err)
	}
	if got := ExtractText(r); got != "fs/read" {
		t.Fatalf("text=%q", got)
	}
}

func TestManagerVerifyToolsRejectsMissing(t *testing.T) {
	// Server advertises only fs/read but manifest expects fs/read + fs/write.
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "fs/read"}},
	})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"fs": srv}),
	})
	defer mgr.Close()

	ctx := context.Background()
	_, err := mgr.Spawn(ctx, ServerSpec{
		Alias:         "fs",
		Transport:     "stdio",
		ExpectedTools: []string{"fs/read", "fs/write"},
	})
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
	if !strings.Contains(err.Error(), "missing expected tool") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManagerVerifyToolsRejectsExtra(t *testing.T) {
	// Server advertises an extra tool beyond what the manifest declared.
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "fs/read"}, {Name: "fs/write"}, {Name: "fs/exec"}},
	})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"fs": srv}),
	})
	defer mgr.Close()

	ctx := context.Background()
	_, err := mgr.Spawn(ctx, ServerSpec{
		Alias:         "fs",
		Transport:     "stdio",
		ExpectedTools: []string{"fs/read", "fs/write"},
	})
	if err == nil {
		t.Fatal("expected drift error for extra tool")
	}
	if !strings.Contains(err.Error(), "unexpected tool") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManagerVerifyToolsSkippedWhenManifestEmpty(t *testing.T) {
	// Empty ExpectedTools opts into dynamic discovery (deferred at v1
	// but the path passes through).
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "anything"}},
	})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"fs": srv}),
	})
	defer mgr.Close()

	ctx := context.Background()
	if _, err := mgr.Spawn(ctx, ServerSpec{
		Alias:     "fs",
		Transport: "stdio",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
}

func TestManagerStop(t *testing.T) {
	srv := NewMockServer(MockServerParams{Tools: []Tool{{Name: "x"}}})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"a": srv}),
	})
	defer mgr.Close()

	ctx := context.Background()
	if _, err := mgr.Spawn(ctx, ServerSpec{
		Alias: "a", Transport: "stdio", ExpectedTools: []string{"x"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := mgr.Stop("a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if mgr.Client("a") != nil {
		t.Fatal("client still registered after Stop")
	}
	// Stop on missing alias is a no-op.
	if err := mgr.Stop("nope"); err != nil {
		t.Fatalf("Stop missing: %v", err)
	}
}

func TestManagerHealthLoopFiresOnUnhealthy(t *testing.T) {
	pingErr := errors.New("simulated ping failure")
	srv := NewMockServer(MockServerParams{
		Tools:       []Tool{{Name: "x"}},
		PingHandler: func() error { return pingErr },
	})
	unhealthy := make(chan string, 4)
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"a": srv}),
		HealthInterval:   30 * time.Millisecond,
		OnUnhealthy:      func(alias string, err error) { unhealthy <- alias },
	})
	defer mgr.Close()

	ctx := context.Background()
	if _, err := mgr.Spawn(ctx, ServerSpec{
		Alias: "a", Transport: "stdio", ExpectedTools: []string{"x"},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case got := <-unhealthy:
		if got != "a" {
			t.Fatalf("alias=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("unhealthy callback never fired")
	}
}

func TestManagerCloseStopsAll(t *testing.T) {
	srvA := NewMockServer(MockServerParams{Tools: []Tool{{Name: "x"}}})
	srvB := NewMockServer(MockServerParams{Tools: []Tool{{Name: "y"}}})
	mgr := NewManager(ManagerParams{
		TransportBuilder: pipeBuilder(map[string]*MockServer{"a": srvA, "b": srvB}),
	})

	ctx := context.Background()
	if _, err := mgr.Spawn(ctx, ServerSpec{
		Alias: "a", Transport: "stdio", ExpectedTools: []string{"x"},
	}); err != nil {
		t.Fatalf("Spawn a: %v", err)
	}
	if _, err := mgr.Spawn(ctx, ServerSpec{
		Alias: "b", Transport: "stdio", ExpectedTools: []string{"y"},
	}); err != nil {
		t.Fatalf("Spawn b: %v", err)
	}
	if got := mgr.Aliases(); len(got) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(got))
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := mgr.Aliases(); len(got) != 0 {
		t.Fatalf("aliases after Close: %v", got)
	}
}

func TestManagerRequiresAlias(t *testing.T) {
	mgr := NewManager(ManagerParams{})
	defer mgr.Close()
	_, err := mgr.Spawn(context.Background(), ServerSpec{})
	if err == nil || !strings.Contains(err.Error(), "empty alias") {
		t.Fatalf("expected empty-alias error, got %v", err)
	}
}

func TestManagerStdioRejectsEmptyCommand(t *testing.T) {
	mgr := NewManager(ManagerParams{})
	defer mgr.Close()
	_, err := mgr.Spawn(context.Background(), ServerSpec{
		Alias:     "x",
		Transport: "stdio",
	})
	if err == nil || !strings.Contains(err.Error(), "Command") {
		t.Fatalf("expected Command-required error, got %v", err)
	}
}

func TestManagerHTTPRejectsEmptyEndpoint(t *testing.T) {
	mgr := NewManager(ManagerParams{})
	defer mgr.Close()
	_, err := mgr.Spawn(context.Background(), ServerSpec{
		Alias:     "x",
		Transport: "http",
	})
	if err == nil || !strings.Contains(err.Error(), "Endpoint") {
		t.Fatalf("expected Endpoint-required error, got %v", err)
	}
}

func TestManagerRejectsUnknownTransport(t *testing.T) {
	mgr := NewManager(ManagerParams{})
	defer mgr.Close()
	_, err := mgr.Spawn(context.Background(), ServerSpec{
		Alias:     "x",
		Transport: "websocket",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("expected unsupported-transport error, got %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
