// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package store opens and manages the per-actor Pebble database that backs
// the Matrix cortex.
//
// Spec: research/04-cortex.md §1 (per-actor isolation), §2 (key encoding),
// §11.1 (write batch atomicity).
//
// Design decision (not in spec, flagged in commit notes): although §1 sketches
// separate runtime subdirectories per namespace ("memories/<actor>/",
// "edges/<actor>/", ...), §11.1's atomic-batch requirement spanning journal +
// head + version + indexes makes ONE Pebble DB per actor mandatory. All
// namespaces from §2.2 are KEY PREFIXES inside that single DB.
//
// Layout on disk:
//
//	<root>/<actor>/store/        # the Pebble DB
//	<root>/<actor>/indexes/      # future: usearch vector index file, etc.
//
// Phase 1 scope: open, close, allocate journal seq, append journal entry,
// iterate journal. Memory/Edge/Snapshot APIs land in Phase 2+.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
)

// JournalHook is invoked from inside AppendJournal (both the standalone
// Store.AppendJournal and the batched WriteBatch.AppendJournal) AFTER the
// j/<seq> key has been staged but BEFORE the batch commits. The hook
// receives the Pebble batch so it can stage additional keys atomically
// with the journal write — the canonical use case is the snapshot layer
// appending an MMR leaf for every journal entry.
//
// Returning a non-nil error aborts the batch. The hook MUST NOT call
// AppendJournal recursively; doing so would deadlock the seq mutex.
type JournalHook func(b *pebble.Batch, seq uint64, leafHash [32]byte) error

// Store is a handle to one actor's Pebble database.
//
// Stores are safe for concurrent use. Journal-seq allocation is serialized by
// seqMu to keep the per-actor monotonic invariant; primary writes through
// the Pebble API are already concurrent-safe.
type Store struct {
	actor string
	root  string
	db    *pebble.DB

	seqMu   sync.Mutex
	nextSeq uint64 // next seq to allocate; loaded from MetaJournalHead on Open

	// journalHook, if non-nil, is invoked from AppendJournal /
	// WriteBatch.AppendJournal with the staged batch + leaf hash. Used
	// by the snapshot layer (Phase 7) to keep the journal MMR in lock-
	// step with j/<seq> writes. nil before SetJournalHook is called.
	journalHook JournalHook
}

// SetJournalHook installs (or replaces) the journal hook. Pass nil to
// uninstall. Safe to call before any AppendJournal; not safe to call
// concurrently with one (callers must coordinate).
func (s *Store) SetJournalHook(fn JournalHook) {
	s.journalHook = fn
}

// Options control Store.Open. Zero values are valid.
type Options struct {
	// Pebble lets callers override the underlying Pebble options if needed for
	// tuning (block size, bloom bits, cache). Nil means library defaults.
	Pebble *pebble.Options
}

// Open opens the actor's Pebble DB rooted under root. The directory layout is
// created on first open. The actor string is used as a directory name; it
// must not contain '/'.
func Open(root, actor string, opts *Options) (*Store, error) {
	if err := keys.ValidateNoSeparator(actor); err != nil {
		return nil, fmt.Errorf("store: actor %q: %w", actor, err)
	}
	if root == "" {
		return nil, errors.New("store: root path required")
	}
	dbDir := filepath.Join(root, actor, "store")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dbDir, err)
	}

	var pebOpts *pebble.Options
	if opts != nil && opts.Pebble != nil {
		pebOpts = opts.Pebble
	} else {
		pebOpts = &pebble.Options{}
	}

	db, err := pebble.Open(dbDir, pebOpts)
	if err != nil {
		return nil, fmt.Errorf("store: pebble.Open %s: %w", dbDir, err)
	}

	s := &Store{actor: actor, root: root, db: db}
	if err := s.loadHead(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: load journal head: %w", err)
	}
	return s, nil
}

// Close releases the underlying Pebble DB.
func (s *Store) Close() error {
	return s.db.Close()
}

// Actor returns the actor name this store is bound to.
func (s *Store) Actor() string { return s.actor }

// Path returns the root directory containing the actor's data.
func (s *Store) Path() string { return filepath.Join(s.root, s.actor) }

// DB exposes the underlying Pebble handle. Reserved for sibling packages in
// later phases (memory writer, snapshot accumulator); avoid using from
// application code.
func (s *Store) DB() *pebble.DB { return s.db }

// loadHead reads meta/journal_head from the DB. If absent, initializes to 0.
func (s *Store) loadHead() error {
	v, closer, err := s.db.Get(keys.MetaJournalHead)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			s.nextSeq = 0
			return nil
		}
		return err
	}
	defer closer.Close()
	if len(v) != 8 {
		return fmt.Errorf("store: meta/journal_head has unexpected length %d", len(v))
	}
	s.nextSeq = binary.BigEndian.Uint64(v)
	return nil
}

// NextSeq returns the seq that the next AppendJournal call will allocate.
// Useful for tests and dump tools. Not stable across concurrent appends.
func (s *Store) NextSeq() uint64 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	return s.nextSeq
}

