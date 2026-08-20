// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"path"
	"strings"

	"centra/agents/neo/internal/agent"
	"centra/agents/neo/internal/tools"
)

// describeStep classifies one tool call into a "workspace surface" the client
// renders as an animated viewport — never a label. It extracts the REAL inputs
// (the command, the URL, the file path) from the call arguments and the output
// from the result, so the surface shows what Neo is actually doing.
//
// Surfaces:
//   - terminal : shell / service commands  -> animated terminal window
//   - browser  : browser_* + fetch          -> browser chrome + viewport
//   - editor   : filesystem reads/writes     -> code-editor window
//   - search   : web_search / web_news       -> running chip (cards render separately)
//   - media    : image/video/audio tools     -> running chip (media grid renders separately)
//   - action   : everything else             -> a minimal animated action chip
func describeStep(ev agent.ToolEvent) map[string]interface{} {
	running := ev.Phase == agent.ToolStart
	step := map[string]interface{}{
		"id":      ev.ID,
		"tool":    ev.Name,
		"running": running,
		"ok":      !ev.IsErr,
	}
	bare := stripAlias(ev.Name)

	switch {
	case ev.Name == tools.CoreExecuteTool:
		step["surface"] = "action"
		step["title"] = "Secure execution"

	case hasAlias(ev.Name, "exec"):
		step["surface"] = "terminal"
		cmd := argStr(ev.Args, "command", "cmd", "script")
		if svc := argStr(ev.Args, "name"); cmd == "" && svc != "" {
			// service_list/logs/etc. without a command: show the action.
			cmd = strings.ReplaceAll(bare, "_", " ") + " " + svc
		}
		step["command"] = cmd
		step["cwd"] = argStr(ev.Args, "cwd")
		step["title"] = "Terminal"
		if !running {
			step["output"] = clip(ev.Result, maxTermChars)
		}

	case hasAlias(ev.Name, "browser") || strings.Contains(bare, "browser"):
		step["surface"] = "browser"
		fillBrowser(step, bare, ev, running)

	case hasAlias(ev.Name, "fetch") || strings.Contains(bare, "fetch"):
		step["surface"] = "browser"
		step["action"] = "read"
		step["url"] = argStr(ev.Args, "url")
		step["title"] = "Reading a page"
		if !running {
			step["excerpt"] = clip(ev.Result, maxExcerptChars)
		}

	case hasAlias(ev.Name, "fs"):
		step["surface"] = "editor"
		fillEditor(step, bare, ev, running)

	case isSearchTool(ev.Name):
		step["surface"] = "search"
		step["query"] = argStr(ev.Args, "query")
		if hasAlias(ev.Name, "exa") {
			step["title"] = exaTitle(bare)
		} else {
			step["title"] = "Searching the web"
		}

	case isMediaTool(bare):
		step["surface"] = "media"
		step["title"] = mediaTitle(bare)

	case hasAlias(ev.Name, "finance"):
		step["surface"] = "action"
		step["title"] = financeTitle(bare)

	case hasAlias(ev.Name, "git"):
		step["surface"] = "terminal"
		step["command"] = "git " + strings.TrimPrefix(strings.ReplaceAll(bare, "_", " "), "git ")
		step["title"] = "Repository"
		if !running {
			step["output"] = clip(ev.Result, maxTermChars)
		}

	default:
		step["surface"] = "action"
		step["title"] = "Using " + humanizeTool(ev.Name)
	}
	return step
}

func exaTitle(tool string) string {
	switch tool {
	case "exa_contents":
		return "Reading source evidence"
	case "exa_research_start":
		return "Starting grounded research"
	case "exa_research_get":
		return "Reading grounded research"
	case "exa_research_continue":
		return "Continuing grounded research"
	case "exa_research_cancel":
		return "Stopping grounded research"
	case "exa_outbound_brief":
		return "Researching an outbound brief"
	case "exa_social_draft":
		return "Researching a social draft"
	default:
		return "Searching grounded sources"
	}
}

func financeTitle(tool string) string {
	switch tool {
	case "market_quote", "market_quotes":
		return "Checking market prices"
	case "market_series":
		return "Reading a price chart"
	case "market_search":
		return "Searching markets"
	case "market_profile", "market_fundamentals":
		return "Reading company data"
	case "market_movers", "market_sectors", "market_status":
		return "Reading the market"
	case "market_news":
		return "Reading market news"
	case "market_earnings", "market_dividends":
		return "Reading company events"
	case "market_macro":
		return "Reading economic data"
	case "market_research_start":
		return "Starting grounded company research"
	case "market_research_get":
		return "Reading grounded company research"
	case "market_verify_facts":
		return "Verifying financial facts"
	default:
		return "Reading market data"
	}
}

