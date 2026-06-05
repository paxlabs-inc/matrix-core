// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// idsEqualSet returns true iff the two slices contain the same memory IDs
// regardless of ordering. Tiny test helper local to this file.
func idsEqualSet(a, b []memory.ID) bool {
	if len(a) != len(b) {
		return false
	}
	as := make([]memory.ID, len(a))
	bs := make([]memory.ID, len(b))
	copy(as, a)
	copy(bs, b)
	sort.Slice(as, func(i, j int) bool { return bytes.Compare(as[i][:], as[j][:]) < 0 })
	sort.Slice(bs, func(i, j int) bool { return bytes.Compare(bs[i][:], bs[j][:]) < 0 })
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// resultIDs returns the IDs from a query.Result in the order given.
func resultIDs(r *query.Result) []memory.ID {
	out := make([]memory.ID, len(r.Memories))
	for i, m := range r.Memories {
		out[i] = m.Head.ID
	}
	return out
}

// TestAddEdgeWritesForwardAndReverse verifies the §11.1 atomic-batch invariant:
// forward and reverse edge keys hold the same canonical bytes after one
// AddEdge call.
func TestAddEdgeWritesForwardAndReverse(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{
		CreatedBy: "andrew",
		Weight:    0.5,
	}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	srcU := keys.ULID{}
	copy(srcU[:], srcID[:])
	dstU := keys.ULID{}
	copy(dstU[:], dstID[:])

	fwd, ok, err := c.s.Get(keys.EdgeFromKey(srcU, byte(memory.EdgeReferences), dstU))
	if err != nil || !ok {
		t.Fatalf("forward Get: ok=%v err=%v", ok, err)
	}
	rev, ok, err := c.s.Get(keys.EdgeToKey(dstU, byte(memory.EdgeReferences), srcU))
	if err != nil || !ok {
		t.Fatalf("reverse Get: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(fwd, rev) {
		t.Fatalf("forward and reverse edge bytes differ: fwd=%x rev=%x", fwd, rev)
	}

	var rec memory.EdgeRecord
	if err := memory.DecodeEdge(fwd, &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Type != memory.EdgeReferences || rec.Src != srcID || rec.Dst != dstID {
		t.Fatalf("record fields wrong: %+v", rec)
	}
	if rec.Tombstoned {
		t.Fatalf("expected live edge, got Tombstoned=true")
	}
	if rec.Weight != 0.5 {
		t.Fatalf("weight=%v", rec.Weight)
	}
}

// TestAddEdgeIdempotent: repeated AddEdge on a live (src,type,dst) is a
// no-op. Exactly one journal entry is written.
func TestAddEdgeIdempotent(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	pre := c.s.NextSeq()
	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge#1: %v", err)
	}
	mid := c.s.NextSeq()
	if mid != pre+1 {
		t.Fatalf("expected exactly one journal entry, pre=%d mid=%d", pre, mid)
	}
	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge#2: %v", err)
	}
	post := c.s.NextSeq()
	if post != mid {
		t.Fatalf("idempotent AddEdge journaled (mid=%d post=%d)", mid, post)
	}
}

// TestAddEdgeJournalsKindAddEdge: KindAddEdge entry carries a decodable
// EdgePayload that round-trips back to the input.
func TestAddEdgeJournalsKindAddEdge(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	if err := c.AddEdge(srcID, memory.EdgeCorroborates, dstID, AddEdgeMeta{
		CreatedBy: "andrew",
		Weight:    0.9,
	}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// Find the most recent journal entry and verify shape.
	var last *journal.Entry
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		if e.Kind == journal.KindAddEdge {
			cp := *e
			cp.Payload = append([]byte(nil), e.Payload...)
			cp.CreatedBy = append([]byte(nil), e.CreatedBy...)
			last = &cp
		}
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if last == nil {
		t.Fatalf("no KindAddEdge journal entry written")
	}
	var pl journal.EdgePayload
	if err := journal.DecodeEdgePayload(last.Payload, &pl); err != nil {
		t.Fatalf("DecodeEdgePayload: %v", err)
	}
	if pl.SchemaVersion != 1 {
		t.Fatalf("schema=%d", pl.SchemaVersion)
	}
	if memory.EdgeType(pl.Type) != memory.EdgeCorroborates {
		t.Fatalf("type=%d", pl.Type)
	}
	if memory.ID(pl.Src) != srcID || memory.ID(pl.Dst) != dstID {
		t.Fatalf("src/dst mismatch: %+v", pl)
	}
	if pl.Weight != 0.9 {
		t.Fatalf("weight=%v", pl.Weight)
	}
	if pl.Tombstoned {
		t.Fatalf("Tombstoned=true on KindAddEdge")
	}
}

// TestAddEdgeRejectsSelfAndInvalid asserts the input-validation guards.
func TestAddEdgeRejectsSelfAndInvalid(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	srcID := idOf(a)

	if err := c.AddEdge(srcID, memory.EdgeReferences, srcID, AddEdgeMeta{}); !errors.Is(err, memory.ErrSelfEdge) {
		t.Fatalf("expected ErrSelfEdge, got %v", err)
	}
	if err := c.AddEdge(srcID, memory.EdgeType(0xFF), memory.ID{1}, AddEdgeMeta{}); !errors.Is(err, memory.ErrInvalidEdgeType) {
		t.Fatalf("expected ErrInvalidEdgeType, got %v", err)
	}
}

// TestRemoveEdgeMarksTombstoned: keys remain in the store with
// Tombstoned=true after RemoveEdge; GetEdge surfaces the tombstone.
func TestRemoveEdgeMarksTombstoned(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "obsolete", "andrew"); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}

	rec, err := c.GetEdge(srcID, memory.EdgeReferences, dstID)
	if err != nil {
		t.Fatalf("GetEdge after Remove: %v", err)
	}
	if !rec.Tombstoned {
		t.Fatalf("expected Tombstoned=true, got %+v", rec)
	}
	if rec.TombstonedReason != "obsolete" || rec.TombstonedBy != "andrew" {
		t.Fatalf("audit fields wrong: %+v", rec)
	}

	// Reverse direction also tombstoned (atomic batch).
	srcU := keys.ULID{}
	copy(srcU[:], srcID[:])
	dstU := keys.ULID{}
	copy(dstU[:], dstID[:])
	rev, ok, err := c.s.Get(keys.EdgeToKey(dstU, byte(memory.EdgeReferences), srcU))
	if err != nil || !ok {
		t.Fatalf("reverse get: ok=%v err=%v", ok, err)
	}
	var revRec memory.EdgeRecord
	if err := memory.DecodeEdge(rev, &revRec); err != nil {
		t.Fatalf("decode reverse: %v", err)
	}
	if !revRec.Tombstoned {
		t.Fatalf("reverse not tombstoned: %+v", revRec)
	}
}