// AppendJournal canonical-encodes e (with Seq filled in by the store), writes
// it to j/<seq>, and bumps meta/journal_head — atomically in one Pebble batch.
//
// On success returns the allocated Seq. e.Seq is overwritten with that value
// so the caller's struct reflects the persisted state.
func (s *Store) AppendJournal(e *journal.Entry) (uint64, error) {
	if e == nil {
		return 0, errors.New("store: nil entry")
	}
	if e.Kind == "" {
		return 0, errors.New("store: entry.Kind required")
	}

	s.seqMu.Lock()
	defer s.seqMu.Unlock()

	seq := s.nextSeq
	e.Seq = seq

	enc, err := e.Encode()
	if err != nil {
		return 0, fmt.Errorf("store: encode entry: %w", err)
	}

	b := s.db.NewBatch()
	defer b.Close()

	if err := b.Set(keys.JournalKey(seq), enc, nil); err != nil {
		return 0, fmt.Errorf("store: batch.Set j/%d: %w", seq, err)
	}
	var headBuf [8]byte
	binary.BigEndian.PutUint64(headBuf[:], seq+1)
	if err := b.Set(keys.MetaJournalHead, headBuf[:], nil); err != nil {
		return 0, fmt.Errorf("store: batch.Set meta/journal_head: %w", err)
	}
	// Invoke the journal hook (if installed) with the leaf hash so the
	// snapshot MMR can stage its leaf in the same atomic batch.
	if s.journalHook != nil {
		leafHash := journal.LeafHash(enc)
		if err := s.journalHook(b, seq, leafHash); err != nil {
			return 0, fmt.Errorf("store: journal hook: %w", err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("store: batch.Commit: %w", err)
	}

	s.nextSeq = seq + 1
	return seq, nil
}

// IterJournal walks the journal in seq order (ascending), invoking fn for each
// entry. The decoded Entry is reused between iterations; callers must not
// retain it across calls. Returning an error from fn aborts iteration.
func (s *Store) IterJournal(fn func(*journal.Entry) error) error {
	lo, hi := keys.JournalRange()
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return fmt.Errorf("store: NewIter: %w", err)
	}
	defer it.Close()

	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return fmt.Errorf("store: iter value: %w", err)
		}
		var e journal.Entry
		if err := journal.Decode(v, &e); err != nil {
			return fmt.Errorf("store: decode journal entry: %w", err)
		}
		if err := fn(&e); err != nil {
			return err
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("store: iter: %w", err)
	}
	return nil
}

// Get reads the value at key. Returns (nil, false, nil) if not present. The
// returned slice is owned by the caller (cloned from Pebble's internal
// buffer).
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer closer.Close()
	out := make([]byte, len(v))
	copy(out, v)
	return out, true, nil
}

// PrefixIter invokes fn for every (key, value) pair whose key begins with
// prefix, in byte-ascending order. The slices passed to fn are valid only
// for the duration of the call; callers must copy if they need to retain.
// Returning an error from fn aborts iteration.
func (s *Store) PrefixIter(prefix []byte, fn func(key, value []byte) error) error {
	lo, hi := keys.PrefixRange(prefix)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return fmt.Errorf("store: NewIter: %w", err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return fmt.Errorf("store: iter value: %w", err)
		}
		if err := fn(it.Key(), v); err != nil {
			return err
		}
	}
	return it.Error()
}

// JournalCount returns the number of entries currently in the journal.
// Equivalent to NextSeq() under the gap-free invariant; provided as a
// distinct API so callers expressing "how many entries do I have" don't have
// to know about seq semantics.
func (s *Store) JournalCount() uint64 {
	return s.NextSeq()
}

// SetMeta writes value at the given meta/<key>. Restricted to keys with
// the meta/ prefix so callers can't bypass journaled writes by smuggling
// state into m/ or vec/ via this surface.
//
// Intended use: derived-state progress markers (Phase 5's embed_cursor,
// embed_vertex_next). Such markers are recomputable by walking the
// journal forward from seq=0, so they live OUTSIDE the journaled-batch
// discipline that protects canonical state (§11.1).
func (s *Store) SetMeta(key, value []byte) error {
	if !hasMetaPrefix(key) {
		return fmt.Errorf("store: SetMeta key %q must start with meta/", key)
	}
	return s.db.Set(key, value, pebble.Sync)
}

// DeleteMeta removes a meta/<key>. Mirrors SetMeta in scope (meta/ only)
// so callers cannot smuggle deletes against canonical m/ or e/ state via
// this surface. Idempotent: deleting an absent key returns nil.
//
// Intended use: sess#32 ambient scheduler tearing down meta/goal_state
// entries when a Goal transitions to GoalAbandoned (housekeeping; the
// stale row is harmless but keeps `cortex.ListGoalStates()` clean).
func (s *Store) DeleteMeta(key []byte) error {
	if !hasMetaPrefix(key) {
		return fmt.Errorf("store: DeleteMeta key %q must start with meta/", key)
	}
	return s.db.Delete(key, pebble.Sync)
}

// hasMetaPrefix mirrors keys.hasPrefix but is local to avoid an extra
// import cycle for one-line bytes comparison.
func hasMetaPrefix(k []byte) bool {
	const p = "meta/"
	if len(k) < len(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if k[i] != p[i] {
			return false
		}
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
