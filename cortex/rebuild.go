// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 11 — cortex-level Rebuild surface.
//
// Spec: research/04-cortex.md §11.5 lists Rebuild as a first-class API
// ("Rebuild(actor AgentRef) error // drop+rebuild indexes from store/").
// This file is the thin facade that resolves a Cortex handle to the
// underlying store + snapshot.State and delegates to cortex/replay.
//
// Phase 11 invariant (research/04-cortex.md §13.4):
//
//	Drop(indexes/<actor>)
//	For each entry in j/<actor>/<seq> in order:
//	    apply(entry) → mutates indexes only
//	Verify(state_roots equal) against latest snap/<seq>
//
// Cortex.Rebuild captures the pre-drop OverallRoot and the post-rebuild
// OverallRoot in the returned RebuildResult; callers verify with
// replay.VerifyPreservesRoot or replay.VerifyAgainstSnapshot.
//
// Embedder discipline: callers MUST stop the async embedder before
// calling Rebuild. A running embedder could journal new KindEmbed
// entries mid-rebuild, racing the SMT/MMR re-derivation. Rebuild
// returns ErrEmbedderRunning instead of silently succeeding.

package cortex

import (
	"errors"

	"matrix/cortex/replay"
)

// ErrEmbedderRunning is returned by Rebuild when c.embed is non-nil.
// Stop the embedder via StopEmbedder() first; restart after Rebuild
// returns.
//
// Note: re-running StartEmbedder after Rebuild walks j/ from cursor=0
// and re-processes every KindWrite entry. The current embedder does
// not skip already-embedded memories (Head.EmbeddingRef populated),
// so it WILL journal new KindEmbed entries — advancing OverallRoot.
// That's a documented limitation; restoring HNSW from vec/meta alone
// (loadOrBuildIndex's fallback path, embedder.go) is the lossless
// alternative when only the HNSW file was lost.
var ErrEmbedderRunning = errors.New("cortex.Rebuild: stop embedder first via StopEmbedder()")

// RebuildResult mirrors replay.Result so callers don't have to import
// the replay package for the common case. Type aliasing keeps the
// fields and methods identical.
type RebuildResult = replay.Result

// RebuildOptions mirrors replay.Options for the same reason.
type RebuildOptions = replay.Options

// Rebuild drops every key under spec's indexes/ namespace (vec/+idx/+
// salience/+accum/+meta/embed_*) and rebuilds them deterministically
// from the kept canonical state (m/+mv/+e/+j/+snap/+chk/+meta/journal_*).
//
// Returns *RebuildResult carrying pre-drop and post-rebuild OverallRoot.
// Use replay.VerifyPreservesRoot(r) for the strongest invariant test
// (PreservesOverallRoot pattern) or replay.VerifyAgainstSnapshot(r, m)
// for the spec §13.4 literal pattern.
//
// Pre-conditions:
//   - StopEmbedder() must have been called (or no embedder ever started).
//   - No in-flight Cortex.Write/Update/UpdateHead/Tombstone/AddEdge/
//     RemoveEdge/Compact (caller's responsibility — Rebuild is not
//     concurrent-safe with mutating operations).
//
// Post-conditions:
//   - c.OverallRoot() == pre-drop OverallRoot (if no other mutations).
//   - vec/* is empty; HNSW index file (if any) must be rebuilt by the
//     next StartEmbedder call (its loadOrBuildIndex fallback handles
//     missing index files by scanning vec/meta — but vec/meta is also
//     empty post-Rebuild, so the embedder will re-process the journal).
//
// Atomicity: Rebuild is NOT atomic across drop+rebuild as a unit. The
// drop commits, then each rebuild step commits its own batch (per-memory,
// per-edge, per-journal-leaf). If Rebuild crashes mid-cycle the next
// Rebuild call resumes correctly — drop is idempotent, rebuild is
// idempotent. Between crash and re-run, c.OverallRoot() returns a root
// over partially-rebuilt state, which compares unequal to any prior
// snapshot. That's the correct "we know derived state is dirty" signal.
func (c *Cortex) Rebuild(opts RebuildOptions) (*RebuildResult, error) {
	if c.embed != nil {
		return nil, ErrEmbedderRunning
	}
	if opts.Now == nil {
		opts.Now = c.now
	}
	return replay.Rebuild(c.s, c.snap, opts)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
