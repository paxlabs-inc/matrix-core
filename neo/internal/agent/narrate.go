package agent

import (
	"fmt"
	"strings"

	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// narrateToolCall generates one concise, action-specific intent sentence
// describing what the agent is about to do and why, drawn from the actual
// tool call operation (the tool name and its arguments). It is NOT a fixed
// boilerplate string — it reflects the real target/operation so consecutive
// narrations carry distinct content (neo-smoothness req.2.2, 2.4).
//
// The tone is direct and unsentimental: no preamble, no validation phrases
// ("Great idea"), no emojis, no protocol jargon (req.2.3). Returns "" when
// the call has no extractable intent (the caller should skip narration then).
func narrateToolCall(call llm.ToolCall) string {
	name := call.Function.Name
	args, err := call.ParseArgs()
	if err != nil {
		return "calling " + bareName(name)
	}

	// core_execute: the intent field IS the narration.
	if name == tools.CoreExecuteTool {
		intent := strings.TrimSpace(stringArg(args["intent"]))
		if intent != "" {
			return "Routing through the secure path: " + clipNarration(intent)
		}
		return "Routing a task through the secure execution path"
	}

	// todo: setting/updating the task list.
	if name == tools.TodoTool {
		return "Updating the task list"
	}

	// task_complete: declaring completion.
	if name == tools.TaskCompleteTool {
		return "Declaring this step complete"
	}

	// memory_recall: searching memory.
	if name == tools.MemoryRecallTool {
		q := strings.TrimSpace(stringArg(args["query"]))
		if q != "" {
			return "Searching memory for: " + clipNarration(q)
		}
		return "Searching memory"
	}

	// spawn_subagents: spawning helpers.
	if name == tools.SpawnSubagentsTool {
		return "Spawning sub-agents for parallel work"
	}

	bare := bareName(name)
	alias := toolAlias(name)

	// Try to extract the most informative argument for the narration.
	// Priority varies by tool type.
	switch {
	case hasArg(args, "command", "cmd", "script"):
		cmd := stringArg(args["command"])
		if cmd == "" {
			cmd = stringArg(args["cmd"])
		}
		if cmd == "" {
			cmd = stringArg(args["script"])
		}
		if cmd != "" {
			return "Running: " + clipNarration(cmd)
		}
	case hasArg(args, "query", "q", "search", "search_query"):
		q := stringArg(args["query"])
		if q == "" {
			q = stringArg(args["q"])
		}
		if q == "" {
			q = stringArg(args["search"])
		}
		if q == "" {
			q = stringArg(args["search_query"])
		}
		if q != "" {
			return "Searching for: " + clipNarration(q)
		}
	case hasArg(args, "path", "file_path", "file"):
		p := stringArg(args["path"])
		if p == "" {
			p = stringArg(args["file_path"])
		}
		if p == "" {
			p = stringArg(args["file"])
		}
		if p != "" {
			base := pathBase(p)
			action := "Reading"
			if strings.HasPrefix(bare, "write") || strings.HasPrefix(bare, "create") {
				action = "Writing"
			} else if strings.HasPrefix(bare, "edit") || strings.HasPrefix(bare, "patch") {
				action = "Editing"
			} else if strings.HasPrefix(bare, "search") || strings.HasPrefix(bare, "find") || strings.HasPrefix(bare, "list") {
				action = "Looking for"
			} else if strings.HasPrefix(bare, "move") || strings.HasPrefix(bare, "rename") {
				action = "Moving"
			}
			return fmt.Sprintf("%s %s", action, base)
		}
	case hasArg(args, "url"):
		u := stringArg(args["url"])
		if u != "" {
			if strings.HasPrefix(bare, "navigate") || strings.Contains(bare, "browser") {
				return "Browsing to " + clipNarration(u)
			}
			return "Fetching " + clipNarration(u)
		}
	}

	// Fallback: humanize the tool name.
	return humanizeToolName(bare, alias)
}

// bareName strips the "<alias>__" prefix, leaving the bare tool name.
func bareName(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

// toolAlias extracts the "<alias>" prefix (or "" when none).
func toolAlias(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		return name[:i]
	}
	return ""
}

// hasArg reports whether any of the keys is present in args with a non-empty
// string value.
func hasArg(args map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if s := stringArg(args[k]); strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// pathBase returns the last path component (the filename).
func pathBase(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// clipNarration trims a string to a readable length for a narration line.
func clipNarration(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = strings.TrimSpace(s[:120]) + "…"
	}
	return s
}

// humanizeToolName turns a bare tool name into a readable action phrase.
func humanizeToolName(bare, alias string) string {
	// Replace underscores with spaces and capitalize.
	s := strings.ReplaceAll(bare, "_", " ")
	if s == "" {
		s = bare
	}
	if len(s) > 0 {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}
