// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"matrix/neo/internal/mcpcontrol"
	neotools "matrix/neo/internal/tools"
)

func TestMCPControlRealBridgeAndBetweenTurnActivation(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "tools", "chronos", "chronos.mjs")
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := mcpcontrol.CommandDigest(node, []string{bridge})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"agent":"neo:mcp-control-test","servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(context.Background(), neotools.Options{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	engine := NewEngine(EngineOptions{Tools: manager, MCPControlDir: t.TempDir()})
	defer engine.Close()
	srv, err := New(engine, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	created := requestMCP(t, srv, http.MethodPost, "/integrations/mcp", map[string]any{
		"config": map[string]any{
			"alias": "route-chronos", "display_name": "Route Chronos", "transport": "stdio",
			"command": node, "args": []string{bridge}, "package_digest": digest, "version": "real-route-test",
		},
	}, http.StatusCreated)
	if created["state"] != string(mcpcontrol.StateCandidate) {
		t.Fatalf("created=%v", created)
	}
	probed := requestMCP(t, srv, http.MethodPost, "/integrations/mcp/route-chronos/probe", map[string]any{}, http.StatusOK)
	serverMap := probed["server"].(map[string]any)
	toolValues := serverMap["tools"].([]any)
	classifications := make([]map[string]any, 0, len(toolValues))
	for _, raw := range toolValues {
		item := raw.(map[string]any)
		name := item["name"].(string)
		classifications = append(classifications, map[string]any{
			"name": name, "effect_class": map[bool]string{true: "read", false: "write"}[name == "alarm_list"],
			"enabled": name == "alarm_list",
		})
	}
	requestMCP(t, srv, http.MethodPost, "/integrations/mcp/route-chronos/classify", map[string]any{"tools": classifications}, http.StatusOK)
	enabled := requestMCP(t, srv, http.MethodPost, "/integrations/mcp/route-chronos/enable", map[string]any{}, http.StatusOK)
	if enabled["applied"] != true || !hasSchema(manager, "route-chronos__alarm_list") {
		t.Fatalf("enable did not atomically publish at idle boundary: %v", enabled)
	}

	engine.registerRun(&run{id: "mcp-boundary-live", convID: "mcp-boundary-conversation"})
	disabled := requestMCP(t, srv, http.MethodPost, "/integrations/mcp/route-chronos/disable", map[string]any{}, http.StatusOK)
	if disabled["applied"] != false || !hasSchema(manager, "route-chronos__alarm_list") {
		t.Fatalf("live turn observed a mid-turn capability change: %v", disabled)
	}
	engine.unregisterRun("mcp-boundary-live")
	applied, err := engine.applyMCP(context.Background())
	if err != nil || !applied {
		t.Fatalf("between-turn apply=%t err=%v", applied, err)
	}
	if hasSchema(manager, "route-chronos__alarm_list") {
		t.Fatal("disabled MCP tool remained in the next turn inventory")
	}
}

func requestMCP(t *testing.T, server *Server, method, path string, body any, status int) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	server.handleMCPControl(recorder, request)
	if recorder.Code != status {
		t.Fatalf("%s %s status=%d body=%s", method, path, recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() == 0 {
		return nil
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func hasSchema(manager *neotools.Manager, name string) bool {
	for _, schema := range manager.Schemas() {
		if schema.Function.Name == name {
			return true
		}
	}
	return false
}
