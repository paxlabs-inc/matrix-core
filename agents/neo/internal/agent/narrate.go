// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"

	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

// narrate.go implements the harness side of narrate-before-act (neo-smoothness
// req.2): when the model dispatches tools WITHOUT writing its own preamble this
// step, the loop synthesizes one concise, action-specific intent line drawn
// from the REAL operation so the user can always follow along. It is the
// opposite of a fixed boilerplate marker (the old "• <tool>" / the NE-2
// "routing this through the secure path" wall): each line reflects the actual
// target/operation, so distinct work reads as distinct progress and
// neo-execution-reliability's narration coalescing collapses only genuine
// consecutive repeats. The tone is direct and plain — no preamble/validation
// phrases, no emojis, and no protocol jargon (req.2.3): tool wire-names are
// humanized and internal machinery is never surfaced.

// narrateMaxLen bounds a narration line so a long argument (a big command, a
// pasted URL) can't blow the line into a wall of text.
const narrateMaxLen = 140

// narrateArgMaxLen bounds the salient-argument fragment embedded in a line.
const narrateArgMaxLen = 90

// narrateBatch produces one concise, action-specific intent line for a batch of
// tool calls about to be dispatched, or "" when there is nothing worth saying
// (e.g. a completion-only batch). A single call is described directly; a
// multi-call batch is summarized by its distinct actions so the line still
// reflects the real work without spamming one line per call.
func narrateBatch(calls []llm.ToolCall) string {
	classes := make(map[string]bool, len(calls))
	for _, c := range calls {
		classes[deterministicMilestoneClass(c)] = true
	}
	for _, candidate := range []struct{ class, line string }{
		{"build", "Starting a durable project build."},
		{"verify", "Running project verification."},
		{"write", "Updating project files."},
		{"command", "Running a project command."},
		{"inspect", "Inspecting the project."},
		{"research", "Gathering source evidence."},
		{"act", "Applying the requested operation."},
	} {
		if classes[candidate.class] {
			return candidate.line
		}
	}
	return ""
}

func deterministicMilestoneClass(call llm.ToolCall) string {
	name := strings.ToLower(call.Function.Name)
	base := name
	if index := strings.LastIndex(base, "__"); index >= 0 {
		base = base[index+2:]
	}
	if base == tools.BuildProjectTool {
		return "build"
	}
	if base == "shell" || strings.Contains(base, "exec") || base == "run" || strings.Contains(base, "command") {
		args, _ := call.ParseArgs()
		command := strings.ToLower(salientArg(args, "command", "cmd", "script"))
		for _, marker := range []string{" test", "go test", " vet", "go vet", " build", "go build", "lint", "typecheck", "check"} {
			if strings.Contains(" "+command, marker) {
				return "verify"
			}
		}
		return "command"
	}
	for _, marker := range []string{"write", "edit", "patch", "create_directory", "move_file", "delete", "remove"} {
		if strings.Contains(base, marker) {
			return "write"
		}
	}
	for _, marker := range []string{"read", "list", "tree", "search_files", "file_info", "git_"} {
		if strings.Contains(base, marker) {
			return "inspect"
		}
	}
	for _, marker := range []string{"web_search", "web_news", "fetch", "exa_", "navigate", "browser"} {
		if strings.Contains(base, marker) {
			return "research"
		}
	}
	if base == tools.TodoTool || base == readOverflowTool {
		return ""
	}
	return "act"
}

