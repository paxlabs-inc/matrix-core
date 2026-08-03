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

	"matrix/vault"
)

// Store types and schema bound into each record's associated data, so a sealed
// line cannot be replayed across users, stores, conversations, or positions.
const (
	storeConvHot     = "neo.conversation"
	storeConvArchive = "neo.conversation.archive"
	storeConvMeta    = "neo.conversation.meta"
	schemaTurnV1     = "turn.v1"
	schemaMetaV1     = "meta.v1"
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

// Summary is the compact list shape for GET /conversations. Project is the
// workbench project tag ("" for untagged dashboard threads) — additive
// omitempty, so the pre-workbench wire shape is unchanged for untagged
// conversations.
type Summary struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	Preview        string    `json:"preview"`
	TurnCount      int       `json:"turn_count"`
	Updated        time.Time `json:"updated"`
	Project        string    `json:"project,omitempty"`
	// Archived hides the thread from the default sidebar (CHAT-01). Additive
	// omitempty: the pre-CHAT-01 wire shape is unchanged for live threads.
	Archived bool `json:"archived,omitempty"`
	// ForkedFrom records fork provenance for the sidebar's "forked from" hint.
	ForkedFrom string `json:"forked_from,omitempty"`
}

// Store is Neo's durable conversation memory. One mutex guards all access;
// conversations are small and Neo serves one user, so this is plenty fast.
type Store struct {
	mu  sync.Mutex
	dir string

	// retainedTurns caps the hot .jsonl turn count; overflow rolls to the
	// archive. Zero means use DefaultRetainedTurns at append time.
	retainedTurns int

	// vault seals each JSONL record when encrypting; nil = plaintext dev/CLI.
	// user is the DID bound into each record's associated data. seqs caches the
	// next record sequence per file so appends stay O(1) (seeded by one line
	// count) while binding each ciphertext to its position.
	vault *vault.Session
	user  string
	seqs  map[string]uint64
}

// SetVault wires the fail-closed data-at-rest session and the owning user DID
// into the store. Called once at engine assembly; a nil session leaves the
// store writing legacy plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = sess
	s.user = user
	s.mu.Unlock()
}

// ad reconstructs the associated data for a record from where it lives — never
// stored, so a record moved between users, stores, conversations, or positions
// fails authentication.
func (s *Store) ad(storeType, stream string, seq uint64) vault.AD {
	return vault.AD{User: s.user, Store: storeType, Stream: stream, Seq: seq, Schema: schemaTurnV1}
}

// nextSeqLocked returns the next record sequence for a file, seeding the cache
// once by counting existing lines so the seq binds each record to its position
// while keeping appends O(1). Caller MUST hold s.mu.
func (s *Store) nextSeqLocked(path string) uint64 {
	if s.seqs == nil {
		s.seqs = map[string]uint64{}
	}
	if n, ok := s.seqs[path]; ok {
		return n
	}
	n := countLines(path)
	s.seqs[path] = n
	return n
}

// countLines counts non-empty lines in a JSONL file (0 when absent).
func countLines(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var n uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		n++
	}
	return n
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
	turns, updated := s.readTurns(storeConvHot, convID, path)
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

// readTurns parses a newline-delimited turn file, decrypting each record under
// the reconstructed associated data (legacy plaintext lines pass through until
// migrated). A line that fails to decrypt or unmarshal — including a wrong-key,
// tampered, or crash-truncated final line — is skipped; the record sequence
// still advances so surviving records stay bound to their positions. Returns
// (turns, latestUpdated).
func (s *Store) readTurns(storeType, stream, path string) ([]Turn, time.Time) {
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
	// A turn line is bounded (a turn's text) but allow generous headroom (with
	// extra room for base64 ciphertext overhead) so large answers are intact.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var seq uint64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		plain, derr := s.vault.DecodeLine(s.ad(storeType, stream, seq), line)
		seq++
		if derr != nil {
			// Skip wrong-key / tampered / crash-truncated lines.
			continue
		}
		var t Turn
		if jerr := json.Unmarshal(plain, &t); jerr != nil {
			continue
		}
		turns = append(turns, t)
		if t.TS.After(latest) {
			latest = t.TS
		}
	}
	if err := sc.Err(); err != nil {
		return turns, latest
	}
	return turns, latest
}

