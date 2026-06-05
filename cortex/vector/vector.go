// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package vector implements a small pure-Go HNSW (Hierarchical Navigable
// Small World) index for the Matrix cortex's Phase 5 vector recall path.
//
// Spec: research/04-cortex.md §10 (vector recall as Phase 3 cascade), §13.1
// (vector index storage + per-actor model pin), §19 (HNSW parameters).
//
// Why HNSW: §15 latency target for Find Near is <100 ms in-process at
// reasonable corpus sizes; HNSW gives logarithmic search at modest recall
// loss vs. exact k-NN. The original paper (Malkov & Yashunin 2018,
// "Efficient and robust approximate nearest neighbor search using
// Hierarchical Navigable Small World graphs") describes the algorithm; the
// implementation here is a direct transcription of Algorithms 1-4 from §4
// of that paper, with these phase-5 simplifications:
//
//   - "Simple" neighbor selection (Algorithm 3) rather than the heuristic
//     variant (Algorithm 4). Slightly worse recall on high-dim data; trades
//     a few % recall for half the code complexity. The Index.SelectMode
//     constant flags this in case a future phase swaps to heuristic.
//   - In-memory graph with on-disk persistence as a single binary file.
//     The spec mentions usearch/hnswlib as preferred engines; both are cgo
//     and were declined for Phase 5 (Q1 lock, sess#5). The Engine name
//     persisted in the file header lets a future migration detect mismatch.
//   - Deterministic level assignment via a seeded math/rand.Rand. The seed
//     is a constructor input; the embedding worker derives it from the
//     actor name so replay is reproducible (see cortex/embedder.go).
//
// Distance: 1 - cosine similarity. Vectors are expected to be unit
// normalized (the embed.HashEmbedder and any production embedder we wire
// will normalize). Under unit norm, cosine reduces to a dot product, so
// distance = 1 - dot(a, b), which gives [0, 2] with 0 == identical.
//
// Persistence layout (LE-encoded for portability with cgo bindings if we
// ever swap back):
//
//	header (66 bytes):
//	  magic        [8]byte  // "MTX-VEC1"
//	  version      uint8    // file format version
//	  dim          uint16
//	  M            uint16
//	  M0           uint16
//	  efConstruction uint16
//	  efSearch     uint16
//	  maxLevel     uint8
//	  entryPoint   uint64
//	  nodeCount    uint64
//	  rngSeed      uint64
//	  modelDigest  [16]byte  // first 16 bytes of sha256(model id)
//	  reserved     [12]byte  // future use, zero
//	per-node records (variable):
//	  vertexID     uint64
//	  memoryID     [16]byte
//	  level        uint8
//	  for lc in 0..level:
//	    neighborCount uint16
//	    neighbors     [neighborCount]uint64
//
// The vector bytes themselves are NOT in this file — they live in
// vec/meta/<id> in Pebble. Rebuild scans vec/meta, calls Add() in
// journal-seq order, and writes a fresh index file. Replay invariant
// holds: drop the file, call Rebuild, get byte-identical bytes back
// (given the same seed + insertion order + neighbor selection).
package vector

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Magic prefixes the on-disk file so loaders can sanity-check the format
// before parsing anything that could mis-read garbage.
var fileMagic = [8]byte{'M', 'T', 'X', '-', 'V', 'E', 'C', '1'}

// FileFormatVersion is bumped on any breaking change to the on-disk layout.
// Loaders refuse files with a different version (no migration in Phase 5).
const FileFormatVersion uint8 = 1

// SelectMode names the neighbor-selection strategy this index uses. Phase 5
// ships only "simple"; a future "heuristic" mode would improve recall on
// clusters at the cost of more code.
const SelectMode = "simple"

// Errors returned by Index operations.
var (
	ErrDimMismatch  = errors.New("vector: query dim does not match index dim")
	ErrEmptyIndex   = errors.New("vector: search on empty index")
	ErrDuplicateID  = errors.New("vector: vertex id already present")
	ErrUnknownID    = errors.New("vector: vertex id not present")
	ErrBadFile      = errors.New("vector: malformed index file")
	ErrWrongVersion = errors.New("vector: index file version mismatch")
)

// MemoryID mirrors memory.ID without importing the memory package, keeping
// this package self-contained (the embedder bridges between memory.ID and
// vector.MemoryID at the boundary).
type MemoryID [16]byte