// TestRemoveEdgeIdempotent: missing or already-tombstoned edge → nil, no
// new journal entry.
func TestRemoveEdgeIdempotent(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	// Missing → nil, no journal.
	pre := c.s.NextSeq()
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "x", "x"); err != nil {
		t.Fatalf("RemoveEdge missing: %v", err)
	}
	if c.s.NextSeq() != pre {
		t.Fatalf("RemoveEdge missing journaled: pre=%d post=%d", pre, c.s.NextSeq())
	}

	// Add then remove twice. Second remove is no-op.
	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "x", "x"); err != nil {
		t.Fatalf("RemoveEdge#1: %v", err)
	}
	mid := c.s.NextSeq()
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "x", "x"); err != nil {
		t.Fatalf("RemoveEdge#2: %v", err)
	}
	if c.s.NextSeq() != mid {
		t.Fatalf("idempotent RemoveEdge journaled: mid=%d post=%d", mid, c.s.NextSeq())
	}
}

// TestAddEdgeRevivesTombstoned: AddEdge on a tombstoned edge revives it
// (Tombstoned flips back to false; one new KindAddEdge journal entry).
func TestAddEdgeRevivesTombstoned(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	b := writePref(t, c, "verbosity", 5)
	srcID := idOf(a)
	dstID := idOf(b)

	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "x", "x"); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}
	if err := c.AddEdge(srcID, memory.EdgeReferences, dstID, AddEdgeMeta{}); err != nil {
		t.Fatalf("AddEdge revive: %v", err)
	}
	rec, err := c.GetEdge(srcID, memory.EdgeReferences, dstID)
	if err != nil {
		t.Fatalf("GetEdge after revive: %v", err)
	}
	if rec.Tombstoned {
		t.Fatalf("expected live edge after revive, got Tombstoned=true: %+v", rec)
	}
}