// narratePhrase returns the lowercase verb phrase for a single tool call (no
// leading capital, no trailing period) so narrateBatch can compose it either as
// a standalone sentence or as one item in a list. Returns "" for calls that
// should not be narrated (the completion gate).
func narratePhrase(call llm.ToolCall) string {
	name := call.Function.Name
	// The task-list update and the internal overflow read are not narrated: the
	// live checklist surface speaks for itself, and read_overflow is plumbing
	// (paging a truncated result back in) — a narration line for either would be
	// noise.
	if name == tools.TodoTool || name == readOverflowTool {
		return ""
	}
	args, _ := call.ParseArgs() // best-effort; a parse error yields nil args

	// Synthetic, Neo-owned tools get purpose-built plain-language phrases so no
	// internal machinery leaks to the user (req.2.3).
	switch name {
	case tools.CoreExecuteTool:
		if intent := salientArg(args, "intent", "description", "task"); intent != "" {
			return "working on " + intent
		}
		return "handling that for you"
	case tools.MemoryRecallTool:
		if q := salientArg(args, "query", "q"); q != "" {
			return "checking my memory for " + q
		}
		return "checking my memory"
	case tools.SpawnSubagentsTool:
		return "splitting this into parallel sub-tasks"
	case tools.ConstructRenderTool:
		return "putting that on your screen"
	case tools.WriteSkillTool:
		return "saving this approach for next time"
	case tools.DesktopLookTool:
		if t := salientArg(args, "target"); t != "" {
			return "looking on the desktop for " + t
		}
		return "looking at the desktop"
	case tools.DesktopA11yTool:
		return "reading the desktop's elements"
	}

	// The disposable-desktop actuation lane (desktop__…) reads as plain
	// computer use, never internal machinery.
	if strings.HasPrefix(name, "desktop__") {
		switch {
		case strings.Contains(name, "click"), strings.Contains(name, "mouse"), strings.Contains(name, "scroll"):
			return "clicking on the desktop"
		case strings.Contains(name, "type"), strings.Contains(name, "paste"), strings.Contains(name, "keys"):
			if t := salientArg(args, "text"); t != "" {
				return "typing " + t
			}
			return "typing on the desktop"
		case strings.Contains(name, "screenshot"):
			return "taking a screenshot of the desktop"
		case strings.Contains(name, "application"):
			if app := salientArg(args, "application"); app != "" {
				return "opening " + app + " on the desktop"
			}
			return "switching apps on the desktop"
		case strings.Contains(name, "read_file"), strings.Contains(name, "write_file"):
			return "moving a file on the desktop"
		}
		return "using the desktop"
	}

	// MCP / natural tools: strip any "server__tool" namespace, classify by
	// keyword, and fold in the most salient argument value.
	base := name
	if i := strings.LastIndex(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	lb := strings.ToLower(base)
	switch {
	case strings.Contains(lb, "search"):
		if q := salientArg(args, "query", "q", "term"); q != "" {
			return "searching for " + q
		}
		return "searching the web"
	case strings.Contains(lb, "news"):
		return "checking the news"
	case strings.Contains(lb, "shell"), strings.Contains(lb, "exec"),
		strings.Contains(lb, "command"), strings.Contains(lb, "bash"),
		strings.Contains(lb, "terminal"), lb == "run":
		if cmd := salientArg(args, "command", "cmd", "script"); cmd != "" {
			return "running " + cmd
		}
		return "running a command"
	case strings.Contains(lb, "write"), strings.Contains(lb, "create"),
		strings.Contains(lb, "edit"), strings.Contains(lb, "append"),
		strings.Contains(lb, "save"), strings.Contains(lb, "patch"):
		if p := salientArg(args, "path", "file", "filename", "name"); p != "" {
			return "writing " + p
		}
		return "writing the changes"
	case strings.Contains(lb, "delete"), strings.Contains(lb, "remove"), lb == "rm":
		if p := salientArg(args, "path", "file", "filename", "name"); p != "" {
			return "removing " + p
		}
		return "removing that"
	case strings.Contains(lb, "list"), strings.Contains(lb, "glob"),
		strings.Contains(lb, "find"), strings.Contains(lb, "grep"), lb == "ls":
		return "looking through the files"
	case strings.Contains(lb, "fetch"), strings.Contains(lb, "read"),
		strings.Contains(lb, "open"), strings.Contains(lb, "load"),
		strings.Contains(lb, "get"), strings.Contains(lb, "cat"):
		if u := salientArg(args, "url", "path", "file", "filename"); u != "" {
			return "reading " + u
		}
		return "reading that"
	case strings.Contains(lb, "image"):
		return "creating an image"
	case strings.Contains(lb, "video"):
		return "creating a video"
	case strings.Contains(lb, "audio"), strings.Contains(lb, "transcribe"):
		return "transcribing the audio"
	case strings.Contains(lb, "browser"), strings.Contains(lb, "navigate"),
		strings.Contains(lb, "goto"), strings.Contains(lb, "click"),
		strings.Contains(lb, "screenshot"):
		return "browsing the web"
	}

	// Fallback: humanize the tool name itself (underscores → spaces) plus any
	// obvious target, so even an unknown tool reads as plain language.
	human := strings.ReplaceAll(base, "_", " ")
	if v := salientArg(args, "query", "q", "path", "file", "url", "name", "text", "prompt"); v != "" {
		return human + " " + v
	}
	return human
}

// salientArg returns the first non-empty string-valued argument among keys, with
// whitespace collapsed and length clamped, for embedding in a narration line.
// Non-string values are ignored (a narration line carries a readable target,
// not a serialized object).
func salientArg(args map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		v, ok := args[k]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			continue
		}
		if len(s) > narrateArgMaxLen {
			s = strings.TrimSpace(s[:narrateArgMaxLen]) + "…"
		}
		return s
	}
	return ""
}

// capitalize upper-cases the first rune of s (ASCII-safe for the lead verbs we
// generate), leaving the remainder untouched.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// clampLine bounds a finished narration line so a salient argument can't push it
// into a wall of text.
func clampLine(s string) string {
	if len(s) > narrateMaxLen {
		return strings.TrimSpace(s[:narrateMaxLen]) + "…"
	}
	return s
}
