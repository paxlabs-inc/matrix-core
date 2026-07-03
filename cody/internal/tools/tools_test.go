// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetch drives the real fetch bridge against an httptest origin.
func TestFetch(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from origin"))
	}))
	defer origin.Close()
	b := New(Config{})
	out, err := b.Dispatch(context.Background(), "fetch", map[string]interface{}{"url": origin.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello from origin") || !strings.Contains(out, "HTTP 200") {
		t.Fatalf("fetch output = %q", out)
	}
	if _, err := b.Dispatch(context.Background(), "fetch", map[string]interface{}{"url": "not-a-url"}); err == nil {
		t.Fatal("fetch accepted a non-absolute URL")
	}
}

// TestWebSearch proves the not-configured (boot-safe) path and the real SearXNG
// JSON path against an httptest backend.
func TestWebSearch(t *testing.T) {
	unconfigured := New(Config{})
	out, err := unconfigured.Dispatch(context.Background(), "web_search", map[string]interface{}{"query": "cody"})
	if err != nil || !strings.Contains(out, "not configured") {
		t.Fatalf("unconfigured web_search = %q, %v", out, err)
	}

	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]string{{"title": "Cody", "url": "https://cody.dev", "content": "the coding agent"}},
		})
	}))
	defer searx.Close()
	b := New(Config{SearxngURL: searx.URL})
	out, err = b.Dispatch(context.Background(), "web_search", map[string]interface{}{"query": "cody"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Cody") || !strings.Contains(out, "https://cody.dev") {
		t.Fatalf("web_search output = %q", out)
	}
}

// fakeMCPBrowser answers the MCP Streamable-HTTP handshake and returns a real
// (tiny) PNG for browser_take_screenshot — proving the browser bridge saves a
// genuine artifact into the workspace (req 13.1/13.2), not a fabricated path.
func fakeMCPBrowser(t *testing.T, pngB64 string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "initialize":
			json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"protocolVersion":"2025-03-26"}`)})
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			b, _ := json.Marshal(req.Params)
			json.Unmarshal(b, &p)
			if p.Name == "browser_take_screenshot" {
				result, _ := json.Marshal(toolCallResult{Content: []struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					Data     string `json:"data,omitempty"`
					MimeType string `json:"mimeType,omitempty"`
				}{{Type: "image", Data: pngB64, MimeType: "image/png"}}})
				json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
				return
			}
			json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[]}`)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestBrowserScreenshotSavesRealArtifact(t *testing.T) {
	// A 1x1 transparent PNG.
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	srv := fakeMCPBrowser(t, png)
	defer srv.Close()

	root := t.TempDir()
	b := New(Config{Root: root, BrowserURL: srv.URL})

	// The tool is advertised only when a browser is configured.
	sawTool := false
	for _, tool := range b.Tools() {
		if tool.Function.Name == "browser_screenshot" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("browser_screenshot tool not advertised with a configured browser")
	}

	out, err := b.Dispatch(context.Background(), "browser_screenshot", map[string]interface{}{
		"url": "http://localhost:3000", "name": "hero",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hero.png") {
		t.Fatalf("screenshot output = %q", out)
	}
	// The artifact is a REAL, non-empty file matching the captured PNG bytes.
	saved := filepath.Join(root, screenshotDir, "hero.png")
	data, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("screenshot not saved: %v", err)
	}
	want, _ := base64.StdEncoding.DecodeString(png)
	if len(data) == 0 || string(data) != string(want) {
		t.Fatalf("saved screenshot bytes do not match the captured image (%d bytes)", len(data))
	}

	// Boot-safe: no browser configured → structured message, no fabricated file.
	nob := New(Config{Root: root})
	out, err = nob.Dispatch(context.Background(), "browser_screenshot", map[string]interface{}{"url": "http://x"})
	if err != nil || !strings.Contains(out, "not configured") {
		t.Fatalf("unconfigured browser = %q, %v", out, err)
	}
}
