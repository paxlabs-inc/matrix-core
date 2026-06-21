// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package query

import (
	"math"
	"testing"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/store"
	"matrix/cortex/vector"
)

// normVec returns a unit-normalized copy of v so it satisfies the vector
// package's unit-norm distance contract (distance = 1 - dot under unit norm).
func normVec(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// writeRankMemory writes a real Head + Version + salience.Score for one
// Preference memory directly through the store batch path (no fakes — these
// are the exact bytes cortex.Write would persist) and inserts its vector
// into the HNSW index. Returns the memory ID.
func writeRankMemory(t *testing.T, s *store.Store, idx *vector.Index, vstore *vector.MapStore, vid uint64, importance uint8, citations uint64, vec []float32, now time.Time) memory.ID {
	t.Helper()
	id := memory.NewID()
	var u keys.ULID
	copy(u[:], id[:])

	h := memory.Head{
		ID:                 id,
		Type:               memory.TypePreference,
		CurrentVersion:     1,
		ActorScope:         "andrew",
		Visibility:         memory.VisPrivate,
		DeclaredImportance: importance,
		LastUpdatedAt:      now,
	}
	v := memory.Version{
		ID:         id,
		Version:    1,
		Type:       memory.TypePreference,
		CreatedAt:  now,
		CreatedBy:  "andrew",
		Confidence: 1.0,
	}
	sc := salience.Score{
		SchemaVersion: salience.SchemaVersion,
		LastUsed:      now.UnixNano(),
		Citations:     citations,
		Importance:    importance,
		ComputedAt:    now.UnixNano(),
	}

	headBytes, err := memory.EncodeHead(&h)
	if err != nil {
		t.Fatalf("EncodeHead: %v", err)
	}
	verBytes, err := memory.EncodeVersion(&v)
	if err != nil {
		t.Fatalf("EncodeVersion: %v", err)
	}
	scBytes, err := salience.Encode(&sc)
	if err != nil {
		t.Fatalf("salience.Encode: %v", err)
	}

	wb := s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(keys.MemoryHeadKey(u), headBytes); err != nil {
		t.Fatalf("set head: %v", err)
	}
	if err := wb.Set(keys.MemoryVersionKey(u, 1), verBytes); err != nil {
		t.Fatalf("set version: %v", err)
	}
	if err := wb.Set(keys.SalienceKey(u), scBytes); err != nil {
		t.Fatalf("set salience: %v", err)
	}
	// The store enforces that every committed batch journals at least once
	// (replay invariant). Mirror cortex.Write's KindWrite entry.
	wpBytes, err := journal.EncodeWritePayload(&journal.WritePayload{
		SchemaVersion: 1,
		ID:            id,
		Version:       1,
		Type:          uint8(memory.TypePreference),
	})
	if err != nil {
		t.Fatalf("encode write payload: %v", err)
	}
	if err := wb.AppendJournal(&journal.Entry{
		Kind:      journal.KindWrite,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte("andrew"),
		Payload:   wpBytes,
	}); err != nil {
		t.Fatalf("append journal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	unit := normVec(vec)
	vstore.Put(vid, unit)
	if err := idx.Add(vid, vector.MemoryID(id), unit); err != nil {
		t.Fatalf("idx.Add: %v", err)
	}
	return id
}

// TestRunNearRanksSalienceOverDistance is the v3 #4 rank-order assertion. A
// high-citation memory placed at a MEDIUM HNSW distance must outrank a
// zero-citation memory at a LOW distance under the default (salience-primary)
// rank mode — utility beats similarity. The same query under RankDistance
// must flip back to closest-first. Real store, real HNSW index, real
// salience scores — no fakes.
func TestRunNearRanksSalienceOverDistance(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const dim = 4
	idx := vector.NewIndex(vector.Params{Dim: dim})
	vstore := vector.NewMapStore()
	idx.BindStore(vstore)

	now := time.Now().UTC()
	// nearID: very close to the query vector, zero citations.
	nearID := writeRankMemory(t, s, idx, vstore, 1, 3, 0,
		[]float32{1, 0.1, 0, 0}, now)
	// citedID: a wider angle (medium distance) but 100 citations.
	citedID := writeRankMemory(t, s, idx, vstore, 2, 3, 100,
		[]float32{1, 1, 0, 0}, now)

	qvec := normVec([]float32{1, 0, 0, 0})

	// --- default (salience-primary): cited > near ---
	res, err := Run(s, Query{
		NearVector: qvec,
		NearIndex:  idx,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Run (salience): %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(res.Memories))
	}
	// Sanity: the distance ordering is genuinely the OPPOSITE of the wanted
	// rank, so this proves salience (not distance) drove the result.
	if !(res.Distances[nearID] < res.Distances[citedID]) {
		t.Fatalf("precondition: near should be closer than cited: near=%f cited=%f",
			res.Distances[nearID], res.Distances[citedID])
	}
	if !(res.Scores[citedID] > res.Scores[nearID]) {
		t.Fatalf("precondition: cited salience should exceed near: cited=%f near=%f",
			res.Scores[citedID], res.Scores[nearID])
	}
	if res.Memories[0].Head.ID != citedID {
		t.Fatalf("salience-primary: want cited first, got %s (cited=%s near=%s)",
			res.Memories[0].Head.ID, citedID, nearID)
	}

	// --- RankDistance rollback: near (closest) first ---
	resD, err := Run(s, Query{
		NearVector: qvec,
		NearIndex:  idx,
		Limit:      10,
		RankMode:   RankDistance,
	})
	if err != nil {
		t.Fatalf("Run (distance): %v", err)
	}
	if len(resD.Memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(resD.Memories))
	}
	if resD.Memories[0].Head.ID != nearID {
		t.Fatalf("distance rollback: want near first, got %s (cited=%s near=%s)",
			resD.Memories[0].Head.ID, citedID, nearID)
	}
}

// TestWithinValidity exercises the v3 #2 bi-temporal valid-time predicate on
// real memory.Version values: ValidFrom defaults to CreatedAt, ValidUntil is
// a half-open upper bound, and the previously-dead ExpiresAt is honoured. No
// fakes — these are the exact struct shapes query.Run decodes from the store.
func TestWithinValidity(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	mk := func(mut func(v *memory.Version)) *memory.Version {
		v := &memory.Version{CreatedAt: base}
		mut(v)
		return v
	}
	hour := time.Hour
	close1 := base.Add(hour)
	exp := base.Add(2 * hour)

	cases := []struct {
		name string
		v    *memory.Version
		asOf time.Time
		want bool
	}{
		{"open interval, asOf after CreatedAt", mk(func(*memory.Version) {}), base.Add(hour), true},
		{"asOf before CreatedAt (default ValidFrom)", mk(func(*memory.Version) {}), base.Add(-hour), false},
		{"explicit ValidFrom in future", mk(func(v *memory.Version) { f := base.Add(hour); v.ValidFrom = &f }), base, false},
		{"explicit ValidFrom reached", mk(func(v *memory.Version) { f := base.Add(hour); v.ValidFrom = &f }), base.Add(hour), true},
		{"closed: asOf before ValidUntil", mk(func(v *memory.Version) { v.ValidUntil = &close1 }), base.Add(30 * time.Minute), true},
		{"closed: asOf == ValidUntil (half-open)", mk(func(v *memory.Version) { v.ValidUntil = &close1 }), close1, false},
		{"closed: asOf after ValidUntil", mk(func(v *memory.Version) { v.ValidUntil = &close1 }), close1.Add(time.Minute), false},
		{"expired: asOf before ExpiresAt", mk(func(v *memory.Version) { v.ExpiresAt = &exp }), base.Add(hour), true},
		{"expired: asOf at ExpiresAt", mk(func(v *memory.Version) { v.ExpiresAt = &exp }), exp, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withinValidity(tc.v, tc.asOf); got != tc.want {
				t.Fatalf("withinValidity = %v, want %v", got, tc.want)
			}
		})
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
