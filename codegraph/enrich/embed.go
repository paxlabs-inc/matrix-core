package enrich

import (
	"context"
	"strings"

	"matrix/codegraph/model"
)

// EmbedStats reports what an Embed pass did.
type EmbedStats struct {
	Eligible int // enrichable nodes considered
	Embedded int // nodes (re-)embedded and indexed this pass (digest was stale)
	Reused   int // nodes whose cached embedding was still valid (digest unchanged)
}

// embedStale reports whether a node needs a fresh embedding: its recorded
// embed reference does not match the reference its current digest would
// produce. Because the reference is digest-stamped (see embedRef), this is true
// exactly when the node has never been embedded or its content moved — so the
// pass re-embeds only nodes whose digest changed (Requirements 8.2, 10.4).
func embedStale(n *model.Node) bool {
	return n.Enrich.EmbedRef != embedRef(n.Id, n.Digest)
}

// embedText is what gets embedded for a node: its one-line summary plus its
// signature. Both are compact, already-public graph fields — never raw source.
func embedText(n *model.Node) string {
	var b strings.Builder
	if s := n.Enrich.Summary; s != "" {
		b.WriteString(s)
	}
	if n.Sig != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(n.Sig)
	}
	if b.Len() == 0 {
		b.WriteString(n.Name)
	}
	return b.String()
}

// embedRef is the digest-stamped reference recorded on a node, pointing at its
// entry in the codegraph vector namespace. Stamping the digest makes the
// reference change exactly when the embedded content moves, so a stale
// embedding is detectable by comparison alone (see embedStale).
func embedRef(id, digest string) string { return Namespace + ":" + id + "@" + digest }

// Embed embeds (summary + signature) for every enrichable node whose embedding
// is stale, indexes each vector in the cortex HNSW under namespace codegraph
// weighted by graph salience, and reuses cached embeddings for unchanged
// digests. It refreshes every enrichable node's graph salience (EMA-blended)
// each pass, since salience tracks graph structure independently of a node's
// own digest. It writes only Enrich.EmbedRef and Enrich.Salience — never a
// structural field (Requirements 8.1, 9.1, 9.2, 10.4).
//
// Nodes are processed in id-sorted, batched order so the pass is deterministic.
func (e *Enricher) Embed(ctx context.Context, ix *model.Index) (EmbedStats, error) {
	if e.Embedder == nil {
		return EmbedStats{}, errNoEmbedder
	}
	if e.Vectors == nil {
		return EmbedStats{}, errNoVectors
	}
	dim := e.Embedder.Dim()
	salience := computeSalience(ix)

	// Refresh salience on every enrichable node first (EMA), so both reused and
	// re-embedded nodes carry a current weight.
	for _, n := range ix.Nodes() {
		if !Enrichable(n) {
			continue
		}
		n.Enrich.Salience = blendSalience(n.Enrich.Salience, salience[n.Id])
	}

	var stats EmbedStats
	var batch []*model.Node
	batchN := e.batchSize()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		texts := make([]string, len(batch))
		for i, n := range batch {
			texts[i] = embedText(n)
		}
		vecs, err := e.Embedder.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if len(vecs) != len(batch) {
			return errEmbedCount
		}
		for i, n := range batch {
			if len(vecs[i]) != dim {
				return errEmbedDim
			}
			if err := e.Vectors.Add(n.Id, vecs[i], n.Enrich.Salience); err != nil {
				return err
			}
			setEmbedding(n, embedRef(n.Id, n.Digest), n.Enrich.Salience)
			stats.Embedded++
		}
		batch = batch[:0]
		return nil
	}

	for _, n := range ix.Nodes() {
		if !Enrichable(n) {
			continue
		}
		stats.Eligible++
		if !embedStale(n) {
			stats.Reused++
			continue
		}
		batch = append(batch, n)
		if len(batch) >= batchN {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
}
