// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package replay implements the cortex replay invariant: drop the derived
// `indexes/` namespaces and rebuild them deterministically from the canonical
// `store/` state.
//
// Spec ground truth — research/04-cortex.md §13.4:
//
//	Drop(indexes/<actor>)
//	For each entry in j/<actor>/<seq> in order:
//	    apply(entry) → mutates indexes only
//	Verify(state_roots equal) against latest snap/<seq>
//
// The literal verify target is "latest snap/<seq>" (singular). Phase 11 ships
// two complementary test surfaces both grounded in that text:
//
//  1. PreservesOverallRoot: capture c.OverallRoot() before drop, run Rebuild,
//     assert post-rebuild root byte-equal. Strongest invariant proof —
//     doesn't even depend on a snapshot existing. THE replay invariant test.
//
//  2. VerifyAgainstSnapshot(manifest): the §13.4 literal path. Caller takes a
//     snapshot, runs Rebuild, then calls VerifyAgainstSnapshot(latest). Both
//     succeed when latest.JournalSeq == journal head at drop time.
//
// Architecture follows research/04-cortex.md §1: store/ is canonical, indexes/
// is derived. The mapping from spec namespaces to our actual Pebble key
// prefixes (single-DB-per-actor; namespaces are key prefixes) is:
//
//	store/      KEEP  m/  mv/  e/  j/  tomb/  snap/  chk/
//	            KEEP  meta/journal_head  meta/snapshot_seq
//
//	indexes/    DROP  vec/  idx/  salience/  accum/
//	            DROP  meta/embed_cursor  meta/embed_vertex_next
//
// Rebuild then walks the kept canonical state to re-emit:
//
//	idx/type/  idx/tag/  idx/frame/  idx/actor_obj/  — predicate indexes
//	salience/<id>                                    — cold-score cache
//	accum/mmr/...                                    — journal MMR
//	idx/smt/memories/...  idx/smt/edges/...          — SMT roots
//
// vec/* (HNSW vector index) is intentionally NOT rebuilt by this package —
// re-embedding lives behind the Embedder boundary that the cortex layer owns
// (StartEmbedder + DrainEmbedder). The Phase 11 OverallRoot invariant does
// not depend on vec/* contents because Head.EmbeddingRef bytes are preserved
// in m/<id> across the drop (m/ is canonical, never touched by Rebuild) and
// the SMT staging uses those bytes verbatim. Documented in RebuildResult.
//
// Determinism: Rebuild is a pure function of the kept canonical state plus
// the supplied clock. salience.Cached values depend on `now` (recency decay)
// but are NOT inputs to OverallRoot, so different Rebuild clocks produce
// different salience caches and identical OverallRoots — exactly what the
// invariant requires.
package replay

