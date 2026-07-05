package enrich

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"matrix/codegraph/model"
)

// structural returns a copy of n with its enrichment zeroed, so equality
// compares only the structural (source-derived) fields.
func structural(n *model.Node) model.Node {
	c := *n
	c.Enrich = model.Enrichment{}
	return c
}

// snapshotStructural records every node's structural fields by id.
func snapshotStructural(ix *model.Index) map[string]model.Node {
	m := make(map[string]model.Node)
	for _, n := range ix.Nodes() {
		m[n.Id] = structural(n)
	}
	return m
}

// assertStructuralUnchanged fails if any node's structural fields differ from
// the snapshot, or if the node set changed.
func assertStructuralUnchanged(t *testing.T, ix *model.Index, snap map[string]model.Node) {
	t.Helper()
	if got := len(ix.Nodes()); got != len(snap) {
		t.Fatalf("node count changed: %d -> %d", len(snap), got)
	}
	for _, n := range ix.Nodes() {
		before, ok := snap[n.Id]
		if !ok {
			t.Fatalf("node %s appeared during enrichment", n.Id)
		}
		if got := structural(n); got != before {
			t.Fatalf("enrichment perturbed structural fields of %s\n before=%+v\n after =%+v", n.Id, before, got)
		}
	}
}

// genGraph builds a pseudo-random but deterministic graph of n funcs in one
// package, each with a distinct digest, plus some call/reference fan-in.
func genGraph(rng *rand.Rand, n int) *model.Index {
	ix := model.NewIndex()
	ix.AddNode(&model.Node{Id: "p", Kind: model.KindPackage, Name: "p", QName: "p", Lang: "go", Digest: "b3:p"})
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("p.F%d", i)
		ids[i] = id
		ix.AddNode(&model.Node{
			Id: id, Kind: model.KindFunc, Name: fmt.Sprintf("F%d", i), QName: id, Lang: "go",
			File: "p/f.go", Range: model.Range{StartLine: i + 1, EndLine: i + 2},
			Sig: fmt.Sprintf("func F%d() int", i), Doc: fmt.Sprintf("F%d does work.", i),
			Exported: true, Digest: fmt.Sprintf("b3:f%d-v0", i),
		})
	}
	for i := 0; i < n; i++ {
		// each func calls a couple of random others
		for k := 0; k < rng.Intn(3); k++ {
			j := rng.Intn(n)
			if j != i {
				ix.AddEdge(model.Edge{Src: ids[i], Dst: ids[j], Type: model.EdgeCalls})
				ix.AddEdge(model.Edge{Src: ids[i], Dst: ids[j], Type: model.EdgeReferences})
			}
		}
	}
	return ix
}

// TestProperty_EnrichmentNonPerturbation is Property 5: across many random
// graphs and random digest mutations, enrichment never alters any structural
// field, and summaries/embeddings regenerate ONLY for nodes whose digest moved
// (Requirements 8.1, 8.2, 10.4).
func TestProperty_EnrichmentNonPerturbation(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	for iter := 0; iter < 40; iter++ {
		n := 3 + rng.Intn(20)
		ix := genGraph(rng, n)
		snap := snapshotStructural(ix)

		sum := &FakeSummarizer{}
		const dim = 32
		emb := NewHashEmbedder(dim)
		vidx := NewCortexIndex(dim, emb.Model())
		e := &Enricher{Summarizer: sum, Embedder: emb, Vectors: vidx, Batch: 1 + rng.Intn(8)}

		// Full enrichment pass.
		if _, err := e.Summarize(ctx, ix); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Embed(ctx, ix); err != nil {
			t.Fatal(err)
		}
		assertStructuralUnchanged(t, ix, snap)

		// Every enrichable node is now summarized+embedded and stamped with its digest.
		for _, node := range ix.Nodes() {
			if !Enrichable(node) {
				continue
			}
			if node.Enrich.Summary == "" || node.Enrich.SummaryDigest != node.Digest {
				t.Fatalf("iter %d: %s not summarized", iter, node.Id)
			}
			if node.Enrich.EmbedRef == "" {
				t.Fatalf("iter %d: %s not embedded", iter, node.Id)
			}
		}

		// Mutate a random subset of digests; record which ids moved.
		moved := map[string]bool{}
		for _, node := range ix.Nodes() {
			if Enrichable(node) && rng.Float64() < 0.4 {
				node.Digest = fmt.Sprintf("b3:%s-v1", node.Id)
				moved[node.Id] = true
			}
		}
		snap2 := snapshotStructural(ix)

		sum.Calls = 0
		eBefore := map[string]string{}
		for _, node := range ix.Nodes() {
			eBefore[node.Id] = node.Enrich.EmbedRef
		}

		sStat, err := e.Summarize(ctx, ix)
		if err != nil {
			t.Fatal(err)
		}
		eStat, err := e.Embed(ctx, ix)
		if err != nil {
			t.Fatal(err)
		}
		assertStructuralUnchanged(t, ix, snap2)

		// Exactly the moved nodes regenerated.
		if sStat.Generated != len(moved) {
			t.Fatalf("iter %d: summaries regenerated=%d, want %d (moved)", iter, sStat.Generated, len(moved))
		}
		if eStat.Embedded != len(moved) {
			t.Fatalf("iter %d: embeddings regenerated=%d, want %d (moved)", iter, eStat.Embedded, len(moved))
		}
		for _, node := range ix.Nodes() {
			if !Enrichable(node) {
				continue
			}
			refChanged := node.Enrich.EmbedRef != eBefore[node.Id]
			if moved[node.Id] {
				if node.Enrich.SummaryDigest != node.Digest {
					t.Fatalf("iter %d: moved %s summary not refreshed", iter, node.Id)
				}
				if !refChanged {
					t.Fatalf("iter %d: moved %s embed_ref not refreshed", iter, node.Id)
				}
			} else if refChanged {
				t.Fatalf("iter %d: unmoved %s was re-embedded (ref changed)", iter, node.Id)
			}
		}
	}
}
