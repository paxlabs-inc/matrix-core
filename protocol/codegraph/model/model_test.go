package model

import (
	"reflect"
	"testing"
)

func TestIndex_AddEdgeDedupAndBidirectional(t *testing.T) {
	ix := NewIndex()
	ix.AddEdge(Edge{Src: "a", Dst: "b", Type: EdgeCalls, Site: "f.go:1"})
	ix.AddEdge(Edge{Src: "a", Dst: "b", Type: EdgeCalls, Site: "f.go:9"}) // duplicate collapses
	ix.AddEdge(Edge{Src: "a", Dst: "c", Type: EdgeCalls})

	if got := ix.Forward("a", EdgeCalls); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("forward = %v, want [b c]", got)
	}
	if got := ix.Reverse("b", EdgeCalls); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("reverse = %v, want [a]", got)
	}
	if site := ix.Site("a", EdgeCalls, "b"); site != "f.go:1" {
		t.Fatalf("site = %q, want first-retained f.go:1", site)
	}
	if edges := ix.Edges(); len(edges) != 2 {
		t.Fatalf("dedup failed: %d edges, want 2", len(edges))
	}
}

func TestIndex_EdgesSortedBySrcTypeDst(t *testing.T) {
	ix := NewIndex()
	ix.AddEdge(Edge{Src: "z", Dst: "a", Type: EdgeCalls})
	ix.AddEdge(Edge{Src: "a", Dst: "y", Type: EdgeReferences})
	ix.AddEdge(Edge{Src: "a", Dst: "x", Type: EdgeReferences})
	ix.AddEdge(Edge{Src: "a", Dst: "b", Type: EdgeCalls})

	got := ix.Edges()
	want := []Edge{
		{Src: "a", Dst: "b", Type: EdgeCalls},
		{Src: "a", Dst: "x", Type: EdgeReferences},
		{Src: "a", Dst: "y", Type: EdgeReferences},
		{Src: "z", Dst: "a", Type: EdgeCalls},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges not sorted by (src,type,dst):\n got %v\nwant %v", got, want)
	}
}

func TestIndex_NodesSortedById(t *testing.T) {
	ix := NewIndex()
	ix.AddNode(&Node{Id: "b"})
	ix.AddNode(&Node{Id: "a"})
	ix.AddNode(&Node{Id: "c"})
	got := ix.Nodes()
	if len(got) != 3 || got[0].Id != "a" || got[1].Id != "b" || got[2].Id != "c" {
		t.Fatalf("nodes not sorted by id: %v", []string{got[0].Id, got[1].Id, got[2].Id})
	}
}

func TestIndex_NeighborsDirectionAndTypeFilter(t *testing.T) {
	ix := NewIndex()
	ix.AddEdge(Edge{Src: "a", Dst: "b", Type: EdgeCalls})
	ix.AddEdge(Edge{Src: "a", Dst: "c", Type: EdgeReferences})
	ix.AddEdge(Edge{Src: "d", Dst: "a", Type: EdgeCalls})

	if got := ix.Neighbors("a", EdgeCalls, Out); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("out/calls = %v, want [b]", got)
	}
	if got := ix.Neighbors("a", "", Out); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("out/* = %v, want [b c]", got)
	}
	if got := ix.Neighbors("a", EdgeCalls, In); !reflect.DeepEqual(got, []string{"d"}) {
		t.Fatalf("in/calls = %v, want [d]", got)
	}
	if got := ix.Neighbors("a", "", Both); !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Fatalf("both/* = %v, want [b c d]", got)
	}
}
