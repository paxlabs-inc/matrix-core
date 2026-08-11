// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"matrix/executor/tool"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	neotools "matrix/neo/internal/tools"
)

func TestRepeatedNormalizedToolFailureConvergesOnRealExecBridge(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for the production exec bridge test: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatalf("curl is required for the production exec bridge test: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unknown schema"}`))
	}))
	defer server.Close()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	execBridge := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "tools", "exec", "exec.mjs"))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := tool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/neo-tool-convergence-test",
		Servers: []tool.ServerEntry{{
			Alias: "exec", Transport: "stdio", Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("b", 64), Version: "0.1.0",
			Tools: []tool.ToolEntry{
				{Name: "shell", SideEffectClass: tool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_start", SideEffectClass: tool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_list", SideEffectClass: tool.SideEffectRead, TimeoutMs: 10_000},
				{Name: "service_logs", SideEffectClass: tool.SideEffectRead, TimeoutMs: 10_000},
				{Name: "service_stop", SideEffectClass: tool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_restart", SideEffectClass: tool.SideEffectShell, TimeoutMs: 10_000},
			},
		}},
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, rawManifest, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	manager, err := neotools.Spawn(t.Context(), neotools.Options{ManifestPath: manifestPath, SpawnTimeout: 15 * time.Second, StderrSink: os.Stderr})
	if err != nil {
		t.Fatalf("spawn real tool manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if len(manager.Warnings()) != 0 {
		t.Fatalf("real exec bridge did not bind: %v", manager.Warnings())
	}

	cfg := config.Default()
	cfg.ToolDispatchConcurrency = 0
	cfg.NoProgressStall = 3
	cfg.MaxRetriesPerTool = 0
	var events []ToolEvent
	a := New(Options{Config: cfg, Tools: manager, Observer: func(event ToolEvent) {
		if event.Phase == ToolEnd {
			events = append(events, event)
		}
	}})

	for attempt := 1; attempt <= cfg.NoProgressStall; attempt++ {
		attemptText := strconv.Itoa(attempt)
		args, err := json.Marshal(map[string]interface{}{
			"command": "curl -sS " + server.URL + "/schema?attempt=" + attemptText,
			"cwd":     t.TempDir(),
			"expect":  "the endpoint rejects the guessed schema with HTTP 400",
		})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		call := llm.ToolCall{ID: "call-" + attemptText, Function: llm.FunctionCall{Name: "exec__shell", Arguments: string(args)}}
		err = a.runToolCalls(t.Context(), []llm.ToolCall{call})
		if attempt < cfg.NoProgressStall && err != nil {
			t.Fatalf("attempt %d stopped before bound: %v", attempt, err)
		}
		if attempt == cfg.NoProgressStall && !errors.Is(err, ErrIncomplete) {
			t.Fatalf("attempt %d = %v, want bounded incomplete stop", attempt, err)
		}
	}

	if len(events) != cfg.NoProgressStall {
		t.Fatalf("tool end events = %d, want %d", len(events), cfg.NoProgressStall)
	}
	for i, event := range events {
		if !event.IsErr || event.FailureClass != string(tool.FailureHTTP) || !strings.Contains(event.Result, `"http_status":400`) {
			t.Fatalf("event %d lacks normalized raw HTTP failure: %+v", i, event)
		}
	}
	if a.turn.normalizedFailureCount != cfg.NoProgressStall || a.LastFailureClass().String() != "deterministic" {
		t.Fatalf("bounded state = count %d class %s", a.turn.normalizedFailureCount, a.LastFailureClass())
	}
	last := a.working[len(a.working)-1].Content
	if !strings.Contains(last, "class=http") || strings.Contains(last, "stdout_truncated") {
		t.Fatalf("model-facing failure is not concise and typed: %q", last)
	}
}
