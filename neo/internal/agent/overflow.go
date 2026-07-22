// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// overflow.go implements the truncation overflow-file (neo-smoothness req.4):
// the read-full discipline enforced in code. When a tool / core_execute result
// exceeds the inline transcript budget, the FULL output is written to a
// run-scoped file and the transcript keeps a bounded head+tail plus a
// truncation notice carrying the file's token and how to read the remainder —
// instead of silently cutting the middle (the failure class where the model
// confidently states a value pulled from a half-truncated result). The model
// pages the remainder back in with the synthetic read_overflow tool; the loop
// will not let the turn finish while any overflow from this turn is still
// unread (overflowUnread, checked at both finish gates).

// readOverflowTool is the synthetic, agent-intercepted function the model calls
// to page the full content of a truncated result back into the window. It is
// NOT a real MCP server (exempt from the manifest bijection check) and is
// handled inside the agent loop, never routed through the tool Manager, because
// the overflow store lives on the agent.
const readOverflowTool = "read_overflow"

// overflowReadChunk is the default number of bytes read_overflow returns per
// call when the model does not specify a limit — bounded so paging the file
// never re-blows the transcript byte budget that caused the overflow.
const overflowReadChunk = 8000

// ovfEntry tracks one stored overflow payload: its file path, total size, and
// whether the model has read any of it yet (the read-full latch).
type ovfEntry struct {
	path string
	size int
	read bool
}

// overflowStore persists oversized tool results to a run-scoped temp directory
// and serves bounded windows of them back. It is created lazily on the first
// overflow and removed wholesale at turn end (cleanup). All methods are
// goroutine-safe: capping and read_overflow can both run from the concurrent
// dispatch path.
type overflowStore struct {
	mu      sync.Mutex
	dir     string
	seq     int
	entries map[string]*ovfEntry
}

// put writes content to a fresh run-scoped file and returns its token. On any
// filesystem error it returns ok=false so the caller falls back to plain
// head+tail truncation (graceful degradation — a bounded transcript beats a
// crash).
func (s *overflowStore) put(content string) (token string, path string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]*ovfEntry{}
	}
	if s.dir == "" {
		d, err := os.MkdirTemp("", "neo-overflow-")
		if err != nil {
			return "", "", false
		}
		s.dir = d
	}
	s.seq++
	token = "ovf-" + strconv.Itoa(s.seq)
	path = filepath.Join(s.dir, token+".txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", "", false
	}
	s.entries[token] = &ovfEntry{path: path, size: len(content)}
	return token, path, true
}

// read returns a bounded window [offset, offset+limit) of the stored payload
// and marks the entry read. ok=false when the token is unknown. next is the
// offset to pass for the following page, or -1 when the end was reached.
func (s *overflowStore) read(token string, offset, limit int) (chunk string, total, next int, ok bool) {
	s.mu.Lock()
	e := s.entries[token]
	dir := s.dir
	s.mu.Unlock()
	if e == nil {
		return "", 0, -1, false
	}
	data, err := os.ReadFile(e.path)
	if err != nil {
		return "", 0, -1, false
	}
	_ = dir
	total = len(data)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = overflowReadChunk
	}
	if offset >= total {
		s.markRead(token)
		return "", total, -1, true
	}
	end := offset + limit
	if end >= total {
		end = total
		next = -1
	} else {
		next = end
	}
	s.markRead(token)
	return string(data[offset:end]), total, next, true
}

// markRead latches an entry as read (the read-full discipline is satisfied for
// that payload).
func (s *overflowStore) markRead(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[token]; e != nil {
		e.read = true
	}
}

// unread returns the tokens of overflow payloads not yet read, in stable order.
func (s *overflowStore) unread() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i := 1; i <= s.seq; i++ {
		tok := "ovf-" + strconv.Itoa(i)
		if e := s.entries[tok]; e != nil && !e.read {
			out = append(out, tok)
		}
	}
	return out
}

