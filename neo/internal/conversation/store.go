// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package conversation is Neo's durable chat-thread memory.
//
// Neo's sessions are otherwise in-process only: turns vanish on restart and
// the front had no way to list or reopen a past thread (the /conversations
// routes proxied to the MCL daemon, which never saw a Neo conversation). This
// store fixes that — it persists each turn (the user's message and Neo's
// closing answer) as one JSON file per conversation_id, so history survives
// reloads, new chats, suspend, and redeploy.
//
// The on-disk shape is byte-compatible with the daemon's own conversation
// store (executor/cmd/mcl-execute/daemon_conversation.go) and Neo derives the
// SAME directory (filepath.Dir(cortexRoot)/conversations = /data/conversations
// in prod), so a user's pre-Neo daemon threads and their new Neo threads list
// together as one unified history. Since the client now talks only to Neo,
// Neo is the single writer.
//
// It is a pure side-channel: it never touches cortex, signs anything, or
// perturbs replay — conversation continuity and the audit/replay chain are
// independent storage.
package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxTurns bounds retained turns per conversation so a long thread can't
	// grow its file without limit.
	maxTurns = 80
	// DefaultRecallTurns is how many recent turns are seeded back into a
	// resumed conversation's working context by default.
	DefaultRecallTurns = 16
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
	max int
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir), max: maxTurns}
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

func (s *Store) pathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".json")
}

// loadLocked reads a record; a missing/corrupt file yields an empty record
// (never an error to the caller). Caller MUST hold s.mu.
func (s *Store) loadLocked(convID string) *Record {
	rec := &Record{ConversationID: convID}
	path := s.pathLocked(convID)
	if path == "" {
		return rec
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rec
	}
	if jerr := json.Unmarshal(data, rec); jerr != nil {
		return &Record{ConversationID: convID}
	}
	return rec
}

// Append records one turn, trims to the retention bound, and persists
// atomically (tmp + rename). Best-effort: IO errors are logged, never fatal. A
// blank conversation id or text is ignored.
func (s *Store) Append(convID string, turn Turn) {
	if !s.Enabled() || convID == "" || strings.TrimSpace(turn.Text) == "" {
		return
	}
	if turn.TS.IsZero() {
		turn.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.loadLocked(convID)
	rec.Turns = append(rec.Turns, turn)
	if len(rec.Turns) > s.max {
		rec.Turns = rec.Turns[len(rec.Turns)-s.max:]
	}
	rec.Updated = time.Now().UTC()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: mkdir %s: %v\n", s.dir, err)
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: marshal %s: %v\n", convID, err)
		return
	}
	path := s.pathLocked(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
	}
}

// AppendUser / AppendAssistant are thin helpers for the two turn kinds.
func (s *Store) AppendUser(convID, text string) {
	s.Append(convID, Turn{Role: "user", Text: text})
}

func (s *Store) AppendAssistant(convID, intentID, text string) {
	s.Append(convID, Turn{Role: "assistant", Text: text, IntentID: intentID})
}

// Recent returns the last n turns (oldest-first), or nil when there are none /
// persistence is disabled.
func (s *Store) Recent(convID string, n int) []Turn {
	if !s.Enabled() || convID == "" || n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.loadLocked(convID)
	if len(rec.Turns) <= n {
		return rec.Turns
	}
	return rec.Turns[len(rec.Turns)-n:]
}

// Get returns the full (bounded) turn log for one conversation, or nil when
// there are none / persistence is disabled.
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

// List returns a summary of every persisted conversation, newest-first.
// Best-effort: unreadable files are skipped.
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
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		convID := strings.TrimSuffix(name, ".json")
		rec := s.loadLocked(convID)
		if len(rec.Turns) == 0 {
			continue
		}
		out = append(out, Summary{
			ConversationID: convID,
			Title:          title(rec),
			Preview:        preview(rec),
			TurnCount:      len(rec.Turns),
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
