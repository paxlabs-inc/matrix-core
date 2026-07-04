// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The browser bridge speaks the MCP Streamable-HTTP transport to the shared
// browser service (@playwright/mcp behind MATRIX_BROWSER_URL): a JSON-RPC 2.0
// handshake (initialize -> notifications/initialized) then tools/call for
// browser_navigate and browser_take_screenshot. The screenshot's image content
// block is base64-decoded and written to a REAL file under the workspace so the
// worker can hand it to the acceptance gate as UI evidence (req 13.1, 13.2). If
// the service is unreachable the call errors honestly — it never fabricates a
// screenshot file.

// screenshotDir is where captured screenshots land under the workspace.
const screenshotDir = ".cody/screenshots"

// jsonRPCRequest is one JSON-RPC 2.0 call.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 reply (result or error).
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// toolCallResult is the MCP tools/call result: content blocks, one of which is
// the screenshot image for browser_take_screenshot.
type toolCallResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Data     string `json:"data,omitempty"` // base64 image (type == "image")
		MimeType string `json:"mimeType,omitempty"`
	} `json:"content"`
	IsError bool `json:"isError,omitempty"`
}

// browserScreenshot navigates the shared browser to url and saves a screenshot
// under the workspace, returning the workspace-relative path.
func (b *Bridge) browserScreenshot(ctx context.Context, target, name string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("url is required")
	}
	if b.cfg.BrowserURL == "" {
		return "the browser service is not configured; cannot capture a screenshot.", nil
	}
	sess, err := b.mcpInitialize(ctx)
	if err != nil {
		return "", fmt.Errorf("browser: initialize: %w", err)
	}
	if _, err := b.mcpToolCall(ctx, sess, "browser_navigate", map[string]interface{}{"url": target}); err != nil {
		return "", fmt.Errorf("browser: navigate: %w", err)
	}
	res, err := b.mcpToolCall(ctx, sess, "browser_take_screenshot", map[string]interface{}{})
	if err != nil {
		return "", fmt.Errorf("browser: screenshot: %w", err)
	}
	var img []byte
	for _, c := range res.Content {
		if c.Type == "image" && c.Data != "" {
			if raw, derr := base64.StdEncoding.DecodeString(c.Data); derr == nil {
				img = raw
				break
			}
		}
	}
	if len(img) == 0 {
		return "", fmt.Errorf("browser: the screenshot response carried no image data")
	}
	rel, err := b.saveScreenshot(name, target, img)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("screenshot saved to %s (%d bytes). Pass this path to turn_in as screenshot evidence.", rel, len(img)), nil
}

// saveScreenshot writes the PNG under <root>/.cody/screenshots and returns its
// workspace-relative path.
func (b *Bridge) saveScreenshot(name, target string, img []byte) (string, error) {
	dir := filepath.Join(b.cfg.Root, screenshotDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := sanitizeName(name)
	if base == "" {
		sum := sha256.Sum256([]byte(target))
		base = "shot-" + hex.EncodeToString(sum[:6])
	}
	rel := filepath.ToSlash(filepath.Join(screenshotDir, base+".png"))
	if err := os.WriteFile(filepath.Join(b.cfg.Root, filepath.FromSlash(rel)), img, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// mcpSession carries the negotiated session id (if the server issued one).
type mcpSession struct{ id string }

// mcpInitialize performs the MCP handshake and returns the session.
func (b *Bridge) mcpInitialize(ctx context.Context) (*mcpSession, error) {
	resp, sessID, err := b.rpc(ctx, "", jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "codyd", "version": "1"},
		},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("initialize error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	sess := &mcpSession{id: sessID}
	// Best-effort initialized notification (no id, no response expected).
	_, _, _ = b.rpc(ctx, sess.id, jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	return sess, nil
}

// mcpToolCall invokes one MCP tool and decodes the tools/call result.
func (b *Bridge) mcpToolCall(ctx context.Context, sess *mcpSession, name string, args map[string]interface{}) (*toolCallResult, error) {
	resp, _, err := b.rpc(ctx, sess.id, jsonRPCRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call",
		Params: map[string]interface{}{"name": name, "arguments": args},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tool %s error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}
	var out toolCallResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			return nil, fmt.Errorf("tool %s: decode result: %w", name, err)
		}
	}
	if out.IsError {
		return nil, fmt.Errorf("tool %s reported an error", name)
	}
	return &out, nil
}

// rpc POSTs one JSON-RPC message to the browser endpoint and returns the first
// matching response. The Streamable-HTTP transport may answer with a single
// JSON body or an SSE stream of framed messages; both are handled. It returns
// the (possibly empty) session id echoed by the server. A notification (no id)
// expects no body and returns a zero response.
func (b *Bridge) rpc(ctx context.Context, sessID string, msg jsonRPCRequest) (jsonRPCResponse, string, error) {
	payload, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.BrowserURL, bytes.NewReader(payload))
	if err != nil {
		return jsonRPCResponse{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.cfg.BrowserToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.BrowserToken)
	}
	if sessID != "" {
		req.Header.Set("Mcp-Session-Id", sessID)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return jsonRPCResponse{}, "", err
	}
	defer resp.Body.Close()
	outSess := resp.Header.Get("Mcp-Session-Id")
	if outSess == "" {
		outSess = sessID
	}
	// A notification (no id) returns 202/204 with no JSON-RPC body.
	if msg.ID == 0 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return jsonRPCResponse{}, outSess, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return jsonRPCResponse{}, outSess, fmt.Errorf("browser HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		out, err := readSSEResponse(resp.Body, msg.ID)
		return out, outSess, err
	}
	var out jsonRPCResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return jsonRPCResponse{}, outSess, fmt.Errorf("decode json-rpc: %w", err)
	}
	return out, outSess, nil
}

// readSSEResponse scans an SSE stream for the JSON-RPC message matching id.
func readSSEResponse(r io.Reader, id int) (jsonRPCResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var out jsonRPCResponse
		if json.Unmarshal([]byte(data), &out) == nil && out.ID == id {
			return out, nil
		}
	}
	if err := sc.Err(); err != nil {
		return jsonRPCResponse{}, err
	}
	return jsonRPCResponse{}, fmt.Errorf("no matching json-rpc response in stream")
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}
