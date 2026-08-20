package enrich

import (
	"context"
	"sort"

	"lukechampine.com/blake3"

	"centra/core/cortex/embed"
	"centra/core/cortex/vector"
)

// This file wires CodeGraph's enrichment to the EXISTING cortex substrate:
// core/cortex/embed for text→vector and core/cortex/vector (the pure-Go HNSW) for the
// semantic index. CodeGraph adds NO new vector store (Requirement 9.1); it runs
// its own cortex HNSW index under the logical namespace "codegraph".

// CortexEmbedder adapts a cortex embed.Embedder (single-text) to the batch
// enrich.Embedder boundary. Use WrapEmbedder for a live model, or
// NewHashEmbedder for the deterministic cortex hash embedder in tests — both are
// REAL cortex embedders, not fakes.
type CortexEmbedder struct{ inner embed.Embedder }

// WrapEmbedder adapts any cortex embed.Embedder into the enrich.Embedder batch
// boundary.
func WrapEmbedder(e embed.Embedder) *CortexEmbedder { return &CortexEmbedder{inner: e} }

// NewHashEmbedder returns the deterministic cortex hash embedder at the given
// dimension, salted for codegraph, adapted to the batch boundary. It is a real
// embedder with the replay-deterministic contract — suitable for hermetic tests
// and offline indexing.
func NewHashEmbedder(dim int) *CortexEmbedder {
	return WrapEmbedder(embed.NewHashEmbedderWith(dim, "codegraph"))
}

func (a *CortexEmbedder) Dim() int { return a.inner.Dim() }

func (a *CortexEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := a.inner.Embed(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Model reports the underlying cortex embedder's model id, for the index pin.
func (a *CortexEmbedder) Model() string { return a.inner.Model() }

// salienceGain scales how strongly graph salience boosts a semantic hit's
// score. Small and monotonic: it re-ranks near-ties toward higher-salience
// nodes (Requirement 7.2) without overriding clear semantic proximity.
const salienceGain = 0.25

// CortexIndex is a VectorIndex backed by a cortex HNSW index (the "codegraph"
// namespace). It maps CodeGraph string node ids onto the index's uint64 vertex
// ids and [16]byte memory ids, retains per-node graph salience, and blends
// salience into search scores so retrieval is weighted by the cortex-style
// salience signal (Requirement 9.2).
type CortexIndex struct {
	idx   *vector.Index
	store *vector.MapStore
	next  uint64
	byVid map[uint64]string
	byId  map[string]uint64
	sal   map[string]float64
}

// NewCortexIndex builds an in-memory cortex HNSW index of the given dimension
// under the codegraph namespace. model pins the embedding model id to the index.
func NewCortexIndex(dim int, model string) *CortexIndex {
	store := vector.NewMapStore()
	idx := vector.NewIndex(vector.Params{Dim: dim, Model: model})
	idx.BindStore(store)
	return &CortexIndex{
		idx:   idx,
		store: store,
		byVid: map[uint64]string{},
		byId:  map[string]uint64{},
		sal:   map[string]float64{},
	}
}

// midFor derives the stable 16-byte cortex MemoryID for a node id.
func midFor(id string) vector.MemoryID {
	var mid vector.MemoryID
	sum := blake3.Sum256([]byte("codegraph:" + id))
	copy(mid[:], sum[:16])
	return mid
}

// Add indexes vec under id with a graph-salience weight. Re-adding an id
// tombstones the prior vector and inserts the new one, so an incremental
// re-embed of a moved node replaces its entry rather than duplicating it.
func (c *CortexIndex) Add(id string, vec []float32, salience float64) error {
	mid := midFor(id)
	if _, ok := c.byId[id]; ok {
		c.idx.Tombstone(mid)
	}
	vid := c.next
	c.next++
	c.store.Put(vid, vec)
	if err := c.idx.Add(vid, mid, vec); err != nil {
		return err
	}
	c.byVid[vid] = id
	c.byId[id] = vid
	c.sal[id] = salience
	return nil
}

// Search returns up to k salience-weighted nearest neighbors of vec, best
// first. Score = cosineSimilarity * (1 + salienceGain*salience), so among
// semantically comparable hits the higher-salience node ranks ahead.
func (c *CortexIndex) Search(vec []float32, k int) ([]Match, error) {
	hits, err := c.idx.Search(vec, k)
	if err != nil {
		if err == vector.ErrEmptyIndex {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Match, 0, len(hits))
	for _, h := range hits {
		id, ok := c.byVid[h.VertexID]
		if !ok {
			continue
		}
		sim := 1 - float64(h.Distance)/2 // unit-norm cosine in [0,1]
		score := sim * (1 + salienceGain*c.sal[id])
		out = append(out, Match{Id: id, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Id < out[j].Id
	})
	return out, nil
}

// Len reports how many live vectors are indexed.
func (c *CortexIndex) Len() int { return c.idx.Len() }
