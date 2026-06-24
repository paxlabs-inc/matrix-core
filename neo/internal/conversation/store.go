// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package conversation is Neo's durable chat-thread memory.
//
// Neo's sessions are otherwise in-process only: turns vanish on restart and
// the front had no way to list or reopen a past thread (the /conversations
// routes proxied to the MCL daemon, which never saw a Neo conversation). This
// store fixes that — it persists each turn as append-only JSONL (one line per
// turn) per conversation_id, so history survives reloads, new chats, suspend,
// and redeploy at O(1) write cost per turn (not the O(n) full-file rewrite of
// the original single-JSON shape).
//
// The on-disk shape is byte-compatible with the daemon's own conversation
// store (executor/cmd/mcl-execute/daemon_conversation.go): a pre-Neo daemon
// thread written as a single JSON object is still readable (loadLocked falls
// back to the legacy single-object parse when the file is not JSONL). New
// writes are always JSONL. Neo derives the SAME directory
// (filepath.Dir(cortexRoot)/conversations = /data/conversations in prod), so a
// user's pre-Neo daemon threads and their new Neo threads list together as one
// unified history. Since the client now talks only to Neo, Neo is the single
// writer.
//
// Retained-turn cap: the hot set (live .jsonl) is bounded by RetainedTurns
// (config NEO_CONVERSATION_RETAINED_TURNS, default 1000). Turns that scroll
// past the cap roll to a sibling archive file (<convID>.archive.jsonl) —
// durable, NOT dropped — and remain retrievable via History/Archived/Recent.
// The rename-based atomic write (tmp+rename) is reserved for the archive
// rollup/compaction step, never for the per-turn append.
//
// It is a pure side-channel: it never touches cortex, signs anything, or
// perturbs replay — conversation continuity and the audit/replay chain are
// independent storage.
package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRecallTurns is how many recent turns are seeded back into a
	// resumed conversation's working context by default.
	DefaultRecallTurns = 16

	// DefaultRetainedTurns is the cap on how many turns stay in the hot
	// .jsonl before older ones roll to the archive. Bounded so the live file
	// (and thus per-append fsync cost) stays small; full history is never lost.
	DefaultRetainedTurns = 1000
)

// Turn is one durable line of a conversation. JSON tags match the daemon store
// and the web client's ConversationTurn.
type Turn struct {
	Role     string    `json:"role"` // "user" | "assistant"
	Text     string    `json:"text"`
	IntentID string    `json:"intent_id,omitempty"`
	TS       time.Time `json:"ts"`
}

// Record is the persisted shape: a bounded, append-only turn list for one
// conversation_id.
type Record struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title,omitempty"`
	Turns          []Turn    `json:"turns"`
	Updated        time.Time `json:"updated"`
}

// Summary is the compact list shape for GET /conversations.
type Summary struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	Preview        string    `json:"preview"`
	TurnCount      int       `json:"turn_count"`
	Updated        time.Time `json:"updated"`
}

// Store is Neo's durable conversation memory. One mutex guards all access;
// conversations are small and Neo serves one user, so this is plenty fast.
type Store struct {
	mu  sync.Mutex
	dir string

	// retainedTurns caps the hot .jsonl turn count; overflow rolls to the
	// archive. Zero means use DefaultRetainedTurns at append time.
	retainedTurns int
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged. The
// retained-turn cap defaults to DefaultRetainedTurns and can be overridden via
// the NEO_CONVERSATION_RETAINED_TURNS env var (read once at Open).
func Open(dir string) *Store {
	return openWithCap(dir, retainedTurnsFromEnv())
}

// openWithCap is the test seam that sets an explicit retained-turn cap. It is
// unexported (test-only); production callers use Open.
func openWithCap(dir string, cap int) *Store {
	s := &Store{dir: strings.TrimSpace(dir), retainedTurns: cap}
	if s.retainedTurns <= 0 {
		s.retainedTurns = DefaultRetainedTurns
	}
	return s
}

// retainedTurnsFromEnv reads the NEO_CONVERSATION_RETAINED_TURNS env var; an
// absent/invalid value yields 0 (Open then applies the default).
func retainedTurnsFromEnv() int {
	v := strings.TrimSpace(os.Getenv("NEO_CONVERSATION_RETAINED_TURNS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

func (s *Store) pathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".jsonl")
}

func (s *Store) archivePathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".archive.jsonl")
}

// legacyPathLocked returns the old single-JSON path (daemon-shaped). Only used
// for backward-compatible reads of pre-JSONL files.
func (s *Store) legacyPathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".json")
}

