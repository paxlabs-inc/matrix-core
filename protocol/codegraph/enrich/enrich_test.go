package enrich

import (
	"context"
	"testing"

	"centra/protocol/codegraph/model"
)

// interface conformance — the fakes ARE Summarizer/Embedder.
var (
	_ Summarizer = (*FakeSummarizer)(nil)
	_ Embedder   = (*FakeEmbedder)(nil)
)

func sampleNode() *model.Node {
	return &model.Node{
		Id: "prog/a.Add", Kind: model.KindFunc, Name: "Add", QName: "prog/a.Add",
		Lang: "go", File: "a/a.go", Range: model.Range{StartLine: 3, EndLine: 3},
		ByteRange: model.ByteRange{StartByte: 10, EndByte: 40},
		Digest:    "b3:deadbeef", Sig: "func Add(x int) int", Exported: true,
		Doc: "Add adds one.",
	}
}

// TestBoundary_WritesOnlyEnrichment proves the L5 mutation helpers touch nothing
// structural: id, digest, and every other structural field survive byte-for-byte
// (Requirement 8.1).
func TestBoundary_WritesOnlyEnrichment(t *testing.T) {
	n := sampleNode()
	before := *n // value copy of all structural + enrichment fields

	setSummary(n, "adds one to x", n.Digest)
	setEmbedding(n, "codegraph/prog/a.Add", 0.75)

	// Structural fields untouched.
	got := *n
	got.Enrich = before.Enrich // ignore enrichment when comparing structure
	if got != before {
		t.Fatalf("structural fields mutated by enrichment write\n got=%+v\nwant=%+v", got, before)
	}
	// Enrichment fields set.
	if n.Enrich.Summary != "adds one to x" || n.Enrich.SummaryDigest != n.Digest {
		t.Fatalf("summary not written: %+v", n.Enrich)
	}
	if n.Enrich.EmbedRef != "codegraph/prog/a.Add" || n.Enrich.Salience != 0.75 {
		t.Fatalf("embedding not written: %+v", n.Enrich)
	}
}

func TestFakeSummarizer_DeterministicAndDistinct(t *testing.T) {
	f := &FakeSummarizer{}
	reqs := []Request{
		requestFor(sampleNode()),
		{Id: "prog/a.Sub", Kind: model.KindFunc, Name: "Sub", Sig: "func Sub(x int) int"},
	}
	a, err := f.Summarize(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Summarize(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a[0] == "" || a[1] == "" {
		t.Fatalf("bad summaries: %v", a)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("summary %d not deterministic: %q vs %q", i, a[i], b[i])
		}
	}
	if a[0] == a[1] {
		t.Fatalf("distinct inputs collapsed to same summary: %q", a[0])
	}
}

func TestFakeEmbedder_DeterministicUnitVectors(t *testing.T) {
	e := &FakeEmbedder{Dimension: 16}
	texts := []string{"func Add(x int) int", "func Sub(x int) int"}
	v1, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range v1 {
		if len(v1[i]) != 16 {
			t.Fatalf("vector %d width = %d, want 16", i, len(v1[i]))
		}
		var norm float64
		for j := range v1[i] {
			if v1[i][j] != v2[i][j] {
				t.Fatalf("embedding %d not deterministic at dim %d", i, j)
			}
			norm += float64(v1[i][j]) * float64(v1[i][j])
		}
		if norm < 0.99 || norm > 1.01 {
			t.Fatalf("vector %d not unit-normalized: |v|^2=%f", i, norm)
		}
	}
	if vectorsEqual(v1[0], v1[1]) {
		t.Fatal("distinct texts produced identical vectors")
	}
}

func TestEnrichable(t *testing.T) {
	cases := map[model.Kind]bool{
		model.KindFunc: true, model.KindMethod: true, model.KindType: true,
		model.KindInterface: true, model.KindConst: true, model.KindVar: true,
		model.KindField: true,
		model.KindRepo:  false, model.KindModule: false,
		model.KindPackage: false, model.KindFile: false,
	}
	for k, want := range cases {
		if got := Enrichable(&model.Node{Kind: k}); got != want {
			t.Errorf("Enrichable(%s) = %v, want %v", k, got, want)
		}
	}
}

func vectorsEqual(a, b []float32) bool {
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
