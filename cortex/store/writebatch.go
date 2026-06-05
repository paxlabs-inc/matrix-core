// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package store

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
)

// WriteBatch is a logical write transaction that pre-allocates one or more
// journal seqs and exposes the underlying Pebble batch so callers (e.g.
// cortex.Write) can compose journal entry + memory head + memory version
// + indexes in one atomic commit, satisfying §11.1 batch atomicity.
//
// Most callers append exactly one journal entry per batch. cortex.Attest
// is the lone Phase-12 exception: it appends KindAttest at seq=N then
// KindLearnWeights at seq=N+1 in the SAME batch so the (citation bumps,
// EMA weight update) pair is atomic and replay-deterministic. The store
// allocates a contiguous range of seqs for the batch and advances
// store.nextSeq by the number of appends on Commit.
//
// Concurrency: BeginWrite acquires the per-store seqMu and holds it until
// Commit or Abort. Only one WriteBatch may be open per Store at a time.
// Callers MUST always call Commit or Abort to release the lock; defer Abort
// is the standard pattern.
type WriteBatch struct {
	s  *Store
	pb *pebble.Batch

	startSeq    uint64 // first seq this batch will allocate
	nextSeq     uint64 // next-to-allocate within this batch
	lastSeq     uint64 // seq of the most recently appended entry (0 before first append)
	appendCount int    // number of AppendJournal calls on this batch

	leafHash [32]byte // set by AppendJournal when a hook is installed
	closed   bool     // true after Commit or Abort
}

// ErrBatchAlreadyClosed is returned when Commit/Abort is called twice or the
// batch is used after a previous Commit/Abort.
var ErrBatchAlreadyClosed = errors.New("store: write batch already closed")

// ErrBatchNoJournal is returned by Commit when the caller never wrote a
// journal entry. Every store mutation MUST be journaled (replay invariant).
var ErrBatchNoJournal = errors.New("store: write batch missing AppendJournal call")

// BeginWrite starts a new write batch, pre-allocating store.nextSeq as the
// first journal seq for this batch. The caller MUST eventually call Commit
// or Abort. Multiple AppendJournal calls consume consecutive seqs starting
// from startSeq.
//
// The backing Pebble batch is an indexed batch (NewIndexedBatch) so derived
// state stagers (Phase 7 MMR cascade, future SMT multi-update) can read
// their own pending writes within a single batch. Cost: small in-memory
// index overhead per batch; correctness benefit: multi-AppendJournal
// batches (Phase 12 cortex.Attest emits both KindAttest and
// KindLearnWeights atomically) produce the same OverallRoot live and on
// replay-rebuild.
func (s *Store) BeginWrite() *WriteBatch {
	s.seqMu.Lock()
	wb := &WriteBatch{
		s:        s,
		pb:       s.db.NewIndexedBatch(),
		startSeq: s.nextSeq,
		nextSeq:  s.nextSeq,
	}
	return wb
}

// Seq returns the seq of the most recently appended journal entry, or the
// next-to-allocate seq if no AppendJournal has been called yet. Stable
// after the most recent AppendJournal on this batch; callers that need
// the matching seq for an entry should call Seq() IMMEDIATELY after the
// AppendJournal that wrote that entry.
func (wb *WriteBatch) Seq() uint64 { return wb.seqGetter() }

// Set adds a key/value to the batch. Mirrors pebble.Batch.Set.
func (wb *WriteBatch) Set(key, value []byte) error {
	if wb.closed {
		return ErrBatchAlreadyClosed
	}
	return wb.pb.Set(key, value, nil)
}

// Delete adds a key deletion to the batch. Reserved for tombstone-related
// flows; ordinary memory writes never delete primary records.
func (wb *WriteBatch) Delete(key []byte) error {
	if wb.closed {
		return ErrBatchAlreadyClosed
	}
	return wb.pb.Delete(key, nil)
}

