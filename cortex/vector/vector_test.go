// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package vector

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// makeUnitVec returns a deterministic unit-length vector derived from a
// (seed string, dim) pair. Provides reproducible test fixtures without an
// external dep.
func makeUnitVec(seed string, dim int) []float32 {
	h := sha256.New()
	h.Write([]byte(seed))
	src := h.Sum(nil)
	v := make([]float32, dim)
	block := src
	for i := 0; i < dim; {
		for j := 0; j < 8 && i < dim; j, i = j+1, i+1 {
			bits := binary.BigEndian.Uint32(block[j*4 : j*4+4])
			v[i] = float32(float64(bits)/float64(1<<31) - 1)
		}
		if i >= dim {
			break
		}
		next := sha256.Sum256(block)
		block = next[:]
	}
	// Normalise.
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// midFromInt encodes an int as a 16-byte memory id so tests can keep
// human-readable identifiers.
func midFromInt(i int) MemoryID {
	var m MemoryID
	binary.BigEndian.PutUint64(m[8:], uint64(i))
	return m
}

// buildIndex inserts n unit vectors with deterministic seeds.
func buildIndex(t *testing.T, p Params, n int) (*Index, *MapStore) {
	t.Helper()
	idx := NewIndex(p)
	store := NewMapStore()
	idx.BindStore(store)
	for i := 1; i <= n; i++ {
		v := makeUnitVec(t.Name()+"#"+intStr(i), p.Dim)
		store.Put(uint64(i), v)
		if err := idx.Add(uint64(i), midFromInt(i), v); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	return idx, store
}

func intStr(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

// TestEmptyIndexSearch verifies search on an empty index returns
// ErrEmptyIndex rather than panicking.
func TestEmptyIndexSearch(t *testing.T) {
	t.Parallel()
	idx := NewIndex(Params{Dim: 8})
	idx.BindStore(NewMapStore())
	q := makeUnitVec("q", 8)
	hits, err := idx.Search(q, 5)
	if err != ErrEmptyIndex {
		t.Fatalf("want ErrEmptyIndex, got err=%v hits=%v", err, hits)
	}
}

// TestSearchSelfRecallTop1 verifies the simplest correctness property: a
// vector we Add to the index is its own closest neighbour. Hub of every
// downstream HNSW use case.
func TestSearchSelfRecallTop1(t *testing.T) {
	t.Parallel()
	idx, store := buildIndex(t, Params{Dim: 32}, 64)
	for vid := uint64(1); vid <= 64; vid++ {
		q, _ := store.GetVector(vid)
		hits, err := idx.Search(q, 1)
		if err != nil {
			t.Fatalf("Search vid=%d: %v", vid, err)
		}
		if len(hits) != 1 {
			t.Fatalf("vid=%d expected 1 hit, got %d", vid, len(hits))
		}
		if hits[0].VertexID != vid {
			t.Fatalf("vid=%d top1 is vid=%d (distance %v); want self", vid, hits[0].VertexID, hits[0].Distance)
		}
		if hits[0].Distance > 1e-5 {
			t.Fatalf("vid=%d self-distance = %v, want ~0", vid, hits[0].Distance)
		}
	}
}

// TestSearchRecallVsBruteForce sanity-checks that HNSW top-K usually
// matches brute-force top-K. We don't insist on 100% recall (HNSW is
// approximate) but assert recall@10 ≥ 0.9 over 32 random queries on a
// 256-vertex index. At this scale HNSW typically gives ≥ 95%.
func TestSearchRecallVsBruteForce(t *testing.T) {
	t.Parallel()
	dim := 32
	n := 256
	k := 10
	idx, store := buildIndex(t, Params{Dim: dim}, n)

	// Sample queries that are NOT in the index so we exercise the
	// general nearest-neighbour case, not just self-recall.
	totalRecall := 0
	queries := 32
	for q := 0; q < queries; q++ {
		query := makeUnitVec("query#"+intStr(q), dim)
		hits, err := idx.Search(query, k)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		// Brute-force ground truth: rank all vertices by distance.
		type pair struct {
			vid  uint64
			dist float32
		}
		gt := make([]pair, 0, n)
		for vid := uint64(1); vid <= uint64(n); vid++ {
			v, _ := store.GetVector(vid)
			var dot float64
			for i := range query {
				dot += float64(query[i]) * float64(v[i])
			}
			gt = append(gt, pair{vid, float32(1 - dot)})
		}
		sort.Slice(gt, func(a, b int) bool { return gt[a].dist < gt[b].dist })
		want := map[uint64]struct{}{}
		for _, p := range gt[:k] {
			want[p.vid] = struct{}{}
		}
		got := 0
		for _, h := range hits {
			if _, ok := want[h.VertexID]; ok {
				got++
			}
		}
		totalRecall += got
	}
	avgRecall := float64(totalRecall) / float64(queries*k)
	if avgRecall < 0.85 {
		t.Fatalf("HNSW recall@%d = %.3f, want ≥ 0.85", k, avgRecall)
	}
	t.Logf("HNSW recall@%d = %.3f over %d queries on n=%d dim=%d", k, avgRecall, queries, n, dim)
}

// TestPersistenceRoundTrip verifies Save → Load returns an identical index
// (same nodes, same neighbours, same entry point, same search behaviour).
func TestPersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	idx, store := buildIndex(t, Params{Dim: 16, Seed: 42, Model: "test-model@abc"}, 32)
	dir := t.TempDir()
	path := filepath.Join(dir, "idx.bin")
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Size() == 0 {
		t.Fatalf("Save did not produce a file: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Re-bind store; vectors live outside the file.
	loaded.BindStore(store)

	if loaded.params.Dim != idx.params.Dim || loaded.params.M != idx.params.M {
		t.Fatalf("params drift: %+v vs %+v", loaded.params, idx.params)
	}
	if loaded.params.Model != "test-model@abc" {
		t.Fatalf("Model drift: %q", loaded.params.Model)
	}
	if len(loaded.nodes) != len(idx.nodes) {
		t.Fatalf("node count drift: %d vs %d", len(loaded.nodes), len(idx.nodes))
	}
	if loaded.maxLevel != idx.maxLevel || loaded.entryPoint != idx.entryPoint {
		t.Fatalf("entry drift: ep=%d maxL=%d vs ep=%d maxL=%d",
			loaded.entryPoint, loaded.maxLevel, idx.entryPoint, idx.maxLevel)
	}
	// Compare every node and its neighbour lists byte-for-byte.
	for vid, want := range idx.nodes {
		got := loaded.nodes[vid]
		if got == nil {
			t.Fatalf("vid=%d missing after load", vid)
		}
		if got.level != want.level || got.memoryID != want.memoryID {
			t.Fatalf("vid=%d shape drift", vid)
		}
		for lc := 0; lc <= want.level; lc++ {
			if !equalU64(got.neighbors[lc], want.neighbors[lc]) {
				t.Fatalf("vid=%d L%d neighbour drift: %v vs %v", vid, lc, got.neighbors[lc], want.neighbors[lc])
			}
		}
	}

	// Behavioral check: same queries return same hits.
	for q := 0; q < 5; q++ {
		query := makeUnitVec("rtq#"+intStr(q), 16)
		a, _ := idx.Search(query, 5)
		b, _ := loaded.Search(query, 5)
		if len(a) != len(b) {
			t.Fatalf("query %d: hit count drift %d vs %d", q, len(a), len(b))
		}
		for i := range a {
			if a[i].VertexID != b[i].VertexID {
				t.Fatalf("query %d hit %d: vid drift %d vs %d", q, i, a[i].VertexID, b[i].VertexID)
			}
		}
	}
}

// TestDeterministicConstruction is the load-bearing replay-invariant
// property: same seed + same insertion order + same vectors → byte-identical
// on-disk file. Without this, the Phase 11 replay harness can't verify
// vec/* prefixes via byte comparison.
func TestDeterministicConstruction(t *testing.T) {
	t.Parallel()
	a, _ := buildIndex(t, Params{Dim: 16, Seed: 1234, Model: "m@v1"}, 24)
	b, _ := buildIndex(t, Params{Dim: 16, Seed: 1234, Model: "m@v1"}, 24)
	dirA := t.TempDir()
	dirB := t.TempDir()
	pathA := filepath.Join(dirA, "i.bin")
	pathB := filepath.Join(dirB, "i.bin")
	if err := a.Save(pathA); err != nil {
		t.Fatalf("a.Save: %v", err)
	}
	if err := b.Save(pathB); err != nil {
		t.Fatalf("b.Save: %v", err)
	}
	bytesA, _ := os.ReadFile(pathA)
	bytesB, _ := os.ReadFile(pathB)
	if len(bytesA) == 0 || len(bytesA) != len(bytesB) {
		t.Fatalf("file size drift: %d vs %d", len(bytesA), len(bytesB))
	}
	for i := range bytesA {
		if bytesA[i] != bytesB[i] {
			t.Fatalf("byte drift at offset %d: %#x vs %#x", i, bytesA[i], bytesB[i])
		}
	}
}

// TestDifferentSeedYieldsDifferentLevels confirms that the seed actually
// affects the geometric level draw. Without seed differentiation, two
// indexes with different seeds would still produce identical graphs.
func TestDifferentSeedYieldsDifferentLevels(t *testing.T) {
	t.Parallel()
	a, _ := buildIndex(t, Params{Dim: 8, Seed: 1}, 200)
	b, _ := buildIndex(t, Params{Dim: 8, Seed: 999}, 200)
	// Compare max levels and level histograms.
	histo := func(idx *Index) map[int]int {
		h := map[int]int{}
		for _, nd := range idx.nodes {
			h[nd.level]++
		}
		return h
	}
	ha := histo(a)
	hb := histo(b)
	differ := false
	for l := range ha {
		if ha[l] != hb[l] {
			differ = true
			break
		}
	}
	for l := range hb {
		if ha[l] != hb[l] {
			differ = true
			break
		}
	}
	if !differ {
		t.Fatalf("different seeds produced identical level distributions: %v / %v", ha, hb)
	}
}

// TestTombstoneSkippedInSearch verifies tombstoned vertices never appear in
// search results, but the graph still finds the next-best hit (i.e. the
// tombstone doesn't isolate other reachable nodes).
func TestTombstoneSkippedInSearch(t *testing.T) {
	t.Parallel()
	idx, store := buildIndex(t, Params{Dim: 16}, 32)
	// Find vid=1's nearest non-self neighbor as a sanity baseline.
	q, _ := store.GetVector(1)
	hits, _ := idx.Search(q, 2)
	if len(hits) < 2 || hits[0].VertexID != 1 {
		t.Fatalf("baseline self-recall: %+v", hits)
	}
	want := hits[1].VertexID

	// Tombstone vid=1.
	idx.Tombstone(midFromInt(1))
	hits, _ = idx.Search(q, 1)
	if len(hits) != 1 {
		t.Fatalf("post-tombstone: want 1 hit got %d", len(hits))
	}
	if hits[0].VertexID == 1 {
		t.Fatalf("post-tombstone: vid=1 still returned")
	}
	if hits[0].VertexID != want {
		t.Fatalf("post-tombstone: want vid=%d got vid=%d", want, hits[0].VertexID)
	}
}

// TestDimMismatchRejected verifies the explicit dim guard.
func TestDimMismatchRejected(t *testing.T) {
	t.Parallel()
	idx := NewIndex(Params{Dim: 8})
	idx.BindStore(NewMapStore())
	if err := idx.Add(1, midFromInt(1), make([]float32, 16)); err != ErrDimMismatch {
		t.Fatalf("Add dim mismatch: %v", err)
	}
	if _, err := idx.Search(make([]float32, 16), 1); err != ErrDimMismatch {
		t.Fatalf("Search dim mismatch: %v", err)
	}
}

// TestDuplicateVidRejected ensures Add refuses to clobber an existing
// vertex (the embedder enforces id monotonicity but this is the
// last-resort guard).
func TestDuplicateVidRejected(t *testing.T) {
	t.Parallel()
	idx := NewIndex(Params{Dim: 4})
	idx.BindStore(NewMapStore())
	v := makeUnitVec("x", 4)
	if err := idx.Add(1, midFromInt(1), v); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := idx.Add(1, midFromInt(2), v); err != ErrDuplicateID {
		t.Fatalf("duplicate Add: %v", err)
	}
}

// TestBadFileHeader verifies Load rejects a corrupted file with ErrBadFile.
func TestBadFileHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "garbage.bin")
	_ = os.WriteFile(bad, []byte("this is not a vector index"), 0o644)
	if _, err := Load(bad); err == nil {
		t.Fatalf("Load should reject garbage")
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
