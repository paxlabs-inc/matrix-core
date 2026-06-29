package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// overflowDir is the base directory for run-scoped overflow files. It is
// created on demand; failures are non-fatal (the result falls back to
// head+tail truncation, preserving today's behavior).
const overflowDir = "/tmp/neo-overflow"

// overflowMu serializes overflow file creation (multiple tool calls may
// dispatch concurrently; os.Create is not atomic under a shared name).
var overflowMu sync.Mutex

// overflowToolResult replaces capToolResult with an overflow-file mechanism.
// When the result exceeds the inline budget (maxToolResultChars), the FULL
// output is persisted to a run-scoped file and the inline content is replaced
// with a truncation notice carrying the file path and instructions to read the
// remainder. When the result is within budget, it is returned unchanged.
//
// The caller (the agent loop) checks the returned flag to inject a guidance
// message telling the model to read the overflow file before reasoning over
// the result (the read-full discipline, enforced in code — req.4.2).
//
// On any I/O failure the function falls back to the legacy head+tail
// truncation so the agent never crashes on an overflow write error.
func overflowToolResult(content, callID string) (inline string, overflowed bool) {
	if len(content) <= maxToolResultChars {
		return content, false
	}
	// Try to persist the full output to a file.
	path, err := writeOverflowFile(content, callID)
	if err != nil {
		// Fallback: legacy head+tail truncation (never silently lose data
		// structure, but the full content is NOT recoverable — the path
		// is empty so no guidance is injected).
		return capToolResult(content), false
	}
	// Build the inline truncation notice: a head of the output + the
	// path marker + instructions. The head is large enough to give the
	// model context about the shape of the result.
	head := maxToolResultChars * 3 / 4
	if head > len(content) {
		head = len(content)
	}
	inline = content[:head] +
		fmt.Sprintf("\n…[OUTPUT TRUNCATED: %d of %d bytes shown. The FULL result was saved to: %s — read this file to see the complete output before answering from this result.]…\n", head, len(content), path)
	return inline, true
}

// writeOverflowFile persists the full tool result to a run-scoped file and
// returns its path. The file is named by the tool call ID (stable, unique
// within a turn) so it can be cleaned up after the run settles.
func writeOverflowFile(content, callID string) (string, error) {
	if callID == "" {
		callID = "unknown"
	}
	// Sanitize the call ID for use as a filename.
	safe := sanitizeFilename(callID)
	path := filepath.Join(overflowDir, safe+".txt")

	overflowMu.Lock()
	defer overflowMu.Unlock()

	if err := os.MkdirAll(overflowDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizeFilename strips path separators and other unsafe characters.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "overflow"
	}
	return out
}

// cleanupOverflowFiles removes run-scoped overflow files for the given call IDs.
// Best-effort: errors are ignored (the files are in /tmp and will be cleaned
// by the OS). Called at the end of a turn or by the compaction path.
func cleanupOverflowFiles(callIDs []string) {
	for _, id := range callIDs {
		if id == "" {
			continue
		}
		path := filepath.Join(overflowDir, sanitizeFilename(id)+".txt")
		_ = os.Remove(path)
	}
}
