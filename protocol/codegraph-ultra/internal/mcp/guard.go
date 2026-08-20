// Package mcp provides a fragment guard that prevents raw source code from
// leaking across the MCP tool boundary. Every tool output passes through
// guardFragment() before reaching the agent.
package mcp

import (
	"fmt"
	"strings"
)

// guardFragment validates that a tool output fragment contains only structural
// metadata (NODE/EDGE lines with key=value pairs) and no raw source code.
// Returns the fragment unchanged if valid, or an error if source leak detected.
func guardFragment(fragment string) (string, error) {
	if fragment == "" {
		return fragment, nil
	}

	lines := strings.Split(fragment, "\n")
	hasHeader := false

	for i, line := range lines {
		// Empty lines are fine
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Comment lines (including # FRAGMENT header)
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			if strings.Contains(line, "FRAGMENT") {
				hasHeader = true
			}
			continue
		}

		// NODE lines
		if strings.HasPrefix(line, "NODE ") {
			continue
		}

		// EDGE lines
		if strings.HasPrefix(line, "EDGE ") {
			continue
		}

		// EDGES section marker
		if strings.TrimSpace(line) == "EDGES" {
			continue
		}

		// Key=value lines (indented edge data, forward or reverse)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "^") || strings.Contains(trimmed, "=") {
			continue
		}

		// STATS/META lines
		if strings.HasPrefix(trimmed, "NODES ") || strings.HasPrefix(trimmed, "EDGES ") ||
			strings.HasPrefix(trimmed, "FILES ") || strings.HasPrefix(trimmed, "LANGS ") ||
			strings.HasPrefix(trimmed, "NODES BY ") || strings.HasPrefix(trimmed, "EDGES BY ") {
			continue
		}

		// If we get here, the line doesn't match any allowed pattern.
		// Check if it looks like indented source code (tab or 4+ space prefix).
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
			return "", fmt.Errorf("fragment guard: line %d looks like raw source code (indented text); refusing to emit", i+1)
		}

		// Unknown top-level line format — be conservative
		return "", fmt.Errorf("fragment guard: line %d has unrecognized format %q; refusing to emit", i+1, truncateStr(line, 60))
	}

	if !hasHeader && len(lines) > 3 {
		return fragment, nil // Allow short fragments without header
	}

	return fragment, nil
}

// guardFragmentSafe wraps guardFragment, returning an error message fragment instead of an error.
func guardFragmentSafe(fragment string) string {
	result, err := guardFragment(fragment)
	if err != nil {
		return fmt.Sprintf("# FRAGMENT guard_error=true\nERROR: %v", err)
	}
	return result
}

// sanitizeKVXValue escapes a value for safe inclusion in a .kvx line.
// Prevents newlines that could break the fragment grammar.
func sanitizeKVXValue(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
