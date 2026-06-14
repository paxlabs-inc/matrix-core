// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"path"
	"strings"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/tools"
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
		step["title"] = "Searching the web"

	case isMediaTool(bare):
		step["surface"] = "media"
		step["title"] = mediaTitle(bare)

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

// fillBrowser maps a browser_* tool onto the browser surface: the action verb,
// the URL it's on, the human element it's acting on, and any typed text.
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
	if !running && action != "navigate" {
		// A short readable trace of what came back (never the full snapshot
		// dump). Navigation results are mostly accessibility trees, so we omit
		// them — the chrome + URL is the signal there.
		step["excerpt"] = clip(ev.Result, maxExcerptChars)
	}
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
	maxTermChars    = 6000
	maxEditorChars  = 6000
	maxExcerptChars = 800
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
