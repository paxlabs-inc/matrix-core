package store

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"centra/protocol/codegraph/model"
)

const goldenPath = "testdata/cortex.kvx"

func TestGolden_RoundTripByteIdentical(t *testing.T) {
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	ix, merkle, err := Parse(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if merkle != "b3:00aa11bb22cc33dd" {
		t.Fatalf("banner merkle = %q, want b3:00aa11bb22cc33dd", merkle)
	}
	var got bytes.Buffer
	if err := Write(&got, ix, merkle); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("round-trip not byte-identical.\n--- got ---\n%s\n--- want ---\n%s", got.String(), want)
	}
}

func TestGolden_ParsedShape(t *testing.T) {
	f, err := os.Open(goldenPath)
	if err != nil {
		t.Fatalf("open golden: %v", err)
	}
	defer f.Close()
	ix, _, err := Parse(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ix.Len() != 6 {
		t.Fatalf("nodes = %d, want 6", ix.Len())
	}
	snap := ix.Node("centra/core/cortex.Snapshot")
	if snap == nil {
		t.Fatal("missing Snapshot node")
	}
	if snap.Kind != model.KindType || snap.Range != (model.Range{StartLine: 142, EndLine: 210}) {
		t.Fatalf("Snapshot fields wrong: %+v", snap)
	}
	if snap.Enrich.Summary != "Merkle-rooted point-in-time cortex state." || snap.Enrich.Salience != 0.71 {
		t.Fatalf("Snapshot enrichment wrong: %+v", snap.Enrich)
	}
	// Forward implements + reconstructed reverse edges (source lives in-shard).
	if got := ix.Forward("centra/core/cortex.Snapshot", model.EdgeImplements); len(got) != 1 || got[0] != "centra/core/cortex.Persistable" {
		t.Fatalf("Snapshot implements = %v", got)
	}
	if got := ix.Reverse("centra/core/cortex.Snapshot", model.EdgeReferences); len(got) != 1 || got[0] != "centra/core/cortex.NewStore" {
		t.Fatalf("Snapshot <-references = %v", got)
	}
}

// Determinism independent of the golden: build an index with edges and nodes
// added in scrambled order, and assert Write sorts nodes by id and edges by
// (src,type,dst) with types in canonical order (req 1.2 / 5.2).
func TestWrite_DeterministicOrdering(t *testing.T) {
	ix := model.NewIndex()
	ix.AddNode(&model.Node{Id: "p/z", Kind: model.KindFunc, File: "z.go", Range: model.Range{StartLine: 1, EndLine: 2}, Lang: "go", Digest: "b3:z"})
	ix.AddNode(&model.Node{Id: "p/a", Kind: model.KindFunc, File: "a.go", Range: model.Range{StartLine: 1, EndLine: 2}, Lang: "go", Digest: "b3:a"})
	// scramble edge insert order and edge types
	ix.AddEdge(model.Edge{Src: "p/a", Dst: "p/z", Type: model.EdgeReferences})
	ix.AddEdge(model.Edge{Src: "p/a", Dst: "p/b", Type: model.EdgeCalls})
	ix.AddEdge(model.Edge{Src: "p/a", Dst: "p/a2", Type: model.EdgeCalls})

	var buf bytes.Buffer
	if err := Write(&buf, ix, "b3:root"); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if idxA, idxZ := strings.Index(out, "NODE id=p/a"), strings.Index(out, "NODE id=p/z"); !(idxA >= 0 && idxA < idxZ) {
		t.Fatalf("nodes not sorted by id:\n%s", out)
	}
	// within p/a: calls (sorted p/a2,p/b) precedes references, per EdgeTypes order
	wantBlock := "EDGES\n  calls=p/a2,p/b\n  references=p/z\n"
	if !strings.Contains(out, wantBlock) {
		t.Fatalf("edges not in canonical/sorted order; got:\n%s", out)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	for _, s := range []string{"a\nb", "c\\d", "e\r\nf", "plain", `tab\ttail`} {
		if got := unescape(escape(s)); got != s {
			t.Fatalf("escape round-trip %q -> %q", s, got)
		}
	}
}