// cleanup removes the run-scoped directory and all its files. Idempotent.
func (s *overflowStore) cleanup() {
	s.mu.Lock()
	dir := s.dir
	s.dir = ""
	s.entries = nil
	s.seq = 0
	s.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// overflowGateExempt lists tools whose oversized results never gate the close:
// memory recall is prior context the model consults, not task evidence, so an
// unread recall dump must not hold a finished answer hostage (the 2026-07-22
// loopty-loop was armed by exactly that). The overflow file is still written
// and readable via read_overflow; its entry just starts in the read state.
var overflowGateExempt = map[string]bool{tools.MemoryRecallTool: true}

// capToolResult bounds a single tool result for the working transcript. Within
// budget it is returned verbatim. When it exceeds maxToolResultChars the FULL
// output is persisted to a run-scoped overflow file and the transcript keeps a
// head + tail plus a truncation notice carrying the overflow token and how to
// read the remainder (req.4.1) — never a silent cut. If the file cannot be
// written it degrades to plain head+tail truncation (capToolResultPlain).
// name is the producing tool: gate-exempt tools spill without arming the
// unread-overflow close guard.
func (a *Agent) capToolResult(name, content string) string {
	if len(content) <= maxToolResultChars {
		return content
	}
	if a.turn.overflow == nil {
		a.turn.overflow = &overflowStore{}
	}
	token, path, ok := a.turn.overflow.put(content)
	if !ok {
		return capToolResultPlain(content)
	}
	if overflowGateExempt[name] {
		a.turn.overflow.markRead(token)
	}
	head := maxToolResultChars * 3 / 4
	tail := maxToolResultChars - head
	notice := fmt.Sprintf(
		"\n…[truncation_notice] this result is %d bytes; only the head and tail are shown here. The FULL output is saved at %s. Read the rest with the read_overflow tool (token=%q, with offset/limit to page) BEFORE you rely on or answer from this result — do not reason over the cut-out middle.…\n",
		len(content), path, token,
	)
	return content[:head] + notice + content[len(content)-tail:]
}

// capToolResultPlain is the no-overflow-store fallback: a bounded head+tail with
// an explicit marker (the legacy behavior, used only when the overflow file
// cannot be written so the turn still proceeds with a bounded transcript).
func capToolResultPlain(s string) string {
	if len(s) <= maxToolResultChars {
		return s
	}
	head := maxToolResultChars * 3 / 4
	tail := maxToolResultChars - head
	return s[:head] +
		fmt.Sprintf("\n…(tool result truncated for working memory: %d of %d bytes shown)…\n", maxToolResultChars, len(s)) +
		s[len(s)-tail:]
}

// overflowUnread reports whether any overflow payload from this turn is still
// unread — the read-full gate the loop checks before letting the turn finish.
func (a *Agent) overflowUnread() bool {
	return a.turn.overflow != nil && len(a.turn.overflow.unread()) > 0
}

// overflowUnreadNudge composes the guidance-channel steer that blocks finishing
// while overflow output is unread, naming the outstanding tokens.
func (a *Agent) overflowUnreadNudge() string {
	if a.turn.overflow == nil {
		return ""
	}
	toks := a.turn.overflow.unread()
	if len(toks) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Your answer was NOT delivered — the user has not seen it. It is being held back because a tool result was too large to show in full and was saved to an overflow file (unread: %s). Read each with the read_overflow tool (one call per token), then give your final answer again — reuse your held-back text if it still holds.",
		strings.Join(toks, ", "),
	)
}

// readOverflow handles a model read_overflow call: it returns a bounded window
// of a stored overflow payload and marks it read. Parse/usage errors come back
// in-band (isError=true) so the model corrects rather than the harness retrying.
func (a *Agent) readOverflow(args map[string]interface{}) (string, bool) {
	if a.turn.overflow == nil {
		return "there is no saved overflow output to read in this turn.", true
	}
	token, _ := args["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return "read_overflow needs the 'token' from the truncation notice (e.g. \"ovf-1\").", true
	}
	offset := argInt(args["offset"], 0)
	limit := argInt(args["limit"], overflowReadChunk)
	chunk, total, next, ok := a.turn.overflow.read(token, offset, limit)
	if !ok {
		return fmt.Sprintf("no overflow output is stored under token %q.", token), true
	}
	if chunk == "" && next < 0 {
		return fmt.Sprintf("(end of overflow %q: %d bytes total, nothing past offset %d)", token, total, offset), false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[overflow %s — bytes %d-%d of %d]\n", token, offset, offset+len(chunk), total)
	b.WriteString(chunk)
	if next >= 0 {
		fmt.Fprintf(&b, "\n…(more remains — call read_overflow again with offset=%d to continue)…", next)
	} else {
		b.WriteString("\n…(end of output)…")
	}
	return b.String(), false
}

// readOverflowSchema advertises the synthetic read-full tool to the model. It is
// appended to the agent's advertised schema set (top-level and sub-agent) so the
// agent can always page a truncated result back in.
func readOverflowSchema() llm.Tool {
	return llm.NewFunctionTool(
		readOverflowTool,
		"Read the full content of a tool result that was too large to show inline and was saved to an overflow file (you'll see a [truncation_notice] with a token like \"ovf-1\"). Call this with that token to page the saved output back in BEFORE you rely on or answer from a truncated result — never reason over the cut-out middle. Use offset and limit to read it in chunks.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"token": map[string]interface{}{
					"type":        "string",
					"description": "The overflow token from the truncation notice (e.g. \"ovf-1\").",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Byte offset to start reading from (default 0).",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum bytes to return in this page (default 8000). Page through with successive offsets.",
				},
			},
			"required": []interface{}{"token"},
		},
	)
}

// argInt coerces a JSON number argument (float64) into an int, falling back to
// def for a missing or non-numeric value.
func argInt(v interface{}, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}