// Params controls HNSW construction and search. Zero values fall back to
// the spec §19 defaults so callers can pass Params{}.
type Params struct {
	Dim            int    // vector dimensionality; required if > 0
	M              int    // edges per node on upper layers; default 16
	M0             int    // edges per node on layer 0; default 2*M (32)
	EfConstruction int    // candidate-list size during insert; default 200
	EfSearch       int    // candidate-list size during search; default 64
	Seed           uint64 // RNG seed (level assignment); default deterministic per-Dim
	Model          string // embedding model id pinned to this index
}

// withDefaults returns p with zero fields replaced by spec §19 defaults.
func (p Params) withDefaults() Params {
	if p.Dim <= 0 {
		p.Dim = 768
	}
	if p.M <= 0 {
		p.M = 16
	}
	if p.M0 <= 0 {
		p.M0 = 2 * p.M
	}
	if p.EfConstruction <= 0 {
		p.EfConstruction = 200
	}
	if p.EfSearch <= 0 {
		p.EfSearch = 64
	}
	if p.Seed == 0 {
		// Deterministic per-Dim seed so empty-config indexes are still
		// reproducible. Real callers (embedder.go) override with a seed
		// derived from the actor name.
		h := fnv.New64a()
		_, _ = h.Write([]byte(fmt.Sprintf("matrix.vector.v1:dim=%d", p.Dim)))
		p.Seed = h.Sum64()
	}
	return p
}

// node is one vertex in the HNSW graph. Vectors live OUTSIDE the node
// struct (in vec/meta in Pebble) — the index borrows them via the
// VectorStore callback during search/insert. This keeps the in-memory
// graph compact even at 100k+ vertices.
type node struct {
	vertexID uint64
	memoryID MemoryID
	level    int
	// neighbors[lc] = vertex IDs connected at layer lc, sorted by distance
	// to this node ascending. Length capped at M (Mmax0 at layer 0).
	neighbors [][]uint64
	// tombstoned marks the node as logically deleted. Search skips
	// tombstoned vertices; the graph edges to them remain so we can
	// continue traversing through them as bridges. Phase 5 uses this
	// only when a memory is tombstoned (handled in the embedder hook).
	tombstoned bool
}

// VectorStore abstracts how the index loads vectors. The cortex passes a
// closure that fetches from vec/meta/<id>; tests pass an in-memory map.
// Returning ok=false signals "the index believes this vertex exists but
// its vector is unreachable", which is treated as a soft miss (skipped in
// search, errored on insert).
type VectorStore interface {
	GetVector(vertexID uint64) (vec []float32, ok bool)
}

// Index is a single per-actor HNSW graph. Safe for concurrent reads;
// callers must serialize concurrent Add/Remove (the embedder worker is the
// only writer and is single-goroutine, so this is satisfied in practice).
type Index struct {
	params Params
	rng    *rand.Rand

	mu         sync.RWMutex
	nodes      map[uint64]*node // vertex_id -> node
	byMemory   map[MemoryID]uint64
	entryPoint uint64
	hasEntry   bool
	maxLevel   int

	// mL is the level multiplier: 1 / ln(M). Cached to avoid recomputing
	// on every insert.
	mL float64

	// boundStore resolves vectors during Add/Search. Set via BindStore at
	// construction time by the embedder (production) or test setup (which
	// passes a MapStore). May be nil only if the index is empty and no
	// Add/Search calls have happened yet.
	boundStore VectorStore
}

// NewIndex constructs an empty index with the given params.
func NewIndex(p Params) *Index {
	p = p.withDefaults()
	src := rand.NewSource(int64(p.Seed))
	return &Index{
		params:   p,
		rng:      rand.New(src),
		nodes:    map[uint64]*node{},
		byMemory: map[MemoryID]uint64{},
		mL:       1.0 / math.Log(float64(p.M)),
	}
}

// Params returns the (defaulted) construction parameters.
func (i *Index) Params() Params { return i.params }

// Len returns the number of non-tombstoned vertices in the index.
func (i *Index) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	n := 0
	for _, nd := range i.nodes {
		if !nd.tombstoned {
			n++
		}
	}
	return n
}