func financeResult(ev agent.ToolEvent) (map[string]interface{}, bool) {
	if !hasAlias(ev.Name, "finance") || strings.TrimSpace(ev.Result) == "" {
		return nil, false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Result), &payload); err != nil || len(payload) == 0 {
		return nil, false
	}
	tool, _ := payload["tool"].(string)
	if tool == "" || !strings.HasPrefix(tool, "market_") {
		return nil, false
	}
	return payload, true
}

// fillBrowser maps a browser_* tool onto the browser surface: the action verb,
// the URL it's on, the human element it's acting on, and any typed text.
//
// Playwright's tools return a structured page report (Page URL / Page Title /
// a YAML accessibility snapshot), NOT human prose. We parse the real URL +
// title into the browser chrome and distil the snapshot into readable page
// text — so the surface renders like a page, never a raw JSON/YAML dump.
func fillBrowser(step map[string]interface{}, bare string, ev agent.ToolEvent, running bool) {
	action := strings.TrimPrefix(bare, "browser_")
	step["action"] = action
	step["target"] = argStr(ev.Args, "element")
	switch action {
	case "navigate":
		step["url"] = argStr(ev.Args, "url")
		step["title"] = "Browsing"
	case "navigate_back":
		step["title"] = "Going back"
	case "click":
		step["title"] = "Clicking"
	case "type", "fill_form":
		step["text"] = argStr(ev.Args, "text")
		step["title"] = "Typing"
	case "take_screenshot", "snapshot":
		step["title"] = "Looking at the page"
	case "wait_for":
		step["title"] = "Waiting for the page"
	default:
		step["title"] = "Browsing"
	}
	if running {
		return
	}
	// Attach the captured page still (BROWSER-FILMSTRIP req.4): the out-of-band
	// /media URL rides the ToolEvent, so the browser step carries only the URL —
	// never inline bytes — and the client renders this navigation as its own
	// filmstrip screen. Empty for a browser action with no capture.
	if ev.ScreenshotURL != "" {
		step["screenshot_url"] = ev.ScreenshotURL
	}
	// Lift the real page identity out of the tool report so the chrome shows
	// where Neo actually is (the URL in args may be empty for click/type/etc.).
	url, title, body := parseBrowserReport(ev.Result)
	if url != "" {
		step["url"] = url
	}
	if title != "" {
		step["page_title"] = title
	}
	if body != "" {
		step["excerpt"] = body
	}
}

// parseBrowserReport pulls the URL, title, and a readable text rendering out of
// a Playwright tool result. The result looks like:
//
//   - Page URL: https://example.com
//   - Page Title: Example Domain
//   - Page Snapshot:
//     ```yaml
//   - heading "Example Domain" [level=1] [ref=e1]
//   - paragraph: This domain is for use in examples.
//   - link "More information..." [ref=e2]
//     ```
//
// We render the snapshot as the visible text of the page (headings, prose,
// links, buttons), dropping the YAML scaffolding and [ref=…]/[level=…]
// annotations — turning the machine snapshot into a page-like view.
func parseBrowserReport(result string) (url, title, body string) {
	lines := strings.Split(result, "\n")
	var text []string
	inSnapshot := false
	for _, raw := range lines {
		ln := strings.TrimSpace(raw)
		if ln == "" {
			continue
		}
		if v, ok := afterLabel(ln, "Page URL:"); ok {
			url = v
			continue
		}
		if v, ok := afterLabel(ln, "Page Title:"); ok {
			title = v
			continue
		}
		if strings.Contains(ln, "Page Snapshot:") {
			inSnapshot = true
			continue
		}
		if strings.HasPrefix(ln, "```") {
			continue // open/close of the yaml fence
		}
		if !inSnapshot {
			continue
		}
		if t := snapshotLineText(ln); t != "" {
			// Collapse consecutive duplicates (repeated nav labels, etc.).
			if len(text) == 0 || text[len(text)-1] != t {
				text = append(text, t)
			}
		}
		if len(text) >= maxSnapshotLines {
			break
		}
	}
	body = clip(strings.Join(text, "\n"), maxExcerptChars)
	return url, title, body
}

// afterLabel returns the trimmed remainder after a "- <label>" / "<label>"
// prefix, and whether the line carried that label.
func afterLabel(ln, label string) (string, bool) {
	ln = strings.TrimPrefix(ln, "- ")
	if strings.HasPrefix(ln, label) {
		return strings.TrimSpace(strings.TrimPrefix(ln, label)), true
	}
	return "", false
}