// loadLocked reads a record's HOT turns (the retained set); a missing/corrupt
// file yields an empty record (never an error to the caller). It reads the
// JSONL hot file, falling back to the legacy single-JSON .json shape written
// by the daemon / pre-ring-buffer Neo. A partial (crash-truncated) final JSONL
// line is silently skipped — crash-atomic. Caller MUST hold s.mu.
func (s *Store) loadLocked(convID string) *Record {
	rec := &Record{ConversationID: convID}
	path := s.pathLocked(convID)
	if path == "" {
		return rec
	}
	turns, updated := readJSONLTurns(path)
	if turns != nil {
		rec.Turns = turns
		rec.Updated = updated
		return rec
	}
	// Fall back to the legacy single-JSON shape (daemon / pre-ring-buffer).
	legacy := s.legacyPathLocked(convID)
	if legacy == "" {
		return rec
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		return rec
	}
	if jerr := json.Unmarshal(data, rec); jerr != nil {
		return &Record{ConversationID: convID}
	}
	return rec
}

// readJSONLTurns parses a newline-delimited JSON turn file. A line that fails
// to unmarshal (including a crash-truncated final line with no trailing
// newline) is skipped — crash-atomic. Returns (turns, latestUpdated).
func readJSONLTurns(path string) ([]Turn, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}
	}
	defer f.Close()
	var (
		turns  []Turn
		latest time.Time
	)
	sc := bufio.NewScanner(f)
	// A single turn line is bounded (a turn's text), but allow generous headroom
	// so large assistant answers are not truncated.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var t Turn
		if jerr := json.Unmarshal(line, &t); jerr != nil {
			// Skip unparseable lines (crash-truncated partial writes).
			continue
		}
		turns = append(turns, t)
		if t.TS.After(latest) {
			latest = t.TS
		}
	}
	if err := sc.Err(); err != nil {
		// On read error, return whatever was parsed so far (best-effort).
		return turns, latest
	}
	return turns, latest
}

// appendJSONLTurn appends one turn as a single JSON line to path. O_APPEND +
// a single Write makes the line atomically visible (POSIX) — no reader can see
// a half-written line. Crash-atomic: a torn write yields an unparseable final
// line that readJSONLTurns skips.
func appendJSONLTurn(path string, turn Turn) error {
	b, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Append records one turn and persists it as a single JSONL line append (O(1) in
// the turn size — no full-file rewrite). Best-effort: IO errors are logged,
// never fatal. A blank conversation id or text is ignored. When the hot set
// exceeds the retained-turn cap, the overflow rolls to the durable archive
// (rename-based atomic write) — turns are NOT dropped. Crash-atomic: each turn
// is one atomic line append; a torn final line is skipped on load.
func (s *Store) Append(convID string, turn Turn) {
	if !s.Enabled() || convID == "" || strings.TrimSpace(turn.Text) == "" {
		return
	}
	if turn.TS.IsZero() {
		turn.TS = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: mkdir %s: %v\n", s.dir, err)
		return
	}

	path := s.pathLocked(convID)
	if err := appendJSONLTurn(path, turn); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: append %s: %v\n", path, err)
		return
	}

	// Enforce the retained-turn cap: if the hot set exceeds the cap, roll the
	// oldest overflow turns to the durable archive (atomic tmp+rename).
	s.rollIfOverCapLocked(convID)
}

// rollIfOverCapLocked moves the oldest overflow turns from the hot .jsonl to
// the archive when the hot set exceeds the retained cap. The archive rollup
// uses the rename-based atomic write (tmp+rename) — the ONLY place that path
// is used now. Caller MUST hold s.mu.
func (s *Store) rollIfOverCapLocked(convID string) {
	turns, _ := readJSONLTurns(s.pathLocked(convID))
	cap := s.retainedTurns
	if cap <= 0 {
		cap = DefaultRetainedTurns
	}
	if len(turns) <= cap {
		return
	}
	overflow := len(turns) - cap
	roll := turns[:overflow]     // oldest, move to archive
	keep := turns[overflow:]     // newest, stay hot

	archivePath := s.archivePathLocked(convID)
	if archivePath == "" {
		return
	}

	// Append the rolled turns to the existing archive (or create it).
	for _, t := range roll {
		_ = appendJSONLTurn(archivePath, t)
	}

	// Rewrite the hot file to contain only the retained set. This is the
	// compaction/rollup step — the rename-based atomic write lives here.
	keepJSONL := buildJSONL(keep)
	tmp := s.pathLocked(convID) + ".rollup.tmp"
	if err := os.WriteFile(tmp, keepJSONL, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rollup write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.pathLocked(convID)); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rollup rename %s: %v\n", s.pathLocked(convID), err)
		_ = os.Remove(tmp)
	}
}