import (
	"errors"
	"fmt"
	"time"

	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

// Options configures a Rebuild call. All fields optional; the zero value is
// a valid "rebuild against the underlying store with the wallclock" config.
type Options struct {
	// Now is the clock used for salience recomputation. salience.Cached is
	// recency-decay weighted (research/04-cortex.md §8) so the value
	// depends on the chosen clock. Zero value falls back to time.Now().UTC().
	//
	// salience values are NOT inputs to OverallRoot — different clocks
	// produce different caches and identical roots, by design.
	Now func() time.Time

	// Logf, if non-nil, receives one-line progress messages keyed by phase
	// ("drop", "rebuild_memories", "rebuild_edges", "rebuild_journal").
	// Defaults to a no-op so library users don't get stderr spam.
	Logf func(format string, args ...any)
}

// Result summarizes a completed Rebuild.
type Result struct {
	// JournalSeq is the journal head observed at rebuild time. After
	// Rebuild returns, accum/mmr/leafcount == JournalSeq.
	JournalSeq uint64

	// MemoriesScanned is the count of m/<id> heads re-emitted into idx/
	// and the memories SMT.
	MemoriesScanned uint64

	// EdgesScanned is the count of e/from/<src>/<t>/<dst> records
	// re-emitted into the edges SMT.
	EdgesScanned uint64

	// JournalLeavesAppended is the count of j/<seq> entries staged onto
	// the rebuilt MMR. Equals JournalSeq under the gap-free journal
	// invariant.
	JournalLeavesAppended uint64

	// PreOverallRoot is the OverallRoot observed BEFORE the drop. Set
	// only when Options.PreservesOverallRootCheck was true (or the
	// Cortex-level Rebuild method passed it). Used by VerifyPreservesRoot
	// downstream.
	PreOverallRoot [32]byte

	// PostOverallRoot is the OverallRoot observed AFTER the rebuild.
	// Always populated.
	PostOverallRoot [32]byte

	// SalienceBumpsApplied is the count of (j/<seq>, memoryID) pairs
	// the Phase 11.5 journal walk re-applied to salience.AccessCount/
	// Citations. Equal to sum(len(payload.AccessedIDs) for KindFind) +
	// sum(len(payload.CitedIDs) for KindAttest) over the journal,
	// minus dedupe of tombstoned-at-replay-time memories. Reported for
	// ops visibility and so tests can assert the walk ran at all.
	SalienceBumpsApplied uint64
}

// Errors returned by Rebuild / Verify.
var (
	ErrNilStore        = errors.New("replay: nil store")
	ErrNilSnapshot     = errors.New("replay: nil snapshot state")
	ErrRootMismatch    = errors.New("replay: post-rebuild root mismatch")
	ErrNoSnapshot      = errors.New("replay: no snapshot found")
	ErrSnapshotNoMatch = errors.New("replay: post-rebuild root does not match snapshot")
)

// Rebuild executes the full drop + rebuild cycle. The supplied snapshot.State
// MUST be the same instance the cortex uses (so its in-process MMR/SMT
// handles see the rebuilt accum/+idx/smt/* keys).
//
// Captures pre-drop OverallRoot into Result.PreOverallRoot so callers can
// verify the invariant in one call:
//
//	r, err := replay.Rebuild(s, snap, opts)
//	if r.PreOverallRoot != r.PostOverallRoot { ... }
//
// Side effects on the store:
//
//	DROP   vec/*  idx/*  salience/*  accum/*
//	       meta/embed_cursor  meta/embed_vertex_next
//	WRITE  idx/type/*  idx/tag/*  idx/frame/*  idx/actor_obj/*
//	       salience/*
//	       accum/mmr/leafcount  accum/mmr/n/*
//	       idx/smt/memories/n/*  idx/smt/edges/n/*
//
// All other namespaces (m/, mv/, e/, j/, tomb/, snap/, chk/, meta/journal_head,
// meta/snapshot_seq) are read but never mutated.
func Rebuild(s *store.Store, snap *snapshot.State, opts Options) (*Result, error) {
	if s == nil {
		return nil, ErrNilStore
	}
	if snap == nil {
		return nil, ErrNilSnapshot
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	// Capture pre-drop OverallRoot. This requires the existing accum/+
	// idx/smt/* state to be intact (it is — drop hasn't run yet).
	_, _, preRoot, err := snap.CurrentRoots()
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: capture pre-root: %w", err)
	}

	res := &Result{PreOverallRoot: preRoot}

	opts.Logf("replay: drop derived prefixes")
	if err := DropDerived(s); err != nil {
		return nil, fmt.Errorf("replay.Rebuild: drop: %w", err)
	}

	opts.Logf("replay: rebuild memories indexes + SMT")
	memCount, err := rebuildMemoriesIndexes(s, snap, opts.Now)
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: memories: %w", err)
	}
	res.MemoriesScanned = memCount

	opts.Logf("replay: rebuild edges SMT")
	edgeCount, err := rebuildEdgesSMT(s, snap)
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: edges: %w", err)
	}
	res.EdgesScanned = edgeCount

	// Phase 11.5: replay KindFind (LateBinding) + KindAttest entries to
	// re-feed salience.AccessCount + Citations from the journal. Order
	// matters: rebuildMemoriesIndexes seeded salience with AC=0,C=0; the
	// journal walk below brings them up to the post-bump values. The
	// MMR rebuild that follows doesn't read salience so this step can
	// sit between memories and journal MMR safely.
	opts.Logf("replay: rebuild salience bumps from journal")
	bumps, err := rebuildSalienceFromJournal(s, opts.Now())
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: salience bumps: %w", err)
	}
	res.SalienceBumpsApplied = bumps

	opts.Logf("replay: rebuild journal MMR")
	leafCount, err := rebuildJournalMMR(s, snap)
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: journal: %w", err)
	}
	res.JournalLeavesAppended = leafCount
	res.JournalSeq = leafCount

	// Capture post-rebuild OverallRoot.
	_, _, postRoot, err := snap.CurrentRoots()
	if err != nil {
		return nil, fmt.Errorf("replay.Rebuild: capture post-root: %w", err)
	}
	res.PostOverallRoot = postRoot

	return res, nil
}

// VerifyPreservesRoot returns nil iff r.PreOverallRoot == r.PostOverallRoot.
// Wraps ErrRootMismatch with the two roots on mismatch so callers can
// surface them in error messages.
//
// This is the §13.4 invariant test in its strongest form: "indexes are pure
// projection of canonical state", proved by drop+rebuild round-trip.
func VerifyPreservesRoot(r *Result) error {
	if r == nil {
		return errors.New("replay.VerifyPreservesRoot: nil result")
	}
	if r.PreOverallRoot != r.PostOverallRoot {
		return fmt.Errorf("%w: pre=%x post=%x",
			ErrRootMismatch, r.PreOverallRoot, r.PostOverallRoot)
	}
	return nil
}

// VerifyAgainstSnapshot returns nil iff r.PostOverallRoot equals the
// OverallRoot persisted in m. This implements the spec §13.4 verification
// path verbatim ("Verify(state_roots equal) against latest snap/<seq>").
//
// Caller responsibility: pass the LATEST snapshot for the actor (i.e. the
// snapshot taken at the journal head before the drop). Snapshots taken at
// older journal seqs will not match — that's not a bug, that's the
// snapshot being stale relative to current state.
func VerifyAgainstSnapshot(r *Result, m *snapshot.Manifest) error {
	if r == nil {
		return errors.New("replay.VerifyAgainstSnapshot: nil result")
	}
	if m == nil {
		return errors.New("replay.VerifyAgainstSnapshot: nil manifest")
	}
	if r.PostOverallRoot != m.OverallRoot {
		return fmt.Errorf("%w: post=%x snap=%x (snap_seq=%d journal_seq=%d)",
			ErrSnapshotNoMatch, r.PostOverallRoot, m.OverallRoot,
			m.SeqAtSnapshot, m.JournalSeq)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