// TestIterEdgesOutFiltersTombstonedAndType verifies the iterator's
// default tombstone-skip and the per-Type filter.
func TestIterEdgesOutFiltersTombstonedAndType(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	srcID := idOf(a)

	if err := c.AddEdge(srcID, memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(srcID, memory.EdgeCorroborates, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveEdge(srcID, memory.EdgeReferences, idOf(b), "x", "x"); err != nil {
		t.Fatal(err)
	}

	// Default: only the live corroborates edge surfaces.
	var liveCount int
	if err := c.IterEdgesOut(srcID, IterEdgesOptions{}, func(_ *memory.EdgeRecord) error {
		liveCount++
		return nil
	}); err != nil {
		t.Fatalf("IterEdgesOut: %v", err)
	}
	if liveCount != 1 {
		t.Fatalf("liveCount=%d want 1", liveCount)
	}

	// IncludeTombstoned: both surface.
	var allCount int
	if err := c.IterEdgesOut(srcID, IterEdgesOptions{IncludeTombstoned: true}, func(_ *memory.EdgeRecord) error {
		allCount++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if allCount != 2 {
		t.Fatalf("allCount=%d want 2", allCount)
	}

	// Type filter: corroborates only.
	var typeCount int
	if err := c.IterEdgesOut(srcID, IterEdgesOptions{Types: []memory.EdgeType{memory.EdgeCorroborates}}, func(rec *memory.EdgeRecord) error {
		typeCount++
		if rec.Type != memory.EdgeCorroborates {
			t.Fatalf("unexpected type %v", rec.Type)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if typeCount != 1 {
		t.Fatalf("typeCount=%d want 1", typeCount)
	}
}

// TestFindFromOneHopOut: BFS from A returns direct outgoing neighbours.
func TestFindFromOneHopOut(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	srcID := idOf(a)
	if err := c.AddEdge(srcID, memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(srcID, memory.EdgeReferences, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 1},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(b), idOf(cc)}) {
		t.Fatalf("ids=%v want {b,c}", resultIDs(res))
	}
	for _, id := range resultIDs(res) {
		if res.Hops[id] != 1 {
			t.Fatalf("hop[%v]=%d want 1", id, res.Hops[id])
		}
	}
}

// TestFindFromMultiHopOut: 2-hop traversal A→B→C surfaces both at the
// correct hop counts (B=1, C=2).
func TestFindFromMultiHopOut(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(b), memory.EdgeReferences, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 2},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("len=%d want 2", len(res.Memories))
	}
	if res.Hops[idOf(b)] != 1 {
		t.Fatalf("hop[b]=%d want 1", res.Hops[idOf(b)])
	}
	if res.Hops[idOf(cc)] != 2 {
		t.Fatalf("hop[c]=%d want 2", res.Hops[idOf(cc)])
	}
	// Default order is hop-asc, so B (hop 1) precedes C (hop 2).
	if res.Memories[0].Head.ID != idOf(b) {
		t.Fatalf("hop-asc order broken: first=%v", res.Memories[0].Head.ID)
	}
}

// TestFindFromMinHops: MinHops=2 excludes 1-hop neighbours.
func TestFindFromMinHops(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(b), memory.EdgeReferences, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MinHops: 2, MaxHops: 2},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(cc)}) {
		t.Fatalf("ids=%v want {c}", resultIDs(res))
	}
}

// TestFindFromIncoming: DirIn returns incoming neighbours.
func TestFindFromIncoming(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	// b → a, c → a
	if err := c.AddEdge(idOf(b), memory.EdgeReferences, idOf(a), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(cc), memory.EdgeReferences, idOf(a), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirIn, MaxHops: 1},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(b), idOf(cc)}) {
		t.Fatalf("incoming ids=%v want {b,c}", resultIDs(res))
	}
}