// appendTurn appends one turn as a single (sealed, when encrypting) JSONL line.
// O_APPEND + a single Write makes the line atomically visible (POSIX) — no
// reader sees a half-written line. Crash-atomic: a torn write yields an
// unparseable final line that readTurns skips. Caller MUST hold s.mu.
func (s *Store) appendTurn(storeType, stream, path string, turn Turn) error {
	b, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	seq := s.nextSeqLocked(path)
	line, err := s.vault.EncodeLine(s.ad(storeType, stream, seq), b)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.seqs[path] = seq + 1
	return nil
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

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: mkdir %s: %v\n", s.dir, err)
		return
	}

	path := s.pathLocked(convID)
	if err := s.appendTurn(storeConvHot, convID, path, turn); err != nil {
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
	turns, _ := s.readTurns(storeConvHot, convID, s.pathLocked(convID))
	cap := s.retainedTurns
	if cap <= 0 {
		cap = DefaultRetainedTurns
	}
	if len(turns) <= cap {
		return
	}
	overflow := len(turns) - cap
	roll := turns[:overflow] // oldest, move to archive
	keep := turns[overflow:] // newest, stay hot

	archivePath := s.archivePathLocked(convID)
	if archivePath == "" {
		return
	}

	// Append the rolled turns to the existing archive (or create it). Each is
	// sealed under the archive store type at its own growing archive position.
	for _, t := range roll {
		_ = s.appendTurn(storeConvArchive, convID, archivePath, t)
	}

	// Rewrite the hot file to contain only the retained set, re-sealing each
	// kept turn at its NEW hot position. This is the compaction/rollup step —
	// the rename-based atomic write lives here.
	keepJSONL, err := s.buildJSONL(storeConvHot, convID, keep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rollup seal %s: %v\n", convID, err)
		return
	}
	tmp := s.pathLocked(convID) + ".rollup.tmp"
	if err := os.WriteFile(tmp, keepJSONL, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rollup write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.pathLocked(convID)); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rollup rename %s: %v\n", s.pathLocked(convID), err)
		_ = os.Remove(tmp)
		return
	}
	// The hot file now holds exactly len(keep) records at positions 0..n-1.
	s.seqs[s.pathLocked(convID)] = uint64(len(keep))
}

// buildJSONL serializes a turn slice as newline-delimited (sealed, when
// encrypting) JSON, binding each record to its position starting at seq 0.
// Caller MUST hold s.mu.
func (s *Store) buildJSONL(storeType, stream string, turns []Turn) ([]byte, error) {
	var buf bytes.Buffer
	for i, t := range turns {
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		line, err := s.vault.EncodeLine(s.ad(storeType, stream, uint64(i)), b)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// convMeta is the per-conversation sidecar (<convID>.meta.json): durable
// metadata that is not a turn — the workbench project tag and the
// personalization-interview flag (ORACLE task 5.3).
type convMeta struct {
	Project string `json:"project,omitempty"`
	// Interview marks a personalization-interview conversation: the session
	// mints the interview agent (guided charter + confirmation-gated save
	// tool) and writeback extraction is structurally excluded (req 12.3).
	Interview bool `json:"interview,omitempty"`
	// Title is an explicit user rename (CHAT-01). When set it overrides the
	// derived first-user-turn label everywhere the title surfaces.
	Title string `json:"title,omitempty"`
	// Archived hides a conversation from the default sidebar without deleting
	// it (CHAT-01). Reversible via unarchive.
	Archived bool `json:"archived,omitempty"`
	// ForkedFrom / ForkedAtTurn record fork provenance: the parent
	// conversation id and the (exclusive) turn count copied into this fork.
	ForkedFrom   string `json:"forked_from,omitempty"`
	ForkedAtTurn int    `json:"forked_at_turn,omitempty"`
	// RecoverySource / RecoveryHandoff bind a deliberately recentered thread
	// to the compact context of the prior thread without copying those turns
	// into the user-visible new conversation.
	RecoverySource  string `json:"recovery_source,omitempty"`
	RecoveryHandoff string `json:"recovery_handoff,omitempty"`
}

func (s *Store) metaPathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, convID+".meta.json")
}

