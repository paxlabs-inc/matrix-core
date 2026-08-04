// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	neotools "matrix/neo/internal/tools"
)

func TestResurrectionIncidentToolSessionDeath_HeartbeatKeepsLiveBrowserCall(
	t *testing.T,
) {
	const sessionID = "resurrection-browser-session"
	heartbeatAnswered := make(chan struct{})
	var heartbeatOnce sync.Once
	browser := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.Method {
			case http.MethodGet:
				if request.Header.Get("Mcp-Session-Id") != sessionID {
					http.Error(
						writer, "missing browser session",
						http.StatusBadRequest,
					)
					return
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(
					writer,
					"data: {\"jsonrpc\":\"2.0\",\"id\":\"browser-heartbeat\",\"method\":\"ping\"}\n\n",
				)
				writer.(http.Flusher).Flush()
				select {
				case <-heartbeatAnswered:
				case <-request.Context().Done():
				}
				return
			case http.MethodPost:
			default:
				http.Error(
					writer, "method not allowed",
					http.StatusMethodNotAllowed,
				)
				return
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			var frame struct {
				ID     interface{} `json:"id"`
				Method string      `json:"method"`
			}
			if err := json.Unmarshal(body, &frame); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			switch frame.Method {
			case "initialize":
				writer.Header().Set("Mcp-Session-Id", sessionID)
				writeIncidentBrowserJSON(writer, frame.ID, map[string]interface{}{
					"protocolVersion": "2024-11-05",
					"serverInfo": map[string]interface{}{
						"name": "incident-browser", "version": "1",
					},
					"capabilities": map[string]interface{}{
						"tools": map[string]interface{}{},
					},
				})
			case "notifications/initialized":
				writer.WriteHeader(http.StatusAccepted)
			case "tools/call":
				select {
				case <-heartbeatAnswered:
				case <-time.After(4 * time.Second):
					http.Error(
						writer, "browser heartbeat was not answered",
						http.StatusGatewayTimeout,
					)
					return
				}
				writeIncidentBrowserJSON(writer, frame.ID, map[string]interface{}{
					"content": []map[string]interface{}{{
						"type": "text",
						"text": "browser session stayed alive through the call",
					}},
					"isError": false,
				})
			case "":
				if frame.ID == "browser-heartbeat" {
					heartbeatOnce.Do(func() {
						close(heartbeatAnswered)
					})
					writer.WriteHeader(http.StatusAccepted)
					return
				}
				http.Error(writer, "unknown response", http.StatusBadRequest)
			default:
				http.Error(writer, "unknown method", http.StatusBadRequest)
			}
		},
	))
	t.Cleanup(browser.Close)
	t.Setenv("MATRIX_BROWSER_URL", browser.URL)
	t.Setenv("MATRIX_BROWSER_TIMEOUT_MS", "10000")

	spawnContext, cancelSpawn := context.WithTimeout(
		t.Context(), 20*time.Second,
	)
	defer cancelSpawn()
	manager, err := neotools.Spawn(spawnContext, neotools.Options{
		ManifestPath: incidentBrowserManifest(t),
		SpawnTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close browser manager: %v", err)
		}
	})
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("browser manager warnings = %v", warnings)
	}

	var step int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			step++
			if step == 1 {
				writeSSETool(
					writer, "browser-call", "browser__browser_navigate",
					map[string]interface{}{
						"url":    "https://example.com",
						"expect": "reports the loaded page",
					},
				)
				return
			}
			writeSSEText(
				writer,
				"The browser session stayed alive and returned the page report.",
			)
		},
	))
	t.Cleanup(gateway.Close)

	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter,
		realTurnStore(
			t, "incident-browser-heartbeat",
			"Load the page and report whether the browser stayed alive.",
		),
		Config{
			TurnID: "incident-browser-heartbeat", Model: "mimo-v2",
			IdleTimeout: 15 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(
		t.Context(),
		"Load the page and report whether the browser stayed alive.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Content !=
		"The browser session stayed alive and returned the page report." ||
		len(response.ToolEvents) != 1 ||
		response.ToolEvents[0].Error != "" ||
		!strings.Contains(
			string(response.ToolEvents[0].Result),
			"browser session stayed alive",
		) {
		t.Fatalf("browser runtime response = %+v", response)
	}
	select {
	case <-heartbeatAnswered:
	default:
		t.Fatal("browser bridge never answered the session heartbeat")
	}
}

func incidentBrowserManifest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean("../../../../agents/neo.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var servers []map[string]json.RawMessage
	if err := json.Unmarshal(document["servers"], &servers); err != nil {
		t.Fatal(err)
	}
	var browser map[string]json.RawMessage
	for _, server := range servers {
		var alias string
		_ = json.Unmarshal(server["alias"], &alias)
		if alias == "browser" {
			browser = server
			break
		}
	}
	if browser == nil {
		t.Fatal("browser server missing from Neo manifest")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve incident browser test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(
		filepath.Dir(sourceFile), "..", "..", "..", "..",
	))
	browserArgs, err := json.Marshal([]string{
		filepath.Join(repositoryRoot, "tools", "browser", "browser.mjs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	browser["args"] = browserArgs
	onlyBrowser, err := json.Marshal(
		[]map[string]json.RawMessage{browser},
	)
	if err != nil {
		t.Fatal(err)
	}
	document["servers"] = onlyBrowser
	filtered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "browser-only.json")
	if err := os.WriteFile(path, filtered, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeIncidentBrowserJSON(
	writer http.ResponseWriter,
	id interface{},
	result interface{},
) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}
