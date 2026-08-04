// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

// Package briefhistory is the durable recommendation-history sidecar for the
// ORACLE morning brief (task 5.5, req 17): delivered recommendation identifiers
// and news source URLs, plus the user's explicit feedback ("liked" /
// "not_interested" / "less_of_this"). The composer consults it so a brief never
// repeats a recommendation or news source without genuinely new context, and
// feedback flows into the profile ONLY as explicit user-attributed preference
// entries — never silent inference.
//
// It is a pure SIDECAR mirroring automatrixlog: an append-only JSONL store on
// the machine volume, crash-atomic (O_APPEND single-line writes, unparseable-
// line skip), vault-sealed per line when encrypting, carrying no secrets.
package briefhistory

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

// recordFile is the single per-daemon history file (per-user daemon, so the
// directory scoping is the ownership boundary).
const recordFile = "brief.history.jsonl"

// Store type and schema bound into each record's associated data.
const (
	storeHistory   = "neo.brief.history"
	schemaHistory1 = "record.v1"
)

// Record kinds.
const (
	KindDelivery = "delivery" // one delivered brief: its rec ids + news URLs
	KindFeedback = "feedback" // one explicit user reaction to a delivered item
)

// Feedback verdicts (req 17.2) — the closed explicit-reaction vocabulary.
const (
	VerdictLiked         = "liked"
	VerdictNotInterested = "not_interested"
	VerdictLessOfThis    = "less_of_this"
)

// ValidVerdict reports whether v is one of the closed feedback verdicts.
func ValidVerdict(v string) bool {
	switch v {
	case VerdictLiked, VerdictNotInterested, VerdictLessOfThis:
		return true
	}
	return false
}

// Record is one durable history entry — a delivery or a feedback event.
type Record struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Date           string   `json:"date,omitempty"`            // delivery: the local date delivered (YYYY-MM-DD)
	ConversationID string   `json:"conversation_id,omitempty"` // delivery: destination thread
	RecIDs         []string `json:"rec_ids,omitempty"`         // delivery: recommendation identifiers
	NewsURLs       []string `json:"news_urls,omitempty"`       // delivery: news source URLs
	Item           string   `json:"item,omitempty"`            // feedback: the rec id / URL / title reacted to
	Verdict        string   `json:"verdict,omitempty"`         // feedback: liked|not_interested|less_of_this
	CreatedAt      string   `json:"created_at"`
}

// Store is the durable brief history. All disk access is serialised by mu;
// every write is crash-atomic.
type Store struct {
	dir string
	mu  sync.Mutex

	vault *vault.Session
	user  string
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// SetVault wires the fail-closed data-at-rest session and owning user DID.
// Called once at engine assembly before any write; nil = legacy plaintext.
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = sess
	s.user = user
	s.mu.Unlock()
}

func (s *Store) ad(seq uint64) vault.AD {
	return vault.AD{User: s.user, Store: storeHistory, Seq: seq, Schema: schemaHistory1}
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// AppendDelivery records one delivered brief's recommendation ids + news URLs.
func (s *Store) AppendDelivery(localDate, convID string, recIDs, newsURLs []string) error {
	return s.append(Record{
		Kind:           KindDelivery,
		Date:           strings.TrimSpace(localDate),
		ConversationID: strings.TrimSpace(convID),
		RecIDs:         cleanList(recIDs),
		NewsURLs:       cleanList(newsURLs),
	})
}

// AppendFeedback records one explicit user reaction (req 17.2). Invalid
// verdicts are rejected.
func (s *Store) AppendFeedback(item, verdict string) error {
	item = strings.TrimSpace(item)
	if item == "" {
		return fmt.Errorf("briefhistory: feedback needs the item reacted to")
	}
	if !ValidVerdict(verdict) {
		return fmt.Errorf("briefhistory: verdict must be one of liked|not_interested|less_of_this (got %q)", verdict)
	}
	return s.append(Record{Kind: KindFeedback, Item: item, Verdict: verdict})
}

func (s *Store) append(rec Record) error {
	if rec.ID == "" {
		rec.ID = newID()
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("briefhistory: mkdir %s: %w", s.dir, err)
	}
	if err := s.appendJSONL(s.path(), rec); err != nil {
		return fmt.Errorf("briefhistory: append %s: %w", s.path(), err)
	}
	return nil
}

// Recent returns the newest n records (most recent first); n <= 0 returns all.
func (s *Store) Recent(n int) []Record {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.readJSONL(s.path())
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	if n > 0 && len(recs) > n {
		recs = recs[:n]
	}
	return recs
}

// RecentDelivered returns the union of recommendation ids and news URLs across
// the newest maxDeliveries delivery records — the composer's no-repeat set
// (req 17.1). maxDeliveries <= 0 scans all.
func (s *Store) RecentDelivered(maxDeliveries int) (recIDs, newsURLs []string) {
	seenRec := map[string]struct{}{}
	seenURL := map[string]struct{}{}
	deliveries := 0
	for _, r := range s.Recent(0) {
		if r.Kind != KindDelivery {
			continue
		}
		deliveries++
		for _, id := range r.RecIDs {
			if _, dup := seenRec[id]; !dup {
				seenRec[id] = struct{}{}
				recIDs = append(recIDs, id)
			}
		}
		for _, u := range r.NewsURLs {
			if _, dup := seenURL[u]; !dup {
				seenURL[u] = struct{}{}
				newsURLs = append(newsURLs, u)
			}
		}
		if maxDeliveries > 0 && deliveries >= maxDeliveries {
			break
		}
	}
	return recIDs, newsURLs
}

func (s *Store) path() string {
	if !s.Enabled() {
		return ""
	}
	return filepath.Join(s.dir, recordFile)
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("bh_%d", time.Now().UnixNano())
	}
	return "bh_" + hex.EncodeToString(b[:])
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// appendJSONL appends one record as a single (sealed, when encrypting) JSON
// line — O_APPEND + one Write is atomically visible; a torn final line is
// skipped on read. The record binds to its on-disk position via the AD.
func (s *Store) appendJSONL(path string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line, err := s.vault.EncodeLine(s.ad(countLines(path)), b)
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

// readJSONL parses the history oldest-first, decrypting sealed lines under the
// reconstructed AD (legacy plaintext passes through until migrated); a line
// that fails to decrypt or unmarshal is skipped, position counter advancing.
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

// Dir resolves the brief-history directory: an explicit override wins; else it
// is derived from the Neocortex root's parent, sharing the briefsettings "brief"
// dir on /data so it survives suspend / redeploy. "" disables persistence.
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "brief")
	}
	return ""
}
