// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 11 integration tests — Cortex.Rebuild + replay invariant.
//
// Spec: research/04-cortex.md §13.4 ("drop indexes/, replay journal,
// verify state_roots equal against latest snap/<seq>").
//
// Test surfaces:
//
//   - TestRebuildPreservesOverallRoot — primary invariant test. Run
//     every cortex mutation kind, capture pre-drop OverallRoot, run
//     Rebuild, assert post-rebuild OverallRoot byte-equal.
//
//   - TestRebuildVerifyAgainstLatestSnap — spec §13.4 literal path.
//     Snapshot, Rebuild, VerifyAgainstSnapshot.
//
//   - Per-kind regression tests for every mutation surface that
//     touches memories or edges SMT (Write, Update, UpdateHead,
//     Tombstone, AddEdge, RemoveEdge, Compact, embedder-Embed).

package cortex

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/embed"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/replay"
	"matrix/cortex/salience"
	"matrix/cortex/store"
)

var _ = store.ErrBatchNoJournal // silence unused-store-import if any

// captureCanonical returns a map of every key/value under canonical
// prefixes for byte-equality assertions across Rebuild.
func captureCanonical(t *testing.T, s *store.Store) map[string][]byte {
	t.Helper()
	canonical := [][]byte{
		keys.PrefixMemoryHead,    // m/
		keys.PrefixMemoryVersion, // mv/
		keys.PrefixEdgeFrom,      // e/from/
		keys.PrefixEdgeTo,        // e/to/
		keys.PrefixJournal,       // j/
		keys.PrefixTombstone,     // tomb/
		keys.PrefixSnapshot,      // snap/
		keys.PrefixCheckpoint,    // chk/
	}
	out := make(map[string][]byte)
	for _, p := range canonical {
		err := s.PrefixIter(p, func(k, v []byte) error {
			kc := make([]byte, len(k))
			copy(kc, k)
			vc := make([]byte, len(v))
			copy(vc, v)
			out[string(kc)] = vc
			return nil
		})
		if err != nil {
			t.Fatalf("captureCanonical scan %q: %v", p, err)
		}
	}
	// meta/journal_head and meta/snapshot_seq are also canonical.
	for _, k := range [][]byte{keys.MetaJournalHead, []byte("meta/snapshot_seq")} {
		v, ok, err := s.Get(k)
		if err != nil {
			t.Fatalf("captureCanonical get %q: %v", k, err)
		}
		if ok {
			out[string(k)] = v
		}
	}
	return out
}

// assertCanonicalEqual asserts every key in a is present in b with
// byte-identical value, and vice versa.
func assertCanonicalEqual(t *testing.T, a, b map[string][]byte, label string) {
	t.Helper()
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			t.Fatalf("%s: canonical key %q present pre, absent post", label, k)
		}
		if !bytes.Equal(va, vb) {
			t.Fatalf("%s: canonical key %q value differs pre/post", label, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Fatalf("%s: canonical key %q absent pre, present post", label, k)
		}
	}
}