// Lookup returns the vertex id bound to mid, or (0, false) if absent.
// Used by the embedder to detect re-embedding of an already-vectored
// memory.
func (i *Index) Lookup(mid MemoryID) (uint64, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	v, ok := i.byMemory[mid]
	return v, ok
}

// Tombstone marks the node bound to mid as logically deleted; subsequent
// Search calls skip it. The graph edges are retained so other nodes can
// still traverse through the deleted vertex. Idempotent.
func (i *Index) Tombstone(mid MemoryID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	v, ok := i.byMemory[mid]
	if !ok {
		return
	}
	if nd := i.nodes[v]; nd != nil {
		nd.tombstoned = true
	}
}

// MemoryID returns the memory.ID bound to a given vertex id, or
// (zero, false). Used by the search path to map results back to the
// Pebble key namespace.
func (i *Index) MemoryID(vid uint64) (MemoryID, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	nd, ok := i.nodes[vid]
	if !ok {
		return MemoryID{}, false
	}
	return nd.memoryID, true
}

// chooseLevel samples a level from the geometric distribution dictated by
// the §13.1 HNSW spec: level = floor(-ln(U) * mL). Higher levels are
// exponentially rarer.
func (i *Index) chooseLevel() int {
	u := i.rng.Float64()
	// Avoid log(0) producing +Inf.
	for u == 0 {
		u = i.rng.Float64()
	}
	l := math.Floor(-math.Log(u) * i.mL)
	// Sanity cap. Levels above ~16 are effectively impossible at any
	// realistic corpus size but the formula is unbounded in theory.
	if l > 16 {
		l = 16
	}
	if l < 0 {
		l = 0
	}
	return int(l)
}

