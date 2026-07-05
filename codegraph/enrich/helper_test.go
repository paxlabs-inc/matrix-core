package enrich

import "matrix/codegraph/model"

// sampleGraph builds a small real Index: package p with funcs A, B, C where B
// and C both call A (and B references A), so A has graph salience and B/C do
// not. Digests are fixed so staleness is controllable in tests.
func sampleGraph() *model.Index {
	ix := model.NewIndex()
	ix.AddNode(&model.Node{Id: "p", Kind: model.KindPackage, Name: "p", QName: "p", Lang: "go", Digest: "b3:p"})
	ix.AddNode(&model.Node{
		Id: "p.A", Kind: model.KindFunc, Name: "A", QName: "p.A", Lang: "go",
		File: "p/a.go", Range: model.Range{StartLine: 1, EndLine: 2},
		Sig: "func A() int", Doc: "A does alpha.", Exported: true, Digest: "b3:a1",
	})
	ix.AddNode(&model.Node{
		Id: "p.B", Kind: model.KindFunc, Name: "B", QName: "p.B", Lang: "go",
		File: "p/b.go", Range: model.Range{StartLine: 1, EndLine: 2},
		Sig: "func B() int", Doc: "B does beta.", Exported: true, Digest: "b3:b1",
	})
	ix.AddNode(&model.Node{
		Id: "p.C", Kind: model.KindFunc, Name: "C", QName: "p.C", Lang: "go",
		File: "p/c.go", Range: model.Range{StartLine: 1, EndLine: 2},
		Sig: "func C() int", Doc: "C does gamma.", Exported: true, Digest: "b3:c1",
	})
	ix.AddEdge(model.Edge{Src: "p.B", Dst: "p.A", Type: model.EdgeCalls})
	ix.AddEdge(model.Edge{Src: "p.C", Dst: "p.A", Type: model.EdgeCalls})
	ix.AddEdge(model.Edge{Src: "p.B", Dst: "p.A", Type: model.EdgeReferences})
	return ix
}

// enrichableIDs returns the ids the enrichment layer should touch, in sampleGraph.
func enrichableIDs() []string { return []string{"p.A", "p.B", "p.C"} }
