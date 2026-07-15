// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package automatrixlog is the durable in-app record of Automatrix completions —
// the canonical, replayable surface for "Neo finished something for you while
// you were away" (req 6.1). When Neo genuinely completes an unprompted
// opportunity it writes one record here; the client renders these as an
// Automatrix inbox with an unread badge, regardless of whether the out-of-app
// ntfy/Apprise ping was delivered. The external ping is best-effort and may be
// lost; this record never is.
//
// It is a pure SIDECAR, mirroring neo/internal/trace: an append-only JSONL
// store on the machine volume that survives reload, suspend, redeploy, and
// reopen-from-history. It never touches cortex, signs anything, or perturbs the
// plan/walk. It obeys the consumer rule — it carries the RESULT, not the
// protocol: only the opportunity summary, a result summary, the conversation
// id, a created_at, and an unread flag. No Chronos/alarm/marker/cortex jargon,
// and NO secrets, tokens, or keys (req 6.5 / 8.5).
//
// Unlike the trace store (a hot-path broker tap that must never block), a
// completion record is written exactly once per genuinely-finished autonomous
// task — never on a hot path — and the inbox needs synchronous reads
// (List/Unread) plus a mutation (MarkRead). So this store serialises its disk
// access with a mutex rather than an async writer goroutine, while reusing the
// same crash-atomic JSONL persistence discipline as trace (O_APPEND single-line
// writes, tmp+rename rewrites, unparseable-line skip).
package automatrixlog

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matrix/vault"
)

// recordFile is the single per-daemon inbox file. The daemon is per-user, so
// one file holds that user/agent's completion inbox; the records carry no user
// id (the directory scoping is the ownership boundary), and no secrets.
const recordFile = "automatrix.complete.jsonl"

// Store type and schema bound into each inbox record's associated data, so a
// sealed line cannot be read across users or positions.
const (
	storeInbox   = "neo.automatrix.log"
	schemaInbox1 = "record.v1"
)

// Record is one durable Automatrix completion — an in-app inbox item. Its shape
// is the whole no-secrets guarantee: these are the only fields that exist.
type Record struct {
	ID                 string `json:"id"`
	OpportunitySummary string `json:"opportunity_summary"`
	ResultSummary      string `json:"result_summary"`
	ConversationID     string `json:"conversation_id"`
	CreatedAt          string `json:"created_at"`
	Read               bool   `json:"read"`
}

// Store is the durable Automatrix completion inbox. All disk access is
// serialised by mu; reads go straight to the filesystem under the lock, every
// write is crash-atomic, so a concurrent reader always sees a consistent file.
type Store struct {
	dir string
	mu  sync.Mutex

	// vault seals each inbox record line when encrypting; nil = plaintext
	// dev/CLI. user is the DID bound into each record's associated data.
	vault *vault.Session
	user  string
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store. Called once at engine assembly before any write; a nil session
// leaves the store writing legacy plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = sess
	s.user = user
	s.mu.Unlock()
}

// ad reconstructs the associated data for an inbox record at a given position —
// never stored, so a record moved between users or positions fails auth.
func (s *Store) ad(seq uint64) vault.AD {
	return vault.AD{User: s.user, Store: storeInbox, Seq: seq, Schema: schemaInbox1}
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// Append writes one completion record durably and returns the stored record
// (with its ID and CreatedAt filled in). A blank ID is assigned an unguessable
// random id; a blank CreatedAt is stamped now (UTC, RFC3339). New records are
// always unread. A disabled store returns the record unchanged with no error,
// so a result is never lost on a misconfigured dev run.
func (s *Store) Append(rec Record) (Record, error) {
	if rec.ID == "" {
		rec.ID = newID()
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	rec.Read = false
	if !s.Enabled() {
		return rec, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return rec, fmt.Errorf("automatrixlog: mkdir %s: %w", s.dir, err)
	}
	if err := s.appendJSONL(s.path(), rec); err != nil {
		return rec, fmt.Errorf("automatrixlog: append %s: %w", s.path(), err)
	}
	return rec, nil
}

// List returns the inbox newest-first (most recent completion at index 0), or
// nil when empty / disabled.
func (s *Store) List() []Record {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.readJSONL(s.path())
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}

// Unread returns the number of unread completion records — the in-app badge
// count. Zero when empty / disabled.
func (s *Store) Unread() int {
	if !s.Enabled() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.readJSONL(s.path()) {
		if !r.Read {
			n++
		}
	}
	return n
}

// MarkRead flips one record's unread flag via an atomic tmp+rename rewrite and
// reports whether a record actually changed (false when the id is unknown or it
// was already read). A disabled store reports false.
func (s *Store) MarkRead(id string) (bool, error) {
	if !s.Enabled() || id == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.readJSONL(s.path())
	changed := false
	for i := range recs {
		if recs[i].ID == id && !recs[i].Read {
			recs[i].Read = true
			changed = true
			break
		}
	}
	if !changed {
		return false, nil
	}
	if err := s.rewriteJSONL(s.path(), recs); err != nil {
		return false, fmt.Errorf("automatrixlog: rewrite %s: %w", s.path(), err)
	}
	return true, nil
}

func (s *Store) path() string {
	if !s.Enabled() {
		return ""
	}
	return filepath.Join(s.dir, recordFile)
}

// newID returns an unguessable record id (no time/user info leaked).
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ax_%d", time.Now().UnixNano())
	}
	return "ax_" + hex.EncodeToString(b[:])
}

// appendJSONL appends one record as a single (sealed, when encrypting) JSON
// line. O_APPEND + a single Write makes the line atomically visible (POSIX); a
// torn write yields an unparseable final line that readJSONL skips (crash-
// atomic). The record is bound to its on-disk position via the reconstructed AD.
func (s *Store) appendJSONL(path string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line, err := s.vault.EncodeLine(s.ad(countInboxLines(path)), b)
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
	return f.Close()
}

// countInboxLines counts non-empty lines in the inbox file (0 when absent) so a
// sealed append binds to its true position.
func countInboxLines(path string) uint64 {
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

// rewriteJSONL atomically replaces the file with recs (tmp+rename), used by
// MarkRead. A crash leaves either the old or the new file intact, never a torn
// one. Each record is re-sealed at its NEW position.
func (s *Store) rewriteJSONL(path string, recs []Record) error {
	var buf bytes.Buffer
	for i, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		line, err := s.vault.EncodeLine(s.ad(uint64(i)), b)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".rewrite.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readJSONL parses a newline-delimited record file, oldest-first, decrypting
// each sealed line under the reconstructed AD (legacy plaintext passes through
// until migrated). A line that fails to decrypt or unmarshal (including a
// wrong-key, tampered, or crash-truncated final line) is skipped; the position
// counter still advances so surviving records stay position-bound.
func (s *Store) readJSONL(path string) []Record {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var seq uint64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		plain, derr := s.vault.DecodeLine(s.ad(seq), append([]byte(nil), line...))
		seq++
		if derr != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(plain, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// Dir resolves Neo's Automatrix completion-inbox directory. An explicit
// override wins; else it is derived from the cortex root's parent (matching the
// conversation / task / trace stores, so the inbox shares /data with them and
// survives suspend / redeploy). Returns "" when neither is available
// (persistence disabled).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "automatrix")
	}
	return ""
}