// Add inserts vertex vid (bound to memory mid) with vector vec into the
// index. Caller is responsible for vertex-id monotonicity (the cortex
// embedder allocates from a per-actor counter).
//
// Algorithm 1 of the HNSW paper, transcribed:
//  1. Pick level l for the new node.
//  2. Walk down from the current entry point through layers > l using ef=1
//     (greedy nearest), refining the search entry.
//  3. From min(maxLevel, l) down to 0: search_layer(ef=efConstruction),
//     pick M closest, wire bidirectional edges, shrink the neighbour
//     of each victim if degree exceeds Mmax at that level.
//  4. If l > current max, become the new entry point.
func (i *Index) Add(vid uint64, mid MemoryID, vec []float32) error {
	if len(vec) != i.params.Dim {
		return ErrDimMismatch
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	if _, dup := i.nodes[vid]; dup {
		return ErrDuplicateID
	}
	level := i.chooseLevel()
	nd := &node{
		vertexID:  vid,
		memoryID:  mid,
		level:     level,
		neighbors: make([][]uint64, level+1),
	}
	// First node ever: just register and return.
	if !i.hasEntry {
		i.nodes[vid] = nd
		i.byMemory[mid] = vid
		i.entryPoint = vid
		i.hasEntry = true
		i.maxLevel = level
		return nil
	}

	// We need a vector store; use the closure-backed inMemoryStore so the
	// search functions can read this node's own vector during traversal
	// (it gets registered into the store via the embedder before Add).
	// To keep the API ergonomic, we stash this node's vector temporarily
	// in a per-insert overlay so search layer can resolve it as a
	// neighbor candidate after it is wired up.
	store := newOverlayStore(i, vid, vec)

	ep := i.entryPoint
	curDist := i.distFromQuery(store, vec, ep)

	// Phase A: descend through layers strictly above `level` with ef=1.
	for lc := i.maxLevel; lc > level; lc-- {
		ep, curDist = i.greedyDescend(store, vec, ep, curDist, lc)
	}

	// Phase B: at each layer from min(maxLevel, level) down to 0, run
	// search_layer with efConstruction, pick M neighbours, wire edges.
	for lc := minInt(i.maxLevel, level); lc >= 0; lc-- {
		candidates := i.searchLayer(store, vec, ep, i.params.EfConstruction, lc, vid)
		// Sort ascending by distance.
		sort.Slice(candidates, func(a, b int) bool { return candidates[a].dist < candidates[b].dist })

		Mmax := i.params.M
		if lc == 0 {
			Mmax = i.params.M0
		}
		neighbors := candidates
		if len(neighbors) > Mmax {
			neighbors = neighbors[:Mmax]
		}

		// Record this node's outgoing edges (sorted by distance asc).
		out := make([]uint64, 0, len(neighbors))
		for _, c := range neighbors {
			out = append(out, c.vertexID)
		}
		nd.neighbors[lc] = out

		// Insert this node into nodes/byMemory so the reverse-edge
		// distance probe can find its vector via the overlay.
		// (Done once below; for safety against the same iteration of the
		// search we register it lazily here only if not present.)
		if _, exists := i.nodes[vid]; !exists {
			i.nodes[vid] = nd
			i.byMemory[mid] = vid
		}

		// Wire reverse edges and shrink victims if degree blows past Mmax.
		for _, c := range neighbors {
			other := i.nodes[c.vertexID]
			if other == nil {
				continue
			}
			if lc >= len(other.neighbors) {
				// Other has no layer-lc list (its level < lc) — skip;
				// shouldn't happen given candidates come from layer lc.
				continue
			}
			other.neighbors[lc] = appendUnique(other.neighbors[lc], vid)
			if len(other.neighbors[lc]) > Mmax {
				other.neighbors[lc] = i.shrinkNeighbours(store, other, lc, Mmax)
			}
		}

		// Update entry-for-next-iter: nearest in this layer's candidates.
		if len(candidates) > 0 {
			ep = candidates[0].vertexID
		}
	}

	// Final registration (idempotent with the lazy register above).
	i.nodes[vid] = nd
	i.byMemory[mid] = vid

	// New global entry if this node breached the previous max level.
	if level > i.maxLevel {
		i.maxLevel = level
		i.entryPoint = vid
	}
	return nil
}

// greedyDescend takes ep at layer lc and walks to the closest neighbour
// repeatedly until no neighbour improves. Returns the final (ep, dist).
func (i *Index) greedyDescend(store VectorStore, q []float32, ep uint64, dist float32, lc int) (uint64, float32) {
	for {
		nd := i.nodes[ep]
		if nd == nil || lc >= len(nd.neighbors) {
			return ep, dist
		}
		improved := false
		bestID := ep
		bestDist := dist
		for _, n := range nd.neighbors[lc] {
			d := i.distFromQuery(store, q, n)
			if d < bestDist {
				bestDist = d
				bestID = n
				improved = true
			}
		}
		if !improved {
			return ep, dist
		}
		ep = bestID
		dist = bestDist
	}
}

// distFromQuery returns 1 - dot(q, vec(vid)) under the unit-norm assumption.
func (i *Index) distFromQuery(store VectorStore, q []float32, vid uint64) float32 {
	v, ok := store.GetVector(vid)
	if !ok || len(v) != len(q) {
		// Unreachable vertex → +Inf so it never wins a min-heap.
		return float32(math.MaxFloat32)
	}
	var dot float64
	for i := range q {
		dot += float64(q[i]) * float64(v[i])
	}
	return float32(1.0 - dot)
}

// candidate pairs a vertex id with its distance from the query for use in
// the search heaps.
type candidate struct {
	vertexID uint64
	dist     float32
}

// searchLayer is Algorithm 2 from the paper. Returns up to ef candidates
// closest to q at layer lc, starting from entry. excludeID may be set to
// skip a known-pending insert (so a node doesn't try to be its own
// neighbour during Add); 0 = no exclusion (vertex IDs are 1-indexed by
// the embedder).
func (i *Index) searchLayer(store VectorStore, q []float32, entry uint64, ef int, lc int, excludeID uint64) []candidate {
	visited := map[uint64]struct{}{entry: {}}
	entryDist := i.distFromQuery(store, q, entry)

	// Candidates: min-heap by distance ascending (closest popped first).
	cands := &minHeap{}
	heap.Init(cands)
	heap.Push(cands, candidate{entry, entryDist})

	// Dynamic nearest list W: max-heap by distance descending (furthest
	// popped first → easy to trim to ef).
	w := &maxHeap{}
	heap.Init(w)
	heap.Push(w, candidate{entry, entryDist})

	for cands.Len() > 0 {
		c := heap.Pop(cands).(candidate)
		// Furthest of W: if c is further than furthest, stop expanding.
		var f candidate
		if w.Len() > 0 {
			f = (*w)[0]
		}
		if w.Len() >= ef && c.dist > f.dist {
			break
		}
		nd := i.nodes[c.vertexID]
		if nd == nil || lc >= len(nd.neighbors) {
			continue
		}
		for _, n := range nd.neighbors[lc] {
			if n == excludeID {
				continue
			}
			if _, seen := visited[n]; seen {
				continue
			}
			visited[n] = struct{}{}
			d := i.distFromQuery(store, q, n)
			if w.Len() < ef || d < (*w)[0].dist {
				heap.Push(cands, candidate{n, d})
				heap.Push(w, candidate{n, d})
				if w.Len() > ef {
					heap.Pop(w)
				}
			}
		}
	}

	out := make([]candidate, 0, w.Len())
	for w.Len() > 0 {
		out = append(out, heap.Pop(w).(candidate))
	}
	return out
}

// shrinkNeighbours keeps the Mmax closest neighbours by recomputing
// distances against the node's own vector and sorting ascending.
func (i *Index) shrinkNeighbours(store VectorStore, nd *node, lc int, Mmax int) []uint64 {
	current := nd.neighbors[lc]
	v, ok := store.GetVector(nd.vertexID)
	if !ok {
		// Vector unreachable; we keep the first Mmax slots and pray.
		if len(current) > Mmax {
			return current[:Mmax]
		}
		return current
	}
	type rec struct {
		id   uint64
		dist float32
	}
	scored := make([]rec, 0, len(current))
	for _, n := range current {
		d := i.distFromQuery(store, v, n)
		scored = append(scored, rec{n, d})
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].dist < scored[b].dist })
	keep := scored
	if len(keep) > Mmax {
		keep = keep[:Mmax]
	}
	out := make([]uint64, 0, len(keep))
	for _, r := range keep {
		out = append(out, r.id)
	}
	return out
}

