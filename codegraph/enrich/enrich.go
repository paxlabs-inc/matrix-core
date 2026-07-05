// Package enrich is CodeGraph's L5 enrichment boundary. Every LLM/embedding
// call sits behind an interface here, so the structural layers (extraction,
// model, serialization) stay a pure function of source and never depend on a
// model call. Enrichment writes touch ONLY a Node's embedded Enrichment struct
// — never its id, digest, or any structural field — so summaries and embeddings
// are a quarantined, regenerable layer that can be distrusted and rebuilt
// without perturbing the graph (Requirement 8.1).
package enrich

import (
	"context"

	"matrix/codegraph/model"
)

// Request is the structural view of one node handed across the L5 boundary. It
// carries only fields already public in the graph store (never raw source), so
// the boundary cannot leak source text into a summary prompt.
type Request struct {
	Id    string
	Kind  model.Kind
	Name  string
	QName string
	Lang  string
	Sig   string
	Doc   string
}

// Summarizer produces a one-line natural-language summary for each request, in
// order. It is the sole place structural facts become prose; a real
// implementation batches through the cheap model tier (Cody via Gateway), a
// fake returns deterministic text for tests. len(out) MUST equal len(reqs).
type Summarizer interface {
	Summarize(ctx context.Context, reqs []Request) ([]string, error)
}

// Embedder maps texts to fixed-width vectors, in order. It is the sole
// embedding boundary; a real implementation calls the cortex embedder, a fake
// derives a deterministic vector. len(out) MUST equal len(texts) and every
// vector MUST have length Dim().
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// VectorIndex is the semantic-search sink: it indexes an embedding under a node
// id within a namespace and answers k-NN queries. CodeGraph targets the
// existing cortex HNSW under namespace "codegraph" rather than a new vector
// store (Requirement 9.1); the interface keeps that dependency behind L5 and
// lets tests substitute an in-memory index.
type VectorIndex interface {
	// Add indexes vec under id with an associated salience weight (see
	// Requirement 9.2). Re-adding the same id replaces the prior entry.
	Add(id string, vec []float32, salience float64) error
	// Search returns up to k node ids nearest to vec, best first, each with its
	// fused score after salience weighting.
	Search(vec []float32, k int) ([]Match, error)
}

// Match is one ranked vector-search hit.
type Match struct {
	Id    string
	Score float64
}

// Namespace is the cortex HNSW namespace CodeGraph embeddings live under.
const Namespace = "codegraph"

// enrichableKinds are the symbol kinds worth summarizing and embedding. Whole-
// file and container nodes (repo/module/package/file) are structural aggregates
// whose digest already rolls up their children; per-symbol enrichment is what an
// agent retrieves against.
var enrichableKinds = map[model.Kind]bool{
	model.KindFunc:      true,
	model.KindMethod:    true,
	model.KindType:      true,
	model.KindInterface: true,
	model.KindConst:     true,
	model.KindVar:       true,
	model.KindField:     true,
}

// Enrichable reports whether a node participates in the enrichment layer.
func Enrichable(n *model.Node) bool { return enrichableKinds[n.Kind] }

// requestFor builds the boundary Request from a node's structural fields.
func requestFor(n *model.Node) Request {
	return Request{
		Id: n.Id, Kind: n.Kind, Name: n.Name, QName: n.QName,
		Lang: n.Lang, Sig: n.Sig, Doc: n.Doc,
	}
}

// setSummary writes a generated summary and stamps it with the digest it was
// derived from — the ONLY mutation path for summary fields. It touches nothing
// structural, upholding the quarantine invariant (Requirement 8.1).
func setSummary(n *model.Node, summary, digest string) {
	n.Enrich.Summary = summary
	n.Enrich.SummaryDigest = digest
}

// setEmbedding writes the embed reference and salience — the ONLY mutation path
// for embedding fields. It too touches nothing structural.
func setEmbedding(n *model.Node, embedRef string, salience float64) {
	n.Enrich.EmbedRef = embedRef
	n.Enrich.Salience = salience
}

// Enricher runs the enrichment passes over an index behind the L5 boundary.
// Summarizer and Embedder are required for their respective passes; Vectors is
// the index the Embed pass writes into. Batch bounds how many nodes go to the
// model per call (0 => defaultBatch).
type Enricher struct {
	Summarizer Summarizer
	Embedder   Embedder
	Vectors    VectorIndex
	Batch      int
}

const defaultBatch = 64

func (e *Enricher) batchSize() int {
	if e.Batch > 0 {
		return e.Batch
	}
	return defaultBatch
}