func (s *Store) loadMetaLocked(convID string) convMeta {
	var m convMeta
	path := s.metaPathLocked(convID)
	if path == "" {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	// Sniff the header: a sealed sidecar decrypts under the reconstructed AD;
	// a legacy plaintext sidecar parses as today. A wrong-key/tampered sealed
	// file yields the zero meta rather than leaking bytes.
	if vault.IsVault(data) {
		uv := s.vault.UserVault()
		if uv == nil {
			return m
		}
		plain, oerr := uv.OpenFile(s.metaAD(convID), data)
		if oerr != nil {
			return m
		}
		data = plain
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// metaAD reconstructs the associated data for a conversation's meta sidecar —
// never stored, so a sidecar moved between users or conversations fails auth.
func (s *Store) metaAD(convID string) vault.AD {
	return vault.AD{User: s.user, Store: storeConvMeta, Stream: convID, Schema: schemaMetaV1}
}

// SetProject tags a conversation with a workbench project id (idempotent;
// atomic tmp+rename sidecar write). Best-effort like Append: IO errors are
// logged, never fatal.
func (s *Store) SetProject(convID, projectID string) {
	if !s.Enabled() || convID == "" || strings.TrimSpace(projectID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadMetaLocked(convID)
	if m.Project == projectID {
		return
	}
	m.Project = strings.TrimSpace(projectID)
	_ = s.saveMetaLocked(convID, m)
}

// saveMetaLocked persists a conversation's meta sidecar (atomic tmp+rename,
// sealed at rest when the vault is wired). Caller MUST hold s.mu.
func (s *Store) saveMetaLocked(convID string, m convMeta) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: mkdir %s: %v\n", s.dir, err)
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Seal the sidecar at rest (fail-closed when the vault is required); nil
	// session = legacy plaintext. tmp+rename atomicity preserved below.
	data, err = s.vault.MaybeSealFile(s.metaAD(convID), data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: seal meta %s: %v\n", convID, err)
		return err
	}
	path := s.metaPathLocked(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: write %s: %v\n", tmp, err)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SetInterview marks a conversation as a personalization interview (ORACLE
// task 5.3). Idempotent; atomic sidecar write.
func (s *Store) SetInterview(convID string) {
	if !s.Enabled() || convID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadMetaLocked(convID)
	if m.Interview {
		return
	}
	m.Interview = true
	_ = s.saveMetaLocked(convID, m)
}

// IsInterview reports whether a conversation is a personalization interview.
func (s *Store) IsInterview(convID string) bool {
	if !s.Enabled() || convID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadMetaLocked(convID).Interview
}

// Project returns a conversation's workbench project tag ("" untagged).
func (s *Store) Project(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadMetaLocked(convID).Project
}

// SetRecoveryHandoff creates the metadata for a fresh recentered conversation.
// The new conversation intentionally has no visible turns until the user sends
// its first message; rebuildAgent reads this sealed sidecar before that send.
// The source project tag is carried forward so coding work stays in the same
// workspace. Existing conversations are never overwritten.
func (s *Store) SetRecoveryHandoff(convID, sourceID, handoff string) bool {
	convID = strings.TrimSpace(convID)
	sourceID = strings.TrimSpace(sourceID)
	handoff = strings.TrimSpace(handoff)
	if !s.Enabled() || convID == "" || handoff == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.historyLocked(convID)) > 0 {
		return false
	}
	if existing := s.loadMetaLocked(convID); existing.RecoveryHandoff != "" {
		return false
	}
	meta := convMeta{RecoverySource: sourceID, RecoveryHandoff: handoff}
	if sourceID != "" {
		meta.Project = s.loadMetaLocked(sourceID).Project
	}
	return s.saveMetaLocked(convID, meta) == nil
}

// RecoveryHandoff returns the prior-thread context attached to a recentered
// conversation. It is model-facing session context, not a visible chat turn.
func (s *Store) RecoveryHandoff(convID string) (sourceID, handoff string) {
	if !s.Enabled() || strings.TrimSpace(convID) == "" {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.loadMetaLocked(strings.TrimSpace(convID))
	return meta.RecoverySource, meta.RecoveryHandoff
}

// AppendUser / AppendAssistant are thin helpers for the two turn kinds.
//
// intentID is the owning run id minted at dispatch time. Persisting it on the
// user turn is the durable anchor a reload/relogin uses to reattach the live
// stream — the client (and the GET /conversations/{id} live_run signal) reads
// it back to decide whether the run is still in flight. An empty intent_id is
// fine (omitempty preserves the pre-F1 wire shape exactly for direct replies
// and tests that don't model a run).
func (s *Store) AppendUser(convID, intentID, text string) {
	s.Append(convID, Turn{Role: "user", Text: text, IntentID: intentID})
}

func (s *Store) AppendAssistant(convID, intentID, text string) {
	s.Append(convID, Turn{Role: "assistant", Text: text, IntentID: intentID})
}

// Meta is the CHAT-01 read view of a conversation's durable, non-turn state:
// its explicit title, archived flag, and fork provenance. Zero value when the
// conversation has no sidecar / persistence is disabled.
type Meta struct {
	Title        string
	Archived     bool
	ForkedFrom   string
	ForkedAtTurn int
}

// Meta returns a conversation's durable metadata (title/archived/provenance).
func (s *Store) Meta(convID string) Meta {
	if !s.Enabled() || convID == "" {
		return Meta{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadMetaLocked(convID)
	return Meta{Title: m.Title, Archived: m.Archived, ForkedFrom: m.ForkedFrom, ForkedAtTurn: m.ForkedAtTurn}
}

// Exists reports whether a conversation has any persisted turn (hot, archived,
// or legacy). Used by the CHAT-01 handlers to reject rename/archive/fork of a
// missing/deleted thread with an explicit error rather than silently creating.
func (s *Store) Exists(convID string) bool {
	if !s.Enabled() || convID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.historyLocked(convID)) > 0
}

// Rename sets an explicit durable title for a conversation (CHAT-01). An empty
// title clears the override, restoring the derived first-user-turn label.
// Returns false when persistence is disabled or the conversation does not
// exist (so the caller can answer an explicit not-found).
func (s *Store) Rename(convID, newTitle string) bool {
	return s.UpdateMeta(convID, &newTitle, nil)
}

// SetArchived hides (or restores) a conversation from the default sidebar
// without deleting it (CHAT-01). Returns false when the conversation does not
// exist / persistence is disabled.
func (s *Store) SetArchived(convID string, archived bool) bool {
	return s.UpdateMeta(convID, nil, &archived)
}

// UpdateMeta applies a rename and/or archive change with one atomic sidecar
// replacement, so a combined PATCH cannot leave only half its fields updated.
func (s *Store) UpdateMeta(convID string, title *string, archived *bool) bool {
	if !s.Enabled() || convID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.historyLocked(convID)) == 0 {
		return false
	}
	m := s.loadMetaLocked(convID)
	if title != nil {
		m.Title = strings.TrimSpace(*title)
	}
	if archived != nil {
		m.Archived = *archived
	}
	return s.saveMetaLocked(convID, m) == nil
}

// Delete permanently removes a conversation's turn logs and metadata sidecar
// (hot, archive, legacy single-JSON, and meta). Returns false when the
// conversation did not exist. Best-effort per-file; a missing file is not an
// error. The trace timeline is a sibling store the handler cleans up
// separately (this store owns only the chat-thread files).
func (s *Store) Delete(convID string) bool {
	if !s.Enabled() || convID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.historyLocked(convID)) == 0 {
		return false
	}
	paths := []string{
		s.pathLocked(convID),
		s.archivePathLocked(convID),
		s.legacyPathLocked(convID),
		s.metaPathLocked(convID),
	}
	type movedFile struct {
		from string
		to   string
	}
	var moved []movedFile
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false
		}
		tomb := p + ".delete.tmp"
		_ = os.Remove(tomb)
		if err := os.Rename(p, tomb); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(moved[i].to, moved[i].from)
			}
			return false
		}
		moved = append(moved, movedFile{from: p, to: tomb})
		delete(s.seqs, p)
	}
	for _, f := range moved {
		if err := os.Remove(f.to); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "neo/conversation: delete %s: %v\n", f.to, err)
		}
	}
	return true
}