// Search returns the up-to-k vertex IDs closest to q, ordered closest
// first. Tombstoned vertices are skipped. Returns ErrEmptyIndex if the
// index has no entry point yet. Concurrent-safe with readers only.
// Uses the VectorStore bound via BindStore to resolve neighbour vectors;
// returns an error if no store is bound and the graph is non-empty.
func (i *Index) Search(q []float32, k int) ([]Hit, error) {
	if len(q) != i.params.Dim {
		return nil, ErrDimMismatch
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.hasEntry {
		return nil, ErrEmptyIndex
	}
	if k <= 0 {
		return nil, nil
	}
	store := i.boundStore
	if store == nil {
		return nil, errors.New("vector: Search: no VectorStore bound (call BindStore first)")
	}
	ep := i.entryPoint
	curDist := i.distFromQuery(store, q, ep)
	for lc := i.maxLevel; lc >= 1; lc-- {
		ep, curDist = i.greedyDescend(store, q, ep, curDist, lc)
	}
	cands := i.searchLayer(store, q, ep, max(i.params.EfSearch, k), 0, 0)
	sort.Slice(cands, func(a, b int) bool { return cands[a].dist < cands[b].dist })
	out := make([]Hit, 0, k)
	for _, c := range cands {
		nd := i.nodes[c.vertexID]
		if nd == nil || nd.tombstoned {
			continue
		}
		out = append(out, Hit{VertexID: c.vertexID, MemoryID: nd.memoryID, Distance: c.dist})
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// Hit is one returned candidate from Search. Distance is 1 - cosine in
// the unit-norm space (so [0, 2], 0 = identical).
type Hit struct {
	VertexID uint64
	MemoryID MemoryID
	Distance float32
}

// --- persistence ---------------------------------------------------------

// Save writes the graph to path atomically (writes to path+".tmp" then
// renames). The vectors are NOT included; rebuild reads them from the
// VectorStore.
func (i *Index) Save(path string) error {
	i.mu.RLock()
	defer i.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vector: mkdir: %w", err)
	}

	var buf bytes.Buffer
	if err := i.writeHeader(&buf); err != nil {
		return err
	}
	// Stable node order for deterministic file bytes: vertex id ascending.
	ids := make([]uint64, 0, len(i.nodes))
	for vid := range i.nodes {
		ids = append(ids, vid)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, vid := range ids {
		if err := i.writeNode(&buf, i.nodes[vid]); err != nil {
			return err
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (i *Index) writeHeader(w io.Writer) error {
	if _, err := w.Write(fileMagic[:]); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, FileFormatVersion); err != nil {
		return err
	}
	fields := []any{
		uint16(i.params.Dim),
		uint16(i.params.M),
		uint16(i.params.M0),
		uint16(i.params.EfConstruction),
		uint16(i.params.EfSearch),
		uint8(i.maxLevel),
		i.entryPoint,
		uint64(len(i.nodes)),
		i.params.Seed,
	}
	for _, f := range fields {
		if err := binary.Write(w, binary.LittleEndian, f); err != nil {
			return err
		}
	}
	// Model digest (first 16 bytes of sha256(model id), but we keep it
	// simple by truncating the string itself padded/clipped to 16 bytes —
	// this is a sanity sentinel, not a security boundary).
	var modelBytes [16]byte
	m := []byte(i.params.Model)
	copy(modelBytes[:], m)
	if _, err := w.Write(modelBytes[:]); err != nil {
		return err
	}
	var reserved [12]byte
	_, err := w.Write(reserved[:])
	return err
}

func (i *Index) writeNode(w io.Writer, nd *node) error {
	if err := binary.Write(w, binary.LittleEndian, nd.vertexID); err != nil {
		return err
	}
	if _, err := w.Write(nd.memoryID[:]); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint8(nd.level)); err != nil {
		return err
	}
	// Tombstone marker as a single byte (0 = live, 1 = tombstoned).
	tomb := uint8(0)
	if nd.tombstoned {
		tomb = 1
	}
	if err := binary.Write(w, binary.LittleEndian, tomb); err != nil {
		return err
	}
	for lc := 0; lc <= nd.level; lc++ {
		var arr []uint64
		if lc < len(nd.neighbors) {
			arr = nd.neighbors[lc]
		}
		if err := binary.Write(w, binary.LittleEndian, uint16(len(arr))); err != nil {
			return err
		}
		for _, n := range arr {
			if err := binary.Write(w, binary.LittleEndian, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// Load reads an index file written by Save. Returns ErrBadFile if the magic
// or version is wrong. The returned Index has no vectors loaded; callers
// must hold a VectorStore that can resolve every vertex_id at search time
// (same constraint as in-memory Add).
func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(data)

	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, ErrBadFile
	}
	if magic != fileMagic {
		return nil, fmt.Errorf("%w: magic = %q", ErrBadFile, string(magic[:]))
	}
	var ver uint8
	if err := binary.Read(r, binary.LittleEndian, &ver); err != nil {
		return nil, ErrBadFile
	}
	if ver != FileFormatVersion {
		return nil, fmt.Errorf("%w: file v%d, library v%d", ErrWrongVersion, ver, FileFormatVersion)
	}
	var (
		dim, M, M0, efC, efS uint16
		maxLevel             uint8
		entryPoint           uint64
		nodeCount            uint64
		seed                 uint64
		modelBytes           [16]byte
		reserved             [12]byte
	)
	if err := binary.Read(r, binary.LittleEndian, &dim); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &M); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &M0); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &efC); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &efS); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &maxLevel); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &entryPoint); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &nodeCount); err != nil {
		return nil, ErrBadFile
	}
	if err := binary.Read(r, binary.LittleEndian, &seed); err != nil {
		return nil, ErrBadFile
	}
	if _, err := io.ReadFull(r, modelBytes[:]); err != nil {
		return nil, ErrBadFile
	}
	if _, err := io.ReadFull(r, reserved[:]); err != nil {
		return nil, ErrBadFile
	}

	model := string(bytes.TrimRight(modelBytes[:], "\x00"))
	idx := NewIndex(Params{
		Dim:            int(dim),
		M:              int(M),
		M0:             int(M0),
		EfConstruction: int(efC),
		EfSearch:       int(efS),
		Seed:           seed,
		Model:          model,
	})
	idx.maxLevel = int(maxLevel)
	idx.entryPoint = entryPoint
	idx.hasEntry = nodeCount > 0
	for k := uint64(0); k < nodeCount; k++ {
		var nd node
		if err := binary.Read(r, binary.LittleEndian, &nd.vertexID); err != nil {
			return nil, fmt.Errorf("%w: node %d vertex: %v", ErrBadFile, k, err)
		}
		if _, err := io.ReadFull(r, nd.memoryID[:]); err != nil {
			return nil, fmt.Errorf("%w: node %d memid: %v", ErrBadFile, k, err)
		}
		var lvl uint8
		if err := binary.Read(r, binary.LittleEndian, &lvl); err != nil {
			return nil, fmt.Errorf("%w: node %d level: %v", ErrBadFile, k, err)
		}
		nd.level = int(lvl)
		var tomb uint8
		if err := binary.Read(r, binary.LittleEndian, &tomb); err != nil {
			return nil, fmt.Errorf("%w: node %d tomb: %v", ErrBadFile, k, err)
		}
		nd.tombstoned = tomb != 0
		nd.neighbors = make([][]uint64, nd.level+1)
		for lc := 0; lc <= nd.level; lc++ {
			var n uint16
			if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
				return nil, fmt.Errorf("%w: node %d L%d count: %v", ErrBadFile, k, lc, err)
			}
			arr := make([]uint64, n)
			for j := range arr {
				if err := binary.Read(r, binary.LittleEndian, &arr[j]); err != nil {
					return nil, fmt.Errorf("%w: node %d L%d edge %d: %v", ErrBadFile, k, lc, j, err)
				}
			}
			nd.neighbors[lc] = arr
		}
		idx.nodes[nd.vertexID] = &nd
		idx.byMemory[nd.memoryID] = nd.vertexID
	}
	return idx, nil
}

