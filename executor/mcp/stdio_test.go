// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// stdioFakeServerMode is enabled when this binary is invoked recursively
// to simulate an MCP server over stdio (see TestStdioWithFakeServer
// below). Triggered by env var MATRIX_MCP_FAKE_SERVER=1.
const fakeServerEnv = "MATRIX_MCP_FAKE_SERVER"

// TestMain hooks into Go's test entry point so we can re-exec the test
// binary as a fake stdio MCP server when the env var is set.
func TestMain(m *testing.M) {
	flag.Parse()
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

// runFakeServer is the trivial stdio MCP server used by TestStdioWith
// FakeServer. Reads one JSON-RPC frame per line from stdin, dispatches
// initialize/tools/list/tools/call/ping, writes responses to stdout.
func runFakeServer() {
	srv := NewMockServer(MockServerParams{
		Tools: []Tool{{Name: "echo"}},
		CallHandler: func(name string, args map[string]interface{}) (*CallToolResult, error) {
			msg, _ := args["msg"].(string)
			return &CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: msg}},
			}, nil
		},
		ServerName: "fake-stdio",
		ServerVer:  "1.0.0",
	})
	r := bufio.NewReader(os.Stdin)
	w := os.Stdout
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// Trim trailing newline.
			for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				resp, herr := srv.handle(line)
				if herr == nil && len(resp) > 0 {
					_, _ = w.Write(resp)
					_, _ = w.Write([]byte("\n"))
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				_, _ = os.Stderr.WriteString("fake server read err: " + err.Error() + "\n")
			}
			return
		}
	}
}

func TestStdioWithFakeServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Discard subprocess stderr to avoid leaking go test framework
	// chatter into our output.
	tr, err := NewStdioTransport(StdioParams{
		Command: exe,
		Args:    []string{"-test.run=TestMain"},
		Env:     append(os.Environ(), fakeServerEnv+"=1"),
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}

	c, err := NewClient(ClientParams{Transport: tr})
	if err != nil {
		_ = tr.Close()
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ServerInfo.Name != "fake-stdio" {
		t.Fatalf("server name=%q", info.ServerInfo.Name)
	}

	tools, err := c.ToolsList(ctx)
	if err != nil {
		t.Fatalf("ToolsList: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%+v", tools)
	}

	res, err := c.ToolsCall(ctx, "echo", map[string]interface{}{"msg": "hello-stdio"})
	if err != nil {
		t.Fatalf("ToolsCall: %v", err)
	}
	if got := ExtractText(res); got != "hello-stdio" {
		t.Fatalf("text=%q", got)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestStdioRequiresCommand(t *testing.T) {
	_, err := NewStdioTransport(StdioParams{})
	if err == nil {
		t.Fatal("expected error on empty command")
	}
}

func TestStdioRejectsBadCommand(t *testing.T) {
	_, err := NewStdioTransport(StdioParams{
		Command: "/nonexistent/binary/path/" + strings.Repeat("x", 8),
	})
	if err == nil {
		t.Fatal("expected error spawning nonexistent binary")
	}
}

func TestStdioCloseIsIdempotent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no test executable")
	}
	tr, err := NewStdioTransport(StdioParams{
		Command: exe,
		Args:    []string{"-test.run=TestMain"},
		Env:     append(os.Environ(), fakeServerEnv+"=1"),
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

func TestStdioRecvAfterCloseReturnsErrClosed(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no test executable")
	}
	tr, err := NewStdioTransport(StdioParams{
		Command: exe,
		Args:    []string{"-test.run=TestMain"},
		Env:     append(os.Environ(), fakeServerEnv+"=1"),
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}
	_ = tr.Close()

	_, err = tr.Recv(context.Background())
	if err != ErrClosed {
		t.Fatalf("got %v, want ErrClosed", err)
	}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != ErrClosed {
		t.Fatalf("Send post-close: got %v, want ErrClosed", err)
	}
}

// helper: ensure a JSON-RPC frame can round-trip through stdio.
func TestStdioFrameRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no test executable")
	}
	tr, err := NewStdioTransport(StdioParams{
		Command: exe,
		Args:    []string{"-test.run=TestMain"},
		Env:     append(os.Environ(), fakeServerEnv+"=1"),
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}
	defer tr.Close()

	ctx := context.Background()
	frame, err := EncodeRequest(&Request{
		ID:     NewIDInt(1),
		Method: MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := tr.Send(ctx, frame); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := tr.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	kind, _, r, _, err := Classify(resp)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if kind != KindResponse || r.Error != nil {
		t.Fatalf("unexpected response: kind=%v err=%v", kind, r.Error)
	}
}

// silence unused import (exec) in some build modes.
var _ = exec.Command

// Copyright © 2026 Paxlabs Inc. All rights reserved.