// TestFindFromBoth: DirBoth unions out and in neighbours.
func TestFindFromBoth(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	// a → b, c → a
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(cc), memory.EdgeReferences, idOf(a), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirBoth, MaxHops: 1},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(b), idOf(cc)}) {
		t.Fatalf("both ids=%v want {b,c}", resultIDs(res))
	}
}

// TestFindFromEdgeTypeFilter: Types restricts the walk; a cross-type edge
// is not traversed.
func TestFindFromEdgeTypeFilter(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	cc := writePref(t, c, "c", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(a), memory.EdgeContradicts, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:  &a,
		Limit: 10,
		Follow: &query.EdgeExpr{
			Direction: query.DirOut,
			MaxHops:   1,
			Types:     []memory.EdgeType{memory.EdgeReferences},
		},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(b)}) {
		t.Fatalf("type-filtered ids=%v want {b}", resultIDs(res))
	}
}

// TestFindFromTombstonedEdgeSkipped: removed edges are not traversed by
// default; IncludeTombstoned restores them.
func TestFindFromTombstonedEdgeSkipped(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveEdge(idOf(a), memory.EdgeReferences, idOf(b), "x", "x"); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 1},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 0 {
		t.Fatalf("expected empty default traversal, got %d", len(res.Memories))
	}

	res2, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 1, IncludeTombstoned: true},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res2), []memory.ID{idOf(b)}) {
		t.Fatalf("IncludeTombstoned ids=%v want {b}", resultIDs(res2))
	}
}

// TestFindFromCycleTerminates: a graph cycle does not loop forever.
func TestFindFromCycleTerminates(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(b), memory.EdgeReferences, idOf(a), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 5},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	// A is excluded from results; only B remains, at hop 1 (BFS gives
	// shortest distance, so the cycle doesn't push it deeper).
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(b)}) {
		t.Fatalf("cycle ids=%v want {b}", resultIDs(res))
	}
	if res.Hops[idOf(b)] != 1 {
		t.Fatalf("hop[b]=%d want 1 (BFS shortest path)", res.Hops[idOf(b)])
	}
}

// TestFindFromMaxHopsCap: requesting MaxHops > MaxHopsCap fails fast.
func TestFindFromMaxHopsCap(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	_, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: query.MaxHopsCap + 1},
	})
	if err == nil || !errors.Is(err, query.ErrUnsupported) && !contains(err.Error(), "MaxHopsCap") {
		t.Fatalf("expected MaxHopsCap error, got %v", err)
	}
}

// TestFindFromCombinedWithWhere: traversal + Where post-filter.
func TestFindFromCombinedWithWhere(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 1)
	cc := writePref(t, c, "c", 9)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(cc), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}

	// Where DeclaredImportance > 5 keeps only c.
	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 1},
		Where:  query.Gt{Field: "head.declared_importance", Value: 5},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !idsEqualSet(resultIDs(res), []memory.ID{idOf(cc)}) {
		t.Fatalf("Where-filtered ids=%v want {c}", resultIDs(res))
	}
}

// TestFindFromExcludesAnchor: the From vertex is not in the result set
// even when a self-loop exists in the graph.
func TestFindFromExcludesAnchor(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "a", 5)
	b := writePref(t, c, "b", 5)
	if err := c.AddEdge(idOf(a), memory.EdgeReferences, idOf(b), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(idOf(b), memory.EdgeReferences, idOf(a), AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Find(query.Query{
		From:   &a,
		Limit:  10,
		Follow: &query.EdgeExpr{Direction: query.DirOut, MaxHops: 6},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, m := range res.Memories {
		if m.Head.ID == idOf(a) {
			t.Fatalf("From vertex leaked into results")
		}
	}
}

// contains is a tiny strings.Contains shim so the test file doesn't import
// strings just for the cap-error check above.
func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
