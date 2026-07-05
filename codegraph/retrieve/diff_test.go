package retrieve

import (
	"strings"
	"testing"

	"matrix/codegraph/model"
)

func node(id string, kind model.Kind, digest string) *model.Node {
	return &model.Node{Id: id, Kind: kind, Digest: digest}
}

func TestDiff_NodeAndEdgeDeltas(t *testing.T) {
	a := model.NewIndex()
	a.AddNode(node("p.A", model.KindFunc, "b3:aaa"))
	a.AddNode(node("p.B", model.KindFunc, "b3:bbb"))
	a.AddNode(node("p.Gone", model.KindType, "b3:ggg"))
	a.AddEdge(model.Edge{Src: "p.A", Dst: "p.B", Type: model.EdgeCalls})
	a.AddEdge(model.Edge{Src: "p.A", Dst: "p.Gone", Type: model.EdgeReferences})

	b := model.NewIndex()
	b.AddNode(node("p.A", model.KindFunc, "b3:aaa"))                       // unchanged
	b.AddNode(node("p.B", model.KindFunc, "b3:bbb2"))                      // digest changed
	b.AddNode(node("p.New", model.KindFunc, "b3:nnn"))                     // added
	b.AddEdge(model.Edge{Src: "p.A", Dst: "p.New", Type: model.EdgeCalls}) // edge added
	b.AddEdge(model.Edge{Src: "p.A", Dst: "p.B", Type: model.EdgeCalls})   // edge kept
	// p.A->p.Gone references edge dropped (p.Gone removed)

	frag := Diff(a, b)

	for _, want := range []string{
		"nodes_added=1", "nodes_removed=1", "nodes_changed=1",
		"edges_added=1", "edges_removed=1",
	} {
		if !strings.Contains(frag, want) {
			t.Fatalf("header missing %q:\n%s", want, frag)
		}
	}
	assertBlock(t, frag, "NODE id=p.New kind=func", "  change=added")
	assertBlock(t, frag, "NODE id=p.Gone kind=type", "  change=removed")
	assertBlock(t, frag, "NODE id=p.B kind=func", "  change=changed")
	if !strings.Contains(frag, "  digest_old=b3:bbb") || !strings.Contains(frag, "  digest_new=b3:bbb2") {
		t.Fatalf("changed node missing digest transition:\n%s", frag)
	}
	if !strings.Contains(frag, "  +calls=p.New") {
		t.Fatalf("missing added edge:\n%s", frag)
	}
	if !strings.Contains(frag, "  -references=p.Gone") {
		t.Fatalf("missing removed edge:\n%s", frag)
	}
}

func TestDiff_IdenticalGraphsEmptyDelta(t *testing.T) {
	a := model.NewIndex()
	a.AddNode(node("p.A", model.KindFunc, "b3:aaa"))
	a.AddEdge(model.Edge{Src: "p.A", Dst: "p.A", Type: model.EdgeReferences})
	b := model.NewIndex()
	b.AddNode(node("p.A", model.KindFunc, "b3:aaa"))
	b.AddEdge(model.Edge{Src: "p.A", Dst: "p.A", Type: model.EdgeReferences})

	frag := Diff(a, b)
	for _, want := range []string{"nodes_added=0", "nodes_removed=0", "nodes_changed=0", "edges_added=0", "edges_removed=0"} {
		if !strings.Contains(frag, want) {
			t.Fatalf("identical graphs not empty (%q):\n%s", want, frag)
		}
	}
}

func assertBlock(t *testing.T, frag, header, line string) {
	t.Helper()
	idx := strings.Index(frag, header)
	if idx < 0 {
		t.Fatalf("missing %q:\n%s", header, frag)
	}
	if !strings.Contains(frag[idx:], line) {
		t.Fatalf("%q not found under %q:\n%s", line, header, frag)
	}
}