// --- helpers -------------------------------------------------------------

// MapStore is an in-memory VectorStore backed by a sync.Map. Useful for
// tests and as the backing store the cortex embedder hands to the index
// during rebuild. Production reads come from vec/meta via a Pebble-backed
// store implementation (cortex/embedder.go).
type MapStore struct {
	mu sync.RWMutex
	m  map[uint64][]float32
}

// NewMapStore returns an empty MapStore.
func NewMapStore() *MapStore { return &MapStore{m: map[uint64][]float32{}} }

// Put stores vec under vid.
func (s *MapStore) Put(vid uint64, vec []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]float32, len(vec))
	copy(cp, vec)
	s.m[vid] = cp
}

// GetVector implements VectorStore.
func (s *MapStore) GetVector(vid uint64) ([]float32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[vid]
	if !ok {
		return nil, false
	}
	return v, true
}

// overlayStore makes Add able to resolve the vector of the in-flight node
// before that node is fully wired into the index. It composes a primary
// store (the production VectorStore) with a single (vid, vec) overlay.
type overlayStore struct {
	primary  VectorStore
	overlay  *MapStore
	indexRef *Index
}

func newOverlayStore(i *Index, vid uint64, vec []float32) *overlayStore {
	ms := NewMapStore()
	ms.Put(vid, vec)
	return &overlayStore{primary: i.boundStore, overlay: ms, indexRef: i}
}