// TestRebuildPreservesOverallRoot is the primary §13.4 invariant test.
// Drives every mutation surface, captures pre-drop OverallRoot, runs
// Rebuild, asserts post-rebuild OverallRoot byte-equal.
func TestRebuildPreservesOverallRoot(t *testing.T) {
	c := openCortex(t)

	// Mix of memory writes spanning multiple types + Tags + Frames so
	// idx/type, idx/tag, idx/frame, idx/actor_obj all populate.
	prefURI1 := writePref(t, c, "tone", 5, "personal")
	prefURI2 := writePref(t, c, "verbosity", 3, "personal", "voice")
	idA := idOf(prefURI1)
	idB := idOf(prefURI2)

	// Identity (auto-pinned tier eligibility)
	idURI, err := c.Write(memory.Head{ActorScope: "andrew"}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write Identity: %v", err)
	}
	_ = idURI

	// Belief (Frames-tagged for idx/frame coverage)
	belURI, err := c.Write(memory.Head{
		ActorScope: "andrew",
		Frames: []memory.FrameRef{
			{Verb: memory.VerbBuild, ObjKind: memory.KindAgent, ObjRef: "did:pax:helper"},
		},
	}, memory.BeliefData{
		SchemaVersion: 1,
		Statement:     "the helper agent is reliable",
		Subject:       "did:pax:helper",
		Stance:        memory.StanceBelieve,
	}, WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write Belief: %v", err)
	}

	// Event (idx/actor_obj coverage — Event-only)
	evURI, err := c.Write(memory.Head{
		ActorScope: "andrew",
		Frames: []memory.FrameRef{
			{Verb: memory.VerbBuild, ObjKind: memory.KindAgent, ObjRef: "did:pax:helper"},
		},
	}, memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventIntentCompleted,
		OutcomeVal:    memory.OutcomeSuccess,
		Summary:       "helper completed task",
	}, WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write Event: %v", err)
	}
	_ = belURI
	_ = evURI

	// Update (Data-version bump)
	if _, err := c.Update(prefURI1, memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone",
		Polarity: memory.PolarityPrefer, StrengthVal: 0.95,
	}, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "pref:tone v2"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// UpdateHead (Head-only mutation; new in Phase 10)
	newTags := []memory.Tag{"important", "v3"}
	newImportance := uint8(8)
	if _, err := c.UpdateHead(prefURI2, HeadPatch{
		Tags:               &newTags,
		DeclaredImportance: &newImportance,
	}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	// Edges (forward + reverse SMT impact: only forward anchored)
	if err := c.AddEdge(idA, memory.EdgeReferences, idB, AddEdgeMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := c.AddEdge(idB, memory.EdgeContradicts, idA, AddEdgeMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("AddEdge contradicts: %v", err)
	}

	// Tombstone (changes Head.Tombstoned)
	tombURI := writePref(t, c, "tombed", 1)
	if err := c.Tombstone(tombURI, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	// RemoveEdge (Tombstoned EdgeRecord; rewrites e/from + e/to)
	if err := c.RemoveEdge(idB, memory.EdgeContradicts, idA, "rethink", "andrew"); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}

	// Capture pre-drop state
	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}
	preCanonical := captureCanonical(t, c.s)

	// Verify there's actually derived state to drop
	preDerived, err := replay.CountDerived(c.s)
	if err != nil {
		t.Fatalf("CountDerived pre: %v", err)
	}
	if preDerived == 0 {
		t.Fatalf("expected non-zero derived state before Rebuild; got 0")
	}

	// Run Rebuild
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// VerifyPreservesRoot
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}

	// Cross-check via OverallRoot()
	postRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot post: %v", err)
	}
	if postRoot != preRoot {
		t.Fatalf("OverallRoot drift: pre=%x post=%x", preRoot, postRoot)
	}
	if res.PostOverallRoot != preRoot {
		t.Fatalf("Result.PostOverallRoot drift: pre=%x result.post=%x", preRoot, res.PostOverallRoot)
	}

	// Canonical state must be byte-identical post-Rebuild.
	postCanonical := captureCanonical(t, c.s)
	assertCanonicalEqual(t, preCanonical, postCanonical, "TestRebuildPreservesOverallRoot")

	// Result counters sanity
	if res.MemoriesScanned == 0 {
		t.Fatalf("MemoriesScanned: 0")
	}
	if res.EdgesScanned == 0 {
		t.Fatalf("EdgesScanned: 0")
	}
	if res.JournalLeavesAppended == 0 {
		t.Fatalf("JournalLeavesAppended: 0")
	}
	if res.JournalSeq != c.s.NextSeq() {
		t.Fatalf("JournalSeq=%d but NextSeq=%d", res.JournalSeq, c.s.NextSeq())
	}
}

// TestRebuildVerifyAgainstLatestSnap implements spec §13.4 literally.
func TestRebuildVerifyAgainstLatestSnap(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "verbosity", 3)
	uA := writePref(t, c, "format", 2)
	uB := writePref(t, c, "tempo", 7)
	if err := c.AddEdge(idOf(uA), memory.EdgeReferences, idOf(uB), AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// Take snapshot AS THE LAST STEP — so snap.JournalSeq == journal head.
	manifest, err := c.Snapshot("explicit")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Rebuild
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if err := replay.VerifyAgainstSnapshot(res, manifest); err != nil {
		t.Fatalf("VerifyAgainstSnapshot: %v", err)
	}
}

