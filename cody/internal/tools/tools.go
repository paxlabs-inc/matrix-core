// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package tools is codyd's worker ExtraTools bridge: the shared browser (for
// screenshot-as-evidence on UI tasks), an HTTP fetch, and a web-search bridge,
// wired into the worker's existing ExtraTools/ExtraDispatch seam (req 13.1).
// Every bridge is BOOT-SAFE: an unconfigured or unreachable service degrades to
// a structured "not configured" / error result and never fabricates output —
// the same posture as the Neo tool proxies. The browser screenshot is saved as
// a REAL file under the workspace so the worker can hand it to the acceptance
// gate as design evidence (req 13.2).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"matrix/cody/internal/llm"
)

// Config wires the shared tool services into a worker bridge.
type Config struct {
	// Root is the workspace root a browser screenshot is saved under.
	Root string
	// BrowserURL is the shared browser MCP Streamable-HTTP endpoint
	// (MATRIX_BROWSER_URL). Empty disables the browser bridge (boot-safe).
	BrowserURL   string
	BrowserToken string
	// SearxngURL is the shared SearXNG JSON endpoint (MATRIX_SEARXNG_URL).
	// Empty makes web_search return a structured "not configured" result.
	SearxngURL   string
	SearxngToken string
	// Timeout bounds one fetch/search/browser round-trip (default 30s).
	Timeout time.Duration
}

// Bridge exposes the extra tool surface and dispatches its calls.
type Bridge struct {
	cfg    Config
	client *http.Client
}

// New builds a bridge. A nil-safe zero Config yields fetch + a not-configured
// web_search (no browser tool advertised) — always safe to construct.
func New(cfg Config) *Bridge {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Bridge{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

// Tools returns the tool schemas to append to the worker's surface. The browser
// screenshot tool is advertised only when a browser service is configured, so a
// worker never sees a tool it cannot use.
func (b *Bridge) Tools() []llm.Tool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		s := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	tools := []llm.Tool{
		llm.NewFunctionTool("fetch", "HTTP GET a URL and return its (capped) text body — for reading docs, APIs, or a running dev server's output.",
			obj(map[string]interface{}{"url": strProp("absolute http(s) URL")}, "url")),
		llm.NewFunctionTool("web_search", "Search the web and return the top results (title, url, snippet).",
			obj(map[string]interface{}{"query": strProp("search query")}, "query")),
	}
	if b.cfg.BrowserURL != "" {
		tools = append(tools, llm.NewFunctionTool("browser_screenshot",
			"Open a URL (e.g. your running dev server) in the shared browser and capture a screenshot. Returns the workspace-relative path to the saved PNG — hand that path to turn_in as your UI screenshot evidence.",
			obj(map[string]interface{}{
				"url":  strProp("absolute URL to open (e.g. http://localhost:3000)"),
				"name": strProp("short name for the screenshot file (optional)"),
			}, "url")))
	}
	return tools
}

// Dispatch executes one ExtraTools call by name. It matches the worker's
// ExtraDispatch signature.
func (b *Bridge) Dispatch(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	switch name {
	case "fetch":
		return b.fetch(ctx, str(args, "url"))
	case "web_search":
		return b.webSearch(ctx, str(args, "query"))
	case "browser_screenshot":
		return b.browserScreenshot(ctx, str(args, "url"), str(args, "name"))
	default:
		return "", fmt.Errorf("unknown extra tool %q", name)
	}
}

// fetchCap bounds a fetch/search response body kept inline.
const fetchCap = 48 * 1024

func (b *Bridge) fetch(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("fetch requires an absolute http(s) URL")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, fetchCap+1))
	truncated := ""
	if len(body) > fetchCap {
		body = body[:fetchCap]
		truncated = "\n[fetch: body truncated]"
	}
	return fmt.Sprintf("[HTTP %d]\n%s%s", resp.StatusCode, string(body), truncated), nil
}

// searxResult is one SearXNG JSON result.
type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func (b *Bridge) webSearch(ctx context.Context, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	if b.cfg.SearxngURL == "" {
		return "web_search is not configured (no search backend); proceed without web results.", nil
	}
	endpoint := strings.TrimRight(b.cfg.SearxngURL, "/") + "/search?format=json&q=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if b.cfg.SearxngToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.SearxngToken)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Results []searxResult `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("web_search: decode: %w", err)
	}
	if len(out.Results) == 0 {
		return "no results for " + query, nil
	}
	var b2 strings.Builder
	for i, r := range out.Results {
		if i >= 8 {
			break
		}
		fmt.Fprintf(&b2, "%d. %s\n   %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL), strings.TrimSpace(r.Content))
	}
	return b2.String(), nil
}

func str(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}