func (s *overlayStore) GetVector(vid uint64) ([]float32, bool) {
	if v, ok := s.overlay.GetVector(vid); ok {
		return v, true
	}
	if s.primary == nil {
		return nil, false
	}
	return s.primary.GetVector(vid)
}

// BindStore sets the VectorStore that Add will use to resolve neighbour
// vectors. The cortex embedder calls this once at startup with a
// Pebble-backed store; tests call it with a MapStore.
//
// Why not just thread the store through Add? Because Add's signature is
// fixed (vid, mid, vec) and the embedder, the rebuild loop, and the tests
// all want to call Add with no extra ceremony. The binding model also
// keeps Search's signature consistent with Add — both use the same
// resolution strategy.
func (i *Index) BindStore(s VectorStore) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.boundStore = s
}

// boundStore field appended below to keep the public method grouping
// readable.

// appendUnique appends v to s only if v is not already present. Stable;
// preserves order. Used when wiring a new reverse edge.
func appendUnique(s []uint64, v uint64) []uint64 {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- heaps ---------------------------------------------------------------

// minHeap is a container/heap.Interface ordered by distance ascending.
type minHeap []candidate

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(a, b int) bool { return h[a].dist < h[b].dist }
func (h minHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(candidate)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// maxHeap is a container/heap.Interface ordered by distance descending,
// so heap.Pop returns the furthest candidate. Used for the dynamic W list
// during HNSW search.
type maxHeap []candidate

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(a, b int) bool { return h[a].dist > h[b].dist }
func (h maxHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *maxHeap) Push(x any)        { *h = append(*h, x.(candidate)) }
func (h *maxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