// TestRebuildIdempotent — running Rebuild twice produces the same root.
func TestRebuildIdempotent(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5, "personal")
	writePref(t, c, "verbosity", 3)

	first, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	second, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if first.PostOverallRoot != second.PostOverallRoot {
		t.Fatalf("rebuild not idempotent: 1st=%x 2nd=%x",
			first.PostOverallRoot, second.PostOverallRoot)
	}
	if second.PreOverallRoot != first.PostOverallRoot {
		t.Fatalf("second pre-root should equal first post-root")
	}
}

// TestRebuildEmpty — fresh cortex, no writes, Rebuild is a no-op
// invariant-wise.
func TestRebuildEmpty(t *testing.T) {
	c := openCortex(t)

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot on empty: %v", err)
	}
	if res.MemoriesScanned != 0 || res.EdgesScanned != 0 || res.JournalLeavesAppended != 0 {
		t.Fatalf("counts on empty store: %+v", res)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("empty rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}
}

// TestRebuildDropsAllDerived — after DropDerived, the derived
// namespaces are empty.
func TestRebuildDropsAllDerived(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5, "personal")
	writePref(t, c, "verbosity", 3)

	if err := replay.DropDerived(c.s); err != nil {
		t.Fatalf("DropDerived: %v", err)
	}
	n, err := replay.CountDerived(c.s)
	if err != nil {
		t.Fatalf("CountDerived: %v", err)
	}
	if n != 0 {
		t.Fatalf("post-drop derived count = %d, want 0", n)
	}
}

// TestRebuildErrEmbedderRunning — Rebuild rejects when embedder is up.
func TestRebuildErrEmbedderRunning(t *testing.T) {
	c := openCortex(t)
	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}
	t.Cleanup(func() { _ = c.StopEmbedder() })

	_, err := c.Rebuild(RebuildOptions{})
	if !errors.Is(err, ErrEmbedderRunning) {
		t.Fatalf("expected ErrEmbedderRunning, got %v", err)
	}
}

// TestRebuildAfterEmbedder — embedder runs, populates vec/meta and
// EmbeddingRef on Heads. Stop embedder, Rebuild, root preserved
// (vec/meta gets dropped but Head.EmbeddingRef bytes are kept in m/
// so the memories SMT is byte-identical).
func TestRebuildAfterEmbedder(t *testing.T) {
	c := openCortex(t)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}

	writePref(t, c, "tone", 5)
	writePref(t, c, "verbosity", 3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DrainEmbedder(ctx); err != nil {
		t.Fatalf("DrainEmbedder: %v", err)
	}

	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("StopEmbedder: %v", err)
	}

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}

	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("post-embedder rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}

	// vec/meta should have been dropped by Rebuild.
	vecCount := uint64(0)
	if err := c.s.PrefixIter([]byte("vec/"), func(_, _ []byte) error {
		vecCount++
		return nil
	}); err != nil {
		t.Fatalf("scan vec/: %v", err)
	}
	if vecCount != 0 {
		t.Fatalf("vec/ count post-Rebuild: %d, want 0", vecCount)
	}
}