// AppendJournal canonical-encodes e (filling Seq from the batch), writes it
// to j/<seq>, and writes meta/journal_head=seq+1. May be called more than
// once per batch — each call consumes the next seq in the batch's pre-
// allocated range, and meta/journal_head is rewritten to the last-
// appended-seq+1 (Pebble overwrite semantics: the final value wins on
// commit). Must be called at least once before Commit (the replay
// invariant: every store mutation MUST be journaled).
//
// Multiple-append usage: cortex.Attest (KindAttest at seq=N then
// KindLearnWeights at seq=N+1) is the canonical case. Other callers
// append exactly once.
func (wb *WriteBatch) AppendJournal(e *journal.Entry) error {
	if wb.closed {
		return ErrBatchAlreadyClosed
	}
	if e == nil {
		return errors.New("store: nil journal entry")
	}
	if e.Kind == "" {
		return errors.New("store: journal entry kind required")
	}
	seq := wb.nextSeq
	e.Seq = seq
	enc, err := e.Encode()
	if err != nil {
		return fmt.Errorf("store: encode journal entry: %w", err)
	}
	if err := wb.pb.Set(keys.JournalKey(seq), enc, nil); err != nil {
		return fmt.Errorf("store: set j/%d: %w", seq, err)
	}
	var headBuf [8]byte
	binary.BigEndian.PutUint64(headBuf[:], seq+1)
	if err := wb.pb.Set(keys.MetaJournalHead, headBuf[:], nil); err != nil {
		return fmt.Errorf("store: set meta/journal_head: %w", err)
	}
	// Invoke the journal hook (Phase 7 MMR append, etc.) inside the same
	// atomic batch so accumulator state moves in lock-step with j/<seq>.
	// The hook is invoked per appended entry — multiple entries in one
	// batch produce multiple MMR-leaf updates, all staged in the same
	// Pebble batch and committed atomically.
	if wb.s.journalHook != nil {
		wb.leafHash = journal.LeafHash(enc)
		if err := wb.s.journalHook(wb.pb, seq, wb.leafHash); err != nil {
			return fmt.Errorf("store: journal hook: %w", err)
		}
	}
	wb.lastSeq = seq
	wb.nextSeq = seq + 1
	wb.appendCount++
	return nil
}

// seqGetter returns wb.lastSeq if there has been at least one AppendJournal
// call, otherwise startSeq (the to-be-allocated seq).
func (wb *WriteBatch) seqGetter() uint64 {
	if wb.appendCount == 0 {
		return wb.startSeq
	}
	return wb.lastSeq
}

// LeafHash returns the journal leaf hash computed by the most recent
// AppendJournal call on this batch. Returns the zero hash before
// AppendJournal is called or when no hook recorded a hash. Useful for
// callers that want to log the leaf hash for audit.
func (wb *WriteBatch) LeafHash() [32]byte { return wb.leafHash }

// Commit flushes the batch to disk with Pebble's Sync option. On success the
// store's nextSeq advances to lastSeq+1 (i.e. by the number of journal
// entries appended); on failure the batch is closed and seqMu is released,
// but nextSeq does not advance (so the same seq range can be reused).
func (wb *WriteBatch) Commit() error {
	if wb.closed {
		return ErrBatchAlreadyClosed
	}
	defer func() {
		wb.closed = true
		_ = wb.pb.Close()
		wb.s.seqMu.Unlock()
	}()
	if wb.appendCount == 0 {
		return ErrBatchNoJournal
	}
	if err := wb.pb.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	wb.s.nextSeq = wb.lastSeq + 1
	return nil
}

// Abort releases the batch without committing. Safe to call after Commit
// (becomes a no-op). Always safe in a `defer wb.Abort()` line.
func (wb *WriteBatch) Abort() {
	if wb.closed {
		return
	}
	wb.closed = true
	_ = wb.pb.Close()
	wb.s.seqMu.Unlock()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