// snapshotLineText distils one accessibility-snapshot line to its visible text,
// or "" for pure-structure nodes (generic/list/region with no own label).
func snapshotLineText(ln string) string {
	ln = strings.TrimPrefix(ln, "- ")
	// Drop trailing [ref=…], [level=…], [cursor=…], [checked], … annotations.
	if i := strings.Index(ln, " ["); i >= 0 {
		ln = ln[:i]
	}
	ln = strings.TrimSpace(ln)
	// "role: value" (e.g. paragraph: text, text: hello) → value, but only when
	// the part before the colon is a bare role token (no quote/space).
	if i := strings.Index(ln, ": "); i >= 0 {
		head := ln[:i]
		if !strings.ContainsAny(head, "\" ") {
			return strings.Trim(strings.TrimSpace(ln[i+2:]), `"`)
		}
	}
	// `role "Label"` (heading/link/button/…) → Label.
	if a := strings.Index(ln, "\""); a >= 0 {
		if b := strings.LastIndex(ln, "\""); b > a {
			return strings.TrimSpace(ln[a+1 : b])
		}
	}
	return ""
}

// fillEditor maps a filesystem tool onto the editor surface: the action, the
// path, the language (from extension), and a bounded preview of the content.
func fillEditor(step map[string]interface{}, bare string, ev agent.ToolEvent, running bool) {
	p := argStr(ev.Args, "path", "file_path")
	if p == "" {
		if ps := argStrings(ev.Args, "paths"); len(ps) > 0 {
			p = ps[0]
		}
	}
	step["path"] = p
	step["language"] = langForPath(p)

	switch {
	case strings.HasPrefix(bare, "read"):
		step["action"] = "read"
		step["title"] = "Reading a file"
		if !running {
			step["preview"] = clip(ev.Result, maxEditorChars)
		}
	case bare == "write_file":
		step["action"] = "write"
		step["title"] = "Writing a file"
		step["preview"] = clip(argStr(ev.Args, "content"), maxEditorChars)
	case bare == "edit_file":
		step["action"] = "edit"
		step["title"] = "Editing a file"
		if !running {
			step["preview"] = clip(ev.Result, maxEditorChars)
		}
	case strings.Contains(bare, "directory") || bare == "directory_tree" || strings.HasPrefix(bare, "list"):
		step["action"] = "list"
		step["title"] = "Browsing files"
		if !running {
			step["preview"] = clip(ev.Result, maxEditorChars)
		}
	case bare == "search_files":
		step["action"] = "search"
		step["title"] = "Searching files"
		if !running {
			step["preview"] = clip(ev.Result, maxEditorChars)
		}
	case bare == "move_file":
		step["action"] = "move"
		step["title"] = "Moving a file"
	default:
		step["action"] = "read"
		step["title"] = "Inspecting files"
		if !running {
			step["preview"] = clip(ev.Result, maxEditorChars)
		}
	}
}

// Display caps — generous enough to look real in a scrolling viewport, bounded
// so a giant payload never floods the SSE stream or the client.
const (
	maxTermChars     = 6000
	maxEditorChars   = 6000
	maxExcerptChars  = 1600
	maxSnapshotLines = 60
)

func isMediaTool(bare string) bool {
	switch bare {
	case "generate_image", "edit_image", "generate_video", "transcribe_audio":
		return true
	}
	return false
}

func mediaTitle(bare string) string {
	switch bare {
	case "generate_image":
		return "Creating an image"
	case "edit_image":
		return "Editing an image"
	case "generate_video":
		return "Generating a video"
	case "transcribe_audio":
		return "Transcribing audio"
	}
	return "Working with media"
}

// hasAlias reports whether name belongs to the given MCP alias (names are
// "<alias>__<tool>").
func hasAlias(name, alias string) bool {
	return strings.HasPrefix(name, alias+"__")
}

// stripAlias drops the "<alias>__" prefix, leaving the bare tool name.
func stripAlias(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

// argStr returns the first non-empty string argument among keys.
func argStr(args map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// argStrings returns a string slice argument (e.g. fs paths).
func argStrings(args map[string]interface{}, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	raw, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// clip trims and bounds a display string to max runes, appending an ellipsis
// when truncated.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "\n…"
}

// langForPath maps a file extension to a coarse language token the editor
// surface uses for syntax tinting. Empty when unknown.
func langForPath(p string) string {
	if p == "" {
		return ""
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".json":
		return "json"
	case ".md", ".mdx":
		return "markdown"
	case ".css":
		return "css"
	case ".html":
		return "html"
	case ".sh", ".bash":
		return "bash"
	case ".sql":
		return "sql"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	}
	return ""
}