// TestRebuildSalienceCacheRecomputed — Rebuild with a different clock
// produces different salience.Cached values but the same OverallRoot.
// salience is NOT in OverallRoot per research/04 §7.2 + Phase 11 Q6.
func TestRebuildSalienceCacheRecomputed(t *testing.T) {
	c := openCortex(t)

	// Original write at clock T0.
	t0 := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { return t0 }
	uri := writePref(t, c, "tone", 5)
	id := idOf(uri)

	// Move clock forward dramatically — recency decay should drop.
	t1 := t0.Add(180 * 24 * time.Hour) // 180 days = 2 half-lives
	c.now = func() time.Time { return t1 }

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	res, err := c.Rebuild(RebuildOptions{Now: func() time.Time { return t1 }})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("salience-clock-shifted Rebuild changed OverallRoot: pre=%x post=%x", preRoot, postRoot)
	}

	// Salience cache exists post-Rebuild (we re-emit it).
	score, ok, err := salience.Read(c.s, id)
	if err != nil || !ok {
		t.Fatalf("salience.Read post-Rebuild: ok=%v err=%v", ok, err)
	}
	if score.Cached <= 0 {
		t.Fatalf("salience post-Rebuild Cached=%f, want >0", score.Cached)
	}
	// LastUsed reflects the rebuild clock t1 (NewForWrite uses now).
	if score.LastUsed != t1.UnixNano() {
		t.Fatalf("salience.LastUsed=%d want %d", score.LastUsed, t1.UnixNano())
	}
}

