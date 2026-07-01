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
)

// recordFile is the single per-daemon inbox file. The daemon is per-user, so
// one file holds that user/agent's completion inbox; the records carry no user
// id (the directory scoping is the ownership boundary), and no secrets.
const recordFile = "automatrix.complete.jsonl"

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
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
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
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return rec, fmt.Errorf("automatrixlog: mkdir %s: %w", s.dir, err)
	}
	if err := appendJSONL(s.path(), rec); err != nil {
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
	recs := readJSONL(s.path())
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
	for _, r := range readJSONL(s.path()) {
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
	recs := readJSONL(s.path())
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
	if err := rewriteJSONL(s.path(), recs); err != nil {
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

// appendJSONL appends one record as a single JSON line. O_APPEND + a single
// Write makes the line atomically visible (POSIX); a torn write yields an
// unparseable final line that readJSONL skips (crash-atomic).
func appendJSONL(path string, rec Record) error {
	b, err := json.Marshal(rec)
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

// rewriteJSONL atomically replaces the file with recs (tmp+rename), used by
// MarkRead. A crash leaves either the old or the new file intact, never a torn
// one.
func rewriteJSONL(path string, recs []Record) error {
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := path + ".rewrite.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readJSONL parses a newline-delimited JSON record file, oldest-first. A line
// that fails to unmarshal (including a crash-truncated final line) is skipped —
// crash-atomic.
func readJSONL(path string) []Record {
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
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
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
