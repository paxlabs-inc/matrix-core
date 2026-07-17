// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"matrix/executor/mcp"
)

func TestFailureClassOfInvocationProtocolAndTransport(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailureClass
	}{
		{name: "invocation", err: ErrUnknownTool, want: FailureInvocation},
		{name: "protocol", err: &mcp.RPCError{Code: -32602, Message: "invalid params"}, want: FailureProtocol},
		{name: "transport", err: errors.New("connection reset"), want: FailureTransport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureClassOf(tc.err); got != tc.want {
				t.Fatalf("FailureClassOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestNormalizeResultRecognizedEnvelopes(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	tests := []struct {
		name      string
		text      string
		isError   bool
		wantError bool
		wantClass FailureClass
		wantExit  *int
		wantHTTP  int
		wantAppOK *bool
	}{
		{name: "application false", text: `{"ok":false,"error":"rejected"}`, wantError: true, wantClass: FailureApplication, wantAppOK: boolPtr(false)},
		{name: "error object", text: `{"error":{"code":"denied","message":"not allowed"}}`, wantError: true, wantClass: FailureApplication},
		{name: "http status", text: `{"ok":false,"http_status":503,"error":"down"}`, wantError: true, wantClass: FailureHTTP, wantHTTP: 503, wantAppOK: boolPtr(false)},
		{name: "nested shell application", text: `{"ok":true,"tool":"shell","exit_code":0,"stdout":"{\"ok\":false,\"error\":\"bad shape\"}\n","stderr":""}`, wantError: true, wantClass: FailureApplication, wantExit: intPtr(0), wantAppOK: boolPtr(false)},
		{name: "process exit", text: `{"ok":false,"tool":"shell","exit_code":7,"stdout":"","stderr":"failed"}`, wantError: true, wantClass: FailureProcess, wantExit: intPtr(7)},
		{name: "mcp isError", text: `plain protocol failure`, isError: true, wantError: true, wantClass: FailureProtocol},
		{name: "ordinary business json", text: `{"ok":true,"error_rate":0.02,"errors":[],"value":42}`, wantAppOK: boolPtr(true)},
		{name: "explicit success with informational error", text: `{"ok":true,"error":"historical field","value":42}`, wantAppOK: boolPtr(true)},
		{name: "unrecognized json", text: `{"status":"complete","value":42}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := &Result{Content: []Content{{Type: ContentTypeText, Text: tc.text}}, IsError: tc.isError}
			NormalizeResult(res)
			if res.IsError != tc.wantError || res.FailureClass != tc.wantClass {
				t.Fatalf("outcome = isError %v class %q, want %v %q", res.IsError, res.FailureClass, tc.wantError, tc.wantClass)
			}
			if !sameIntPtr(res.ProcessExitCode, tc.wantExit) || res.HTTPStatus != tc.wantHTTP || !sameBoolPtr(res.ApplicationOK, tc.wantAppOK) {
				t.Fatalf("layers = exit %v http %d app %v, want %v %d %v", res.ProcessExitCode, res.HTTPStatus, res.ApplicationOK, tc.wantExit, tc.wantHTTP, tc.wantAppOK)
			}
			if got := ExtractText(res); got != tc.text {
				t.Fatalf("raw evidence changed: %q", got)
			}
		})
	}
}

func TestExecBridgeRealProcessAndHTTPNormalization(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for the production exec bridge test: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatalf("curl is required for the production exec bridge test: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/http-error":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error":"bad request"}`))
		case "/app-error":
			_, _ = w.Write([]byte(`{"ok":false,"error":"application rejected request"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"error_rate":0.02,"value":"real-success"}`))
		}
	}))
	defer server.Close()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	execBridge := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "tools", "exec", "exec.mjs"))
	toolNames := []string{"shell", "service_start", "service_list", "service_logs", "service_stop", "service_restart"}
	mgr := mcp.NewManager(mcp.ManagerParams{})
	t.Cleanup(func() { _ = mgr.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := mgr.Spawn(ctx, mcp.ServerSpec{Alias: "exec", Transport: "stdio", Command: "node", Args: []string{execBridge}, ExpectedTools: toolNames}); err != nil {
		t.Fatalf("spawn real exec bridge: %v", err)
	}

	entries := make([]ToolEntry, 0, len(toolNames))
	for _, name := range toolNames {
		sideEffect := SideEffectShell
		if name == "service_list" || name == "service_logs" {
			sideEffect = SideEffectRead
		}
		entries = append(entries, ToolEntry{Name: name, SideEffectClass: sideEffect, TimeoutMs: 10_000})
	}
	manifest := &AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/tool-normalization-test",
		Servers: []ServerEntry{{
			Alias: "exec", Transport: "stdio", Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("a", 64), Version: "0.1.0", Tools: entries,
		}},
	}
	registry, err := NewRegistry(RegistryParams{Manifest: manifest, MCP: mgr})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	shell, err := registry.Get("matrix://tool/mcp/exec/shell@0.1.0")
	if err != nil {
		t.Fatalf("get shell: %v", err)
	}

	tests := []struct {
		name      string
		command   string
		wantError bool
		wantClass FailureClass
		wantHTTP  int
		wantExit  int
	}{
		{name: "zero-exit curl HTTP 400", command: "curl -sS " + server.URL + "/http-error", wantError: true, wantClass: FailureHTTP, wantHTTP: 400, wantExit: 22},
		{name: "HTTP 200 application error", command: "curl -sS " + server.URL + "/app-error", wantError: true, wantClass: FailureApplication, wantExit: 0},
		{name: "nonzero process", command: "printf process-failed >&2; exit 7", wantError: true, wantClass: FailureProcess, wantExit: 7},
		{name: "genuine success", command: "curl -sS " + server.URL + "/success", wantExit: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := shell.Call(ctx, map[string]interface{}{"command": tc.command, "cwd": t.TempDir()})
			if err != nil {
				t.Fatalf("real shell call: %v", err)
			}
			if res.IsError != tc.wantError || res.FailureClass != tc.wantClass {
				t.Fatalf("outcome = isError %v class %q message %q; want %v %q", res.IsError, res.FailureClass, res.FailureMessage, tc.wantError, tc.wantClass)
			}
			if res.ProcessExitCode == nil || *res.ProcessExitCode != tc.wantExit || res.HTTPStatus != tc.wantHTTP {
				t.Fatalf("layers = exit %v http %d; want %d %d; raw=%s", res.ProcessExitCode, res.HTTPStatus, tc.wantExit, tc.wantHTTP, ExtractText(res))
			}
			if tc.wantError && strings.TrimSpace(res.FailureMessage) == "" {
				t.Fatal("failed result lacks concise failure message")
			}
			if tc.name == "genuine success" && !strings.Contains(ExtractText(res), "real-success") {
				t.Fatalf("real successful result was not preserved: %s", ExtractText(res))
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func sameIntPtr(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameBoolPtr(a, b *bool) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