// TestRebuildPreservesRootAfterUpdateHead — UpdateHead writes new Head
// bytes and journals KindUpdateHead. Rebuild must produce same root.
func TestRebuildPreservesRootAfterUpdateHead(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5, "personal")

	newTags := []memory.Tag{"v2", "important"}
	newImp := uint8(9)
	if _, err := c.UpdateHead(uri, HeadPatch{
		Tags:               &newTags,
		DeclaredImportance: &newImp,
	}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	preRoot, _ := c.OverallRoot()
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("post-UpdateHead Rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}
}

// TestRebuildPreservesRootAfterTombstone — Tombstone changes Head bytes
// (Tombstoned field set); Rebuild handles it.
func TestRebuildPreservesRootAfterTombstone(t *testing.T) {
	c := openCortex(t)
	u1 := writePref(t, c, "tone", 5)
	u2 := writePref(t, c, "verbosity", 3)
	if err := c.Tombstone(u1, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	preRoot, _ := c.OverallRoot()
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("post-Tombstone Rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}
	_ = u2
	_ = res
}

// TestRebuildPreservesRootAfterCompact — Compact emits chk/<id> + a
// KindCompact journal entry. Rebuild must include the journal leaf.
func TestRebuildPreservesRootAfterCompact(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium body content", 5)
	mems := resolveAll(t, c, u)
	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "intent_X", StepID: "step_1",
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	preRoot, _ := c.OverallRoot()
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("post-Compact Rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}
}

// TestRebuildPreservesRootAfterEdgeMutations — AddEdge, RemoveEdge,
// AddEdge again on tombstoned (revive) all must round-trip.
func TestRebuildPreservesRootAfterEdgeMutations(t *testing.T) {
	c := openCortex(t)
	uA := writePref(t, c, "a", 5)
	uB := writePref(t, c, "b", 5)
	idA, idB := idOf(uA), idOf(uB)

	if err := c.AddEdge(idA, memory.EdgeReferences, idB, AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveEdge(idA, memory.EdgeReferences, idB, "wrong", "andrew"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idA, memory.EdgeReferences, idB, AddEdgeMeta{}); err != nil { // revive
		t.Fatal(err)
	}

	preRoot, _ := c.OverallRoot()
	if _, err := c.Rebuild(RebuildOptions{}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	postRoot, _ := c.OverallRoot()
	if postRoot != preRoot {
		t.Fatalf("post-edge-mutations Rebuild changed root: pre=%x post=%x", preRoot, postRoot)
	}
}

// TestRebuildVerifyAgainstStaleSnapshotMismatches — the §13.4 verifier
// correctly fails when snap is older than current journal head.
func TestRebuildVerifyAgainstStaleSnapshotMismatches(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)

	// Take snap, then write more (so snap is now stale).
	manifest, err := c.Snapshot("explicit")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writePref(t, c, "verbosity", 3)

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	// Post-rebuild root reflects current journal head, NOT the stale snap.
	err = replay.VerifyAgainstSnapshot(res, manifest)
	if !errors.Is(err, replay.ErrSnapshotNoMatch) {
		t.Fatalf("expected ErrSnapshotNoMatch for stale snap, got %v", err)
	}
}

// TestRebuildReappliesAttestSalienceBumps — Phase 11.5 replay invariant:
// after Attest mutates salience.Citations, drop+Rebuild must re-apply
// the same bump from the KindAttest journal entry so Citations is
// reproduced byte-exactly. AccessCount on the cited memories also
// recovers (citation implies access).
func TestRebuildReappliesAttestSalienceBumps(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 5)
	_, id, _, _ := ParseURI(uri)

	// Bump Citations once via Attest(success).
	if _, err := c.Attest(AttestOpts{
		IntentID:  "intent-1",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uri},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	pre, _, _ := salience.Read(c.s, id)
	if pre.Citations != 1 || pre.AccessCount != 1 {
		t.Fatalf("precondition: Citations=%d AccessCount=%d, want 1/1", pre.Citations, pre.AccessCount)
	}
	preOverall, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	// Drop + Rebuild.
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
	if res.PreOverallRoot != preOverall {
		t.Fatalf("captured pre-root differs from c.OverallRoot(): %x vs %x", res.PreOverallRoot, preOverall)
	}
	// SalienceBumpsApplied must be > 0 — we journaled one KindAttest with 1 cited ID.
	if res.SalienceBumpsApplied == 0 {
		t.Fatalf("SalienceBumpsApplied: got 0, want >= 1 (KindAttest replay)")
	}

	// Citations + AccessCount must round-trip.
	post, _, _ := salience.Read(c.s, id)
	if post.Citations != pre.Citations {
		t.Fatalf("Citations: pre=%d post=%d (replay must reproduce)", pre.Citations, post.Citations)
	}
	if post.AccessCount != pre.AccessCount {
		t.Fatalf("AccessCount: pre=%d post=%d (replay must reproduce)", pre.AccessCount, post.AccessCount)
	}
}

// TestRebuildReappliesFindAccessBumps — late-binding Find bumps
// AccessCount; replay must reproduce. Multiple Finds compound.
func TestRebuildReappliesFindAccessBumps(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 5)
	_, id, _, _ := ParseURI(uri)

	// Three late-binding Finds = 3 AccessCount bumps.
	for i := 0; i < 3; i++ {
		if _, err := c.Find(query.Query{
			Type:        []memory.Type{memory.TypePreference},
			Limit:       10,
			LateBinding: true,
		}); err != nil {
			t.Fatalf("Find #%d: %v", i, err)
		}
	}
	pre, _, _ := salience.Read(c.s, id)
	if pre.AccessCount != 3 {
		t.Fatalf("precondition: AccessCount=%d want 3", pre.AccessCount)
	}

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
	post, _, _ := salience.Read(c.s, id)
	if post.AccessCount != pre.AccessCount {
		t.Fatalf("AccessCount: pre=%d post=%d (replay must reproduce all 3 Find bumps)", pre.AccessCount, post.AccessCount)
	}
}

// TestRebuildReappliesAttestFailureDecrement — failure-with-factual_error
// decrements Citations; replay must reproduce the same end state. We
// pre-bump via Attest(success), then Attest(failure, factual_error), so
// final Citations=0; rebuild must produce Citations=0 (one bump + one
// decrement applied in journal order, not just from the head seed).
func TestRebuildReappliesAttestFailureDecrement(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 5)
	_, id, _, _ := ParseURI(uri)

	// Success: Citations 0 -> 1, AccessCount 0 -> 1.
	if _, err := c.Attest(AttestOpts{
		IntentID:  "intent-1",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uri},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest success: %v", err)
	}
	// Failure with factual_error: Citations 1 -> 0, AccessCount stays 1.
	if _, err := c.Attest(AttestOpts{
		IntentID:  "intent-1-retry",
		Outcome:   AttestOutcomeFailure,
		Reason:    AttestReasonFactualError,
		Cited:     []memory.URI{uri},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest failure: %v", err)
	}
	pre, _, _ := salience.Read(c.s, id)
	if pre.Citations != 0 || pre.AccessCount != 1 {
		t.Fatalf("precondition: Citations=%d AccessCount=%d want 0/1", pre.Citations, pre.AccessCount)
	}

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
	post, _, _ := salience.Read(c.s, id)
	if post.Citations != 0 {
		t.Fatalf("Citations: post=%d want 0 (replay must apply success+failure in order)", post.Citations)
	}
	if post.AccessCount != 1 {
		t.Fatalf("AccessCount: post=%d want 1 (only success bump touches AC)", post.AccessCount)
	}
}

// TestRebuildReappliesLearnedWeights — Phase 12 replay invariant: after
// cortex.Attest persists meta/salience_weights via KindLearnWeights, drop
// + Rebuild must reproduce the same Weights byte-exactly (round-trip
// through Encode/Decode). Pre-drop and post-rebuild OverallRoot are
// byte-identical (weights are sidecar, not anchored — but the
// KindLearnWeights journal entries DO contribute to journal_root).
func TestRebuildReappliesLearnedWeights(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 7)
	_, id, _, _ := ParseURI(uri)

	// Pump the cited memory's factor profile so the EMA step has a
	// non-degenerate gradient.
	preScore, _, _ := salience.Read(c.s, id)
	preScore.Citations = 80
	preScore.AccessCount = 25
	encoded, err := salience.Encode(preScore)
	if err != nil {
		t.Fatalf("encode pre-score: %v", err)
	}
	var u keys.ULID
	copy(u[:], id[:])
	b := c.s.DB().NewBatch()
	if err := b.Set(keys.SalienceKey(u), encoded, nil); err != nil {
		b.Close()
		t.Fatalf("seed salience: %v", err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		b.Close()
		t.Fatalf("commit seed: %v", err)
	}
	b.Close()

	// Three Attest cycles to accumulate weight drift.
	for i := 0; i < 3; i++ {
		if _, err := c.Attest(AttestOpts{
			IntentID: "i", Outcome: AttestOutcomeSuccess,
			Cited: []memory.URI{uri}, CreatedBy: "andrew",
		}); err != nil {
			t.Fatalf("Attest #%d: %v", i, err)
		}
	}
	preWeights, found, err := salience.ReadWeights(c.s)
	if err != nil || !found {
		t.Fatalf("ReadWeights pre-rebuild: found=%v err=%v", found, err)
	}
	if preWeights == salience.DefaultWeights() {
		t.Fatalf("preWeights should have drifted from DefaultWeights after 3 attests")
	}
	preOverall, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
	if res.PreOverallRoot != preOverall {
		t.Fatalf("captured pre-root differs from c.OverallRoot(): %x vs %x", res.PreOverallRoot, preOverall)
	}

	postWeights, found, err := salience.ReadWeights(c.s)
	if err != nil || !found {
		t.Fatalf("ReadWeights post-rebuild: found=%v err=%v", found, err)
	}
	// Weights byte-replay: the four learned factors and V must match
	// exactly. UpdatedAt/Updates are reproduced from the journal
	// payload (each KindLearnWeights re-applies and Increments
	// Updates), so they must match too.
	if postWeights.WR != preWeights.WR ||
		postWeights.WA != preWeights.WA ||
		postWeights.WC != preWeights.WC ||
		postWeights.WD != preWeights.WD ||
		postWeights.WV != preWeights.WV {
		t.Fatalf("learned weight drift across rebuild:\npre=%+v\npost=%+v", preWeights, postWeights)
	}
	if postWeights.Updates != preWeights.Updates {
		t.Fatalf("Updates counter drift: pre=%d post=%d", preWeights.Updates, postWeights.Updates)
	}
}

// TestFindHonoursLearnedWeights — Phase 12 live-ranking invariant:
// after writing per-actor learned weights heavily biased toward
// Citations, a stale-but-highly-cited memory must rank above a fresh-
// but-uncited one in the Find results. Under DefaultWeights the
// ordering would be the opposite.
func TestFindHonoursLearnedWeights(t *testing.T) {
	c := openCortex(t)
	now := c.now()
	// Memory A: fresh, no citations.
	uriA := writePref(t, c, "topicA", 3)
	_, idA, _, _ := ParseURI(uriA)
	// Memory B: stale (one half-life ago) but 100 citations.
	uriB := writePref(t, c, "topicB", 3)
	_, idB, _, _ := ParseURI(uriB)

	scA, _, _ := salience.Read(c.s, idA)
	scA.LastUsed = now.UnixNano()
	scA.AccessCount = 0
	scA.Citations = 0
	scA.Cached = salience.ColdScore(scA, now)
	encA, _ := salience.Encode(scA)

	scB, _, _ := salience.Read(c.s, idB)
	scB.LastUsed = now.Add(-time.Duration(salience.HalfLifeNanos)).UnixNano()
	scB.AccessCount = 50
	scB.Citations = 100
	scB.Cached = salience.ColdScore(scB, now)
	encB, _ := salience.Encode(scB)

	var uA, uB keys.ULID
	copy(uA[:], idA[:])
	copy(uB[:], idB[:])
	b := c.s.DB().NewBatch()
	if err := b.Set(keys.SalienceKey(uA), encA, nil); err != nil {
		b.Close()
		t.Fatalf("seed A: %v", err)
	}
	if err := b.Set(keys.SalienceKey(uB), encB, nil); err != nil {
		b.Close()
		t.Fatalf("seed B: %v", err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		b.Close()
		t.Fatalf("commit seed: %v", err)
	}
	b.Close()

	// Persist heavily-citation-biased learned weights directly so we
	// don't depend on EMA convergence.
	learned := salience.Weights{
		SchemaVersion: salience.WeightsSchemaVersion,
		WR:            0.05, WA: 0.05, WC: 0.75, WD: 0.10, WV: 0.05,
		UpdatedAt: now.UnixNano(),
		Updates:   1,
	}
	encW, err := salience.EncodeWeights(&learned)
	if err != nil {
		t.Fatalf("EncodeWeights: %v", err)
	}
	wb := c.s.BeginWrite()
	if err := wb.Set(keys.MetaSalienceWeights, encW); err != nil {
		wb.Abort()
		t.Fatalf("write learned weights: %v", err)
	}
	// Stage a journal entry so Commit doesn't reject the batch (replay
	// invariant: every store mutation MUST be journaled). KindRaw is
	// fine for a test seed — replay ignores unknown payloads at this
	// stage and we drop+rebuild downstream anyway.
	if err := wb.AppendJournal(&journal.Entry{Kind: "test_seed", Payload: []byte("phase12-find-learned-weights")}); err != nil {
		wb.Abort()
		t.Fatalf("append journal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("commit weights: %v", err)
	}

	// Now Find should rank B (stale + highly cited) above A (fresh, no
	// citations) under the learned weights.
	result, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Memories) < 2 {
		t.Fatalf("Find: got %d hits, want >= 2", len(result.Memories))
	}

	// Locate A and B in the hit list.
	var posA, posB int = -1, -1
	for i, m := range result.Memories {
		if m.Head.ID == idA {
			posA = i
		}
		if m.Head.ID == idB {
			posB = i
		}
	}
	if posA < 0 || posB < 0 {
		t.Fatalf("Find: didn't return both seeded memories (posA=%d posB=%d)", posA, posB)
	}
	if !(posB < posA) {
		t.Fatalf("Find under learned weights: posB=%d posA=%d (want posB<posA \u2014 citation-biased weights should rank cited memory higher)",
			posB, posA)
	}
}

// TestRebuildLearnedWeightsColdStartIdempotent — when no Attest has ever
// run (no KindLearnWeights entries in the journal), rebuild produces no
// meta/salience_weights record; cold-start ReadWeights returns
// (DefaultWeights, false, nil) both before and after.
func TestRebuildLearnedWeightsColdStartIdempotent(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "topic", 5)

	_, foundPre, err := salience.ReadWeights(c.s)
	if err != nil {
		t.Fatalf("ReadWeights pre: %v", err)
	}
	if foundPre {
		t.Fatalf("precondition: no learn entries yet, weights should be absent")
	}

	if _, err := c.Rebuild(RebuildOptions{}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	_, foundPost, err := salience.ReadWeights(c.s)
	if err != nil {
		t.Fatalf("ReadWeights post: %v", err)
	}
	if foundPost {
		t.Fatalf("post-rebuild: weights should still be absent (no journal LearnWeights)")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
