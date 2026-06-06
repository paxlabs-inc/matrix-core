// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_conversation.go — durable per-conversation memory for the
// Liaison front door.
//
// The Liaison is a stateless side-channel: every /chat message was
// previously triaged and dispatched in isolation, with conversation_id
// used only as a client-side correlation label. That made the agent
// forget the thread between turns — a follow-up like "maybe try paxscan"
// was judged with no knowledge of the prior "block time for paxeer"
// request, so it asked "what do you want me to do?". This store closes
// that gap: it persists each turn (the user's message and the agent's
// closing answer) keyed by conversation_id, on the same snapshotted
// volume as the async inbox, so the front door has real multi-turn
// memory that survives suspend, crash, and redeploy.
//
// HARD INVARIANT: this is a side-channel store, exactly like the Liaison
// it serves. It NEVER touches cortex, signs envelopes, or perturbs the
// plan/walk, so it cannot affect the D11 replay byte-identity invariant.
// Conversation continuity and the cortex audit/replay chain are kept on
// separate, independent storage.

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
	// convMaxTurns bounds how many turns are retained per conversation
	// so a long-running thread cannot grow the file without limit.
	convMaxTurns = 60
	// convRecallTurns is how many recent turns are recalled into the
	// triage / closing-answer context by default.
	convRecallTurns = 12
)

// convTurn is one durable line of a conversation: who spoke, what they
// said, and (for agent turns) which run produced it.
type convTurn struct {
	Role     string    `json:"role"` // "user" | "assistant"
	Text     string    `json:"text"`
	IntentID string    `json:"intent_id,omitempty"`
	TS       time.Time `json:"ts"`
}

// conversationRecord is the persisted shape: an append-only (bounded)
// list of turns for one conversation_id.
type conversationRecord struct {
	ConversationID string     `json:"conversation_id"`
	Title          string     `json:"title,omitempty"`
	Turns          []convTurn `json:"turns"`
	Updated        time.Time  `json:"updated"`
}

// conversationSummary is the compact list shape for GET /conversations:
// enough to render a sidebar entry (title + preview + timestamp) without
// shipping the whole turn log.
type conversationSummary struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	Preview        string    `json:"preview"`
	TurnCount      int       `json:"turn_count"`
	Updated        time.Time `json:"updated"`
}

// conversationStore is the daemon-wide durable conversation memory. A
// single mutex guards all reads/writes; conversations are small and the
// daemon serves one user, so this is more than fast enough.
type conversationStore struct {
	mu  sync.Mutex
	dir string
	max int
}

// conversationDir derives the durable conversation directory from the
// daemon's persistent roots, co-located with the snapshotted data tree
// (parent of cortex-root → /data/conversations in prod), falling back to
// the transcripts parent. Empty disables persistence.
func conversationDir(cortexRoot, transcriptsDir string) string {
	switch {
	case cortexRoot != "":
		return filepath.Join(filepath.Dir(cortexRoot), "conversations")
	case transcriptsDir != "":
		return filepath.Join(filepath.Dir(transcriptsDir), "conversations")
	default:
		return ""
	}
}

// newConversationStore builds the store. An empty dir yields a no-op
// store (Append/Recent do nothing) so dev/CLI daemons run unchanged.
func newConversationStore(dir string) *conversationStore {
	return &conversationStore{dir: dir, max: convMaxTurns}
}

func (s *conversationStore) enabled() bool { return s != nil && s.dir != "" }

func (s *conversationStore) pathLocked(convID string) string {
	if !s.enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".json")
}

// loadLocked reads a conversation record from disk. A missing/corrupt
// file yields an empty record (never an error to the caller). Caller
// MUST hold s.mu.
func (s *conversationStore) loadLocked(convID string) *conversationRecord {
	rec := &conversationRecord{ConversationID: convID}
	path := s.pathLocked(convID)
	if path == "" {
		return rec
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rec
	}
	if jerr := json.Unmarshal(data, rec); jerr != nil {
		return &conversationRecord{ConversationID: convID}
	}
	return rec
}

// Append records one turn, trims to the retention bound, and persists
// atomically. Best-effort: an IO error is logged, never fatal. A blank
// conversation id or text is ignored.
func (s *conversationStore) Append(convID string, turn convTurn) {
	if !s.enabled() || convID == "" || turn.Text == "" {
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
		fmt.Fprintf(os.Stderr, "conversation: mkdir %s: %v\n", s.dir, err)
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conversation: marshal %s: %v\n", convID, err)
		return
	}
	path := s.pathLocked(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "conversation: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "conversation: rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
	}
}

// Recent returns the last n turns of a conversation (oldest-first), or
// nil when there are none / persistence is disabled.
func (s *conversationStore) Recent(convID string, n int) []convTurn {
	if !s.enabled() || convID == "" || n <= 0 {
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

// AppendUser / AppendAssistant are thin helpers for the two turn kinds.
func (s *conversationStore) AppendUser(convID, text string) {
	s.Append(convID, convTurn{Role: "user", Text: text})
}

func (s *conversationStore) AppendAssistant(convID, intentID, text string) {
	s.Append(convID, convTurn{Role: "assistant", Text: text, IntentID: intentID})
}

// Get returns the full (bounded) turn log for one conversation, or nil
// when there are none / persistence is disabled.
func (s *conversationStore) Get(convID string) *conversationRecord {
	if !s.enabled() || convID == "" {
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
// It is bounded by the on-disk file set; the daemon serves one user so
// every file belongs to them. Best-effort: unreadable files are skipped.
func (s *conversationStore) List() []conversationSummary {
	if !s.enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := make([]conversationSummary, 0, len(entries))
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
		out = append(out, conversationSummary{
			ConversationID: convID,
			Title:          conversationTitle(rec),
			Preview:        conversationPreview(rec),
			TurnCount:      len(rec.Turns),
			Updated:        rec.Updated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// conversationTitle derives a short human label for a conversation: the
// explicit Title when set, else the first user turn trimmed to a sane
// length, else a generic fallback.
func conversationTitle(rec *conversationRecord) string {
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

// conversationPreview returns the most recent turn's text, trimmed, for
// the sidebar second line.
func conversationPreview(rec *conversationRecord) string {
	if len(rec.Turns) == 0 {
		return ""
	}
	last := rec.Turns[len(rec.Turns)-1]
	return truncateLabel(last.Text, 100)
}

// truncateLabel collapses whitespace and clamps to n runes with an
// ellipsis when longer.
func truncateLabel(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// renderConversationHistory formats recent turns into a compact block
// for an LLM prompt (triage / closing answer). Returns "" when empty.
func renderConversationHistory(turns []convTurn) string {
	if len(turns) == 0 {
		return ""
	}
	var b []byte
	for _, t := range turns {
		role := t.Role
		if role == "" {
			role = "user"
		}
		b = append(b, '-')
		b = append(b, ' ')
		b = append(b, []byte(role)...)
		b = append(b, ':', ' ')
		b = append(b, []byte(t.Text)...)
		b = append(b, '\n')
	}
	return string(b)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
