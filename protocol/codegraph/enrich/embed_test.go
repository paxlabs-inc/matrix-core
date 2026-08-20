package enrich

import (
	"context"
	"testing"
)

// TestEmbed_RealCortexSubstrate runs the embed pass against the REAL cortex
// substrate — the cortex hash embedder and the cortex HNSW index — with no
// fakes, proving CodeGraph reuses that substrate rather than adding a vector
// store (Requirements 9.1, 9.2). It also proves digest-keyed caching and
// self-retrieval.
func TestEmbed_RealCortexSubstrate(t *testing.T) {
	ix := sampleGraph()
	ctx := context.Background()

	// Summaries first (embedding text = summary + sig), then embed.
	if _, err := (&Enricher{Summarizer: &FakeSummarizer{}}).Summarize(ctx, ix); err != nil {
		t.Fatal(err)
	}

	const dim = 64
	emb := NewHashEmbedder(dim)
	vidx := NewCortexIndex(dim, emb.Model())
	e := &Enricher{Embedder: emb, Vectors: vidx}

	st, err := e.Embed(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Eligible != 3 || st.Embedded != 3 || st.Reused != 0 {
		t.Fatalf("first embed stats = %+v, want eligible=3 embedded=3 reused=0", st)
	}
	if vidx.Len() != 3 {
		t.Fatalf("index holds %d vectors, want 3", vidx.Len())
	}
	for _, id := range enrichableIDs() {
		n := ix.Node(id)
		if n.Enrich.EmbedRef != Namespace+":"+id+"@"+n.Digest {
			t.Fatalf("%s embed_ref = %q", id, n.Enrich.EmbedRef)
		}
	}

	// Graph salience: p.A (called+referenced by B and C) outranks the leaves.
	if a, b := ix.Node("p.A").Enrich.Salience, ix.Node("p.B").Enrich.Salience; a <= b {
		t.Fatalf("p.A salience %f should exceed p.B salience %f", a, b)
	}
	if ix.Node("p.C").Enrich.Salience != 0 {
		t.Fatalf("p.C (no fan-in) salience should be 0, got %f", ix.Node("p.C").Enrich.Salience)
	}

	// Self-retrieval: embedding p.B's own text finds p.B as the top hit.
	q, err := emb.Embed(ctx, []string{embedText(ix.Node("p.B"))})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := vidx.Search(q[0], 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Id != "p.B" {
		t.Fatalf("search for p.B returned %+v, want p.B first", hits)
	}

	// Second pass: unchanged digests ⇒ all reused, no re-embedding.
	st, err = e.Embed(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Embedded != 0 || st.Reused != 3 {
		t.Fatalf("no-change embed stats = %+v, want embedded=0 reused=3", st)
	}

	// Move p.A's digest and re-summarize+embed: only p.A re-embeds; index still
	// holds exactly 3 live vectors (replace, not duplicate).
	ix.Node("p.A").Digest = "b3:a2"
	if _, err := (&Enricher{Summarizer: &FakeSummarizer{}}).Summarize(ctx, ix); err != nil {
		t.Fatal(err)
	}
	st, err = e.Embed(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Embedded != 1 || st.Reused != 2 {
		t.Fatalf("post-edit embed stats = %+v, want embedded=1 reused=2", st)
	}
	if vidx.Len() != 3 {
		t.Fatalf("index holds %d live vectors after replace, want 3", vidx.Len())
	}
}

func TestEmbed_MissingDeps(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Enricher{Vectors: NewCortexIndex(8, "m")}).Embed(ctx, sampleGraph()); err == nil {
		t.Fatal("expected error with no embedder")
	}
	if _, err := (&Enricher{Embedder: NewHashEmbedder(8)}).Embed(ctx, sampleGraph()); err == nil {
		t.Fatal("expected error with no vector index")
	}
}