// buildJSONL serializes a turn slice as newline-delimited JSON.
func buildJSONL(turns []Turn) []byte {
	var buf bytes.Buffer
	for _, t := range turns {
		b, _ := json.Marshal(t)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// AppendUser / AppendAssistant are thin helpers for the two turn kinds.
func (s *Store) AppendUser(convID, text string) {
	s.Append(convID, Turn{Role: "user", Text: text})
}

func (s *Store) AppendAssistant(convID, intentID, text string) {
	s.Append(convID, Turn{Role: "assistant", Text: text, IntentID: intentID})
}

// Recent returns the last n turns (oldest-first) from the full history, or nil
// when there are none / persistence is disabled. n spans the hot set AND the
// archive so no turn is unreachable.
func (s *Store) Recent(convID string, n int) []Turn {
	if !s.Enabled() || convID == "" || n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.historyLocked(convID)
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// Get returns the hot (bounded) turn log for one conversation, or nil when
// there are none / persistence is disabled. For the full history including
// archived turns use History.
func (s *Store) Get(convID string) *Record {
	if !s.Enabled() || convID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.loadLocked(convID)
	if len(rec.Turns) == 0 {
		return nil
	}
	return rec
}

// Archived returns the rolled (durable) turns for one conversation — the
// overflow that scrolled past the retained cap. Oldest-first, or nil when
// there are none / persistence is disabled.
func (s *Store) Archived(convID string) []Turn {
	if !s.Enabled() || convID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, _ := readJSONLTurns(s.archivePathLocked(convID))
	return turns
}

// History returns the FULL turn log (archive + hot), oldest-first, so no turn
// is ever unreachable. Returns nil when there are none / persistence disabled.
func (s *Store) History(convID string) []Turn {
	if !s.Enabled() || convID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyLocked(convID)
}

// historyLocked is the full-history read (archive + hot), oldest-first. Caller
// MUST hold s.mu.
func (s *Store) historyLocked(convID string) []Turn {
	archived, _ := readJSONLTurns(s.archivePathLocked(convID))
	hot := s.loadLocked(convID).Turns
	if len(archived) == 0 {
		return hot
	}
	if len(hot) == 0 {
		return archived
	}
	out := make([]Turn, 0, len(archived)+len(hot))
	out = append(out, archived...)
	out = append(out, hot...)
	return out
}

// List returns a summary of every persisted conversation, newest-first.
// Best-effort: unreadable files are skipped. Counts cover the FULL history
// (archive + hot) so turn_count reflects every retained turn.
func (s *Store) List() []Summary {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		convID := ""
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			convID = strings.TrimSuffix(name, ".jsonl")
		case strings.HasSuffix(name, ".archive.jsonl"):
			// Archive files are summarized via their hot sibling (below).
			convID = ""
		case strings.HasSuffix(name, ".json"):
			convID = strings.TrimSuffix(name, ".json")
		default:
			continue
		}
		if convID == "" {
			continue
		}
		// Skip archive-only entries: a conversation is listed once, via its
		// .jsonl (or legacy .json). An archive file with no hot sibling means
		// the hot set is empty — list it under the archive's convID.
		if strings.HasSuffix(name, ".archive.jsonl") {
			continue
		}
		hot := s.loadLocked(convID)
		all := s.historyLocked(convID)
		if len(all) == 0 {
			continue
		}
		rec := &Record{
			ConversationID: convID,
			Title:          hot.Title,
			Turns:          all,
			Updated:        hot.Updated,
		}
		out = append(out, Summary{
			ConversationID: convID,
			Title:          title(rec),
			Preview:        preview(rec),
			TurnCount:      len(all),
			Updated:        rec.Updated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// title derives a short human label: the explicit Title, else the first user
// turn trimmed, else a generic fallback.
func title(rec *Record) string {
	if rec.Title != "" {
		return rec.Title
	}
	for _, t := range rec.Turns {
		if t.Role == "user" && t.Text != "" {
			return truncateLabel(t.Text, 60)
		}
	}
	return "New chat"
}

// preview returns the most recent turn's text, trimmed, for the sidebar.
func preview(rec *Record) string {
	if len(rec.Turns) == 0 {
		return ""
	}
	last := rec.Turns[len(rec.Turns)-1]
	return truncateLabel(last.Text, 100)
}

// truncateLabel collapses whitespace and clamps to n runes with an ellipsis.
func truncateLabel(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Dir resolves Neo's conversation directory. An explicit override wins; else it
// is derived from the cortex root's parent (matching the daemon, so Neo and the
// daemon share /data/conversations and history is unified). Returns "" when
// neither is available (persistence disabled).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "conversations")
	}
	return ""
}
