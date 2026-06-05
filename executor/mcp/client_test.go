// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper: build a Client wired to a fresh MockServer over a pipe transport.
func newPipeClient(t *testing.T, srv *MockServer) (*Client, func()) {
	t.Helper()
	tr := PipeMock(srv)
	c, err := NewClient(ClientParams{Transport: tr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, func() { _ = c.Close() }
}

func TestClientInitializeHandshake(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		ServerName: "test-server",
		ServerVer:  "1.2.3",
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	info, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ServerInfo.Name != "test-server" || info.ServerInfo.Version != "1.2.3" {
		t.Fatalf("unexpected server info: %+v", info.ServerInfo)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version: %s", info.ProtocolVersion)
	}

	// Wait for the post-init notification to land server-side.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !srv.Initialized() {
		time.Sleep(5 * time.Millisecond)
	}
	if !srv.Initialized() {
		t.Fatal("server never received notifications/initialized")
	}
}

func TestClientInitializeIdempotent(t *testing.T) {
	srv := NewMockServer(MockServerParams{})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	r1, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize 1: %v", err)
	}
	r2, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize 2: %v", err)
	}
	if r1 == nil || r2 == nil {
		t.Fatal("nil result")
	}
	// Cached result so second call doesn't reissue any frames.
	if got := c.Info(); got == nil {
		t.Fatal("Info() returned nil after Initialize")
	}
}

func TestClientToolsList(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{
			{Name: "fs/read", Description: "read a file"},
			{Name: "fs/write", Description: "write a file"},
		},
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	tools, err := c.ToolsList(ctx)
	if err != nil {
		t.Fatalf("ToolsList: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].Name != "fs/read" || tools[1].Name != "fs/write" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

func TestClientToolsCallText(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "echo"}},
		CallHandler: func(name string, args map[string]interface{}) (*CallToolResult, error) {
			if name != "echo" {
				return nil, errors.New("unknown tool")
			}
			msg, _ := args["msg"].(string)
			return &CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: "echo: " + msg}},
			}, nil
		},
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.ToolsCall(ctx, "echo", map[string]interface{}{"msg": "hi"})
	if err != nil {
		t.Fatalf("ToolsCall: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %+v", res)
	}
	if got := ExtractText(res); got != "echo: hi" {
		t.Fatalf("text=%q want %q", got, "echo: hi")
	}
}

func TestClientToolsCallIsErrorPropagates(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		CallHandler: func(name string, args map[string]interface{}) (*CallToolResult, error) {
			return &CallToolResult{
				IsError: true,
				Content: []Content{{Type: ContentTypeText, Text: "boom"}},
			}, nil
		},
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)
	res, err := c.ToolsCall(ctx, "any", nil)
	if err != nil {
		t.Fatalf("expected nil err for in-band failure, got %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true")
	}
}

func TestClientCallSurfacesRPCError(t *testing.T) {
	srv := NewMockServer(MockServerParams{})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)
	// Method not found.
	err := c.Call(ctx, "no/such/method", struct{}{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("expected *RPCError, got %T (%v)", err, err)
	}
	if rpc.Code != ErrCodeMethodNotFound {
		t.Fatalf("code=%d want %d", rpc.Code, ErrCodeMethodNotFound)
	}
}

func TestClientPing(t *testing.T) {
	srv := NewMockServer(MockServerParams{})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClientPingFailureSurfacesRPCError(t *testing.T) {
	srv := NewMockServer(MockServerParams{
		PingHandler: func() error { return errors.New("server overloaded") },
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)
	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server overloaded") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestClientCloseUnblocksPendingCall(t *testing.T) {
	srv := NewMockServer(MockServerParams{})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)

	// Block out a request that the server will never answer by overriding
	// the call handler to never return until close. We simulate via a
	// background goroutine that closes the client.
	stuck := make(chan error, 1)
	srvSlow := NewMockServer(MockServerParams{
		CallHandler: func(string, map[string]interface{}) (*CallToolResult, error) {
			time.Sleep(2 * time.Second)
			return &CallToolResult{}, nil
		},
	})
	c2, _ := newPipeClient(t, srvSlow)
	_, _ = c2.Initialize(ctx)
	go func() {
		stuck <- c2.Call(ctx, MethodToolsCall, CallToolParams{Name: "x"}, nil)
	}()

	// Give the call a moment to register.
	time.Sleep(50 * time.Millisecond)
	_ = c2.Close()

	select {
	case err := <-stuck:
		if err == nil {
			t.Fatal("expected error after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not unblock after Close")
	}
}

func TestClientNotificationHandler(t *testing.T) {
	got := make(chan *Notification, 1)
	srv := NewMockServer(MockServerParams{})
	tr := PipeMock(srv)
	c, err := NewClient(ClientParams{
		Transport:     tr,
		NotifyHandler: func(n *Notification) { got <- n },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// The mock doesn't emit notifications spontaneously; this path is
	// exercised end-to-end via the manager test below. Here we just
	// confirm no notifications arrive.
	select {
	case n := <-got:
		t.Fatalf("unexpected notification: %+v", n)
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}
}

func TestClientConcurrentCallsSerialized(t *testing.T) {
	// Run N concurrent ToolsCalls to verify the pending-map machinery
	// returns the right response to the right caller.
	srv := NewMockServer(MockServerParams{
		CallHandler: func(name string, args map[string]interface{}) (*CallToolResult, error) {
			tag, _ := args["tag"].(string)
			return &CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: tag}},
			}, nil
		},
	})
	c, cleanup := newPipeClient(t, srv)
	defer cleanup()

	ctx := context.Background()
	_, _ = c.Initialize(ctx)

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]string, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			r, err := c.ToolsCall(ctx, "any", map[string]interface{}{"tag": fmtInt(i)})
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			results[i] = ExtractText(r)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r != fmtInt(i) {
			t.Errorf("result[%d]=%q want %q", i, r, fmtInt(i))
		}
	}
}

func fmtInt(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