// Fork creates a new conversation (newID) containing exactly the chronological
// prefix of srcID's full history through upToTurn (inclusive), with parent/turn
// provenance recorded in the fork's meta sidecar. The fork has an independent
// future: later turns on either thread do not affect the other. Returns false
// when the source is missing, newID is blank/taken, or upToTurn is out of the
// 1..len(history) range.
func (s *Store) Fork(srcID, newID string, upToTurn int) bool {
	if !s.Enabled() || srcID == "" || strings.TrimSpace(newID) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.historyLocked(newID)) > 0 {
		return false // never clobber an existing conversation
	}
	hist := s.historyLocked(srcID)
	if len(hist) == 0 || upToTurn < 1 || upToTurn > len(hist) {
		return false
	}
	prefix := hist[:upToTurn]

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: mkdir %s: %v\n", s.dir, err)
		return false
	}
	// Write the prefix into the fork's hot file, re-sealed at its new stream +
	// positions (each fork line is bound to newID, so it cannot be replayed
	// from the parent).
	jsonl, err := s.buildJSONL(storeConvHot, newID, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: fork seal %s: %v\n", newID, err)
		return false
	}
	path := s.pathLocked(newID)
	tmp := path + ".fork.tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, jsonl, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "neo/conversation: fork write %s: %v\n", tmp, err)
		return false
	}

	// Record provenance; carry the parent's project tag so the fork stays in
	// the same workbench context.
	m := convMeta{
		Project:      s.loadMetaLocked(srcID).Project,
		ForkedFrom:   srcID,
		ForkedAtTurn: upToTurn,
	}
	if err := s.saveMetaLocked(newID, m); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(s.metaPathLocked(newID))
		return false
	}
	if s.seqs == nil {
		s.seqs = map[string]uint64{}
	}
	s.seqs[path] = uint64(len(prefix))
	return true
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
	// An explicit user rename (CHAT-01) overrides the derived label.
	if mt := s.loadMetaLocked(convID).Title; mt != "" {
		rec.Title = mt
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
	turns, _ := s.readTurns(storeConvArchive, convID, s.archivePathLocked(convID))
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
	archived, _ := s.readTurns(storeConvArchive, convID, s.archivePathLocked(convID))
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
		case strings.HasSuffix(name, ".meta.json"):
			// Metadata sidecars are read via their conversation, never listed.
			continue
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
		meta := s.loadMetaLocked(convID)
		recTitle := hot.Title
		if meta.Title != "" {
			recTitle = meta.Title
		}
		rec := &Record{
			ConversationID: convID,
			Title:          recTitle,
			Turns:          all,
			Updated:        hot.Updated,
		}
		out = append(out, Summary{
			ConversationID: convID,
			Title:          title(rec),
			Preview:        preview(rec),
			TurnCount:      len(all),
			Updated:        rec.Updated,
			Project:        meta.Project,
			Archived:       meta.Archived,
			ForkedFrom:     meta.ForkedFrom,
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
