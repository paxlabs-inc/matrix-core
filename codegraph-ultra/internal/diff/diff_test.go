package diff

import (
	"testing"

	"codegraph-ultra/internal/model"
)

func makeIndex(nodes []*model.Node, edges []model.Edge) *model.Index {
	ix := model.NewIndex()
	for _, n := range nodes {
		ix.AddNode(n)
	}
	for _, e := range edges {
		ix.AddEdge(e)
	}
	return ix
}

func TestCompareIdentical(t *testing.T) {
	nodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1"},
		{ID: "b", Kind: model.KindFunc, Name: "b", Digest: "v1"},
	}
	edges := []model.Edge{
		{Src: "a", Dst: "b", Type: model.EdgeCalls},
	}

	ix := makeIndex(nodes, edges)
	d := Compare(ix, ix)

	if d.Stats.NodesAdded != 0 {
		t.Errorf("NodesAdded = %d, want 0", d.Stats.NodesAdded)
	}
	if d.Stats.NodesRemoved != 0 {
		t.Errorf("NodesRemoved = %d, want 0", d.Stats.NodesRemoved)
	}
	if d.Stats.NodesChanged != 0 {
		t.Errorf("NodesChanged = %d, want 0", d.Stats.NodesChanged)
	}
}

func TestCompareAddedNodes(t *testing.T) {
	oldNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1"},
	}
	newNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1"},
		{ID: "b", Kind: model.KindFunc, Name: "b", Digest: "v2"},
	}

	oldIX := makeIndex(oldNodes, nil)
	newIX := makeIndex(newNodes, nil)

	d := Compare(oldIX, newIX)
	if d.Stats.NodesAdded != 1 {
		t.Errorf("NodesAdded = %d, want 1", d.Stats.NodesAdded)
	}
	if len(d.AddedNodes) != 1 || d.AddedNodes[0].ID != "b" {
		t.Errorf("AddedNodes = %v, want [b]", d.AddedNodes)
	}
}

func TestCompareRemovedNodes(t *testing.T) {
	oldNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1"},
		{ID: "b", Kind: model.KindFunc, Name: "b", Digest: "v2"},
	}
	newNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1"},
	}

	oldIX := makeIndex(oldNodes, nil)
	newIX := makeIndex(newNodes, nil)

	d := Compare(oldIX, newIX)
	if d.Stats.NodesRemoved != 1 {
		t.Errorf("NodesRemoved = %d, want 1", d.Stats.NodesRemoved)
	}
}

func TestCompareChangedNodes(t *testing.T) {
	oldNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v1", Sig: "old"},
	}
	newNodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a", Digest: "v2", Sig: "new"},
	}

	oldIX := makeIndex(oldNodes, nil)
	newIX := makeIndex(newNodes, nil)

	d := Compare(oldIX, newIX)
	if d.Stats.NodesChanged != 1 {
		t.Errorf("NodesChanged = %d, want 1", d.Stats.NodesChanged)
	}
	if len(d.ChangedNodes) != 1 {
		t.Fatalf("ChangedNodes len = %d, want 1", len(d.ChangedNodes))
	}
	if d.ChangedNodes[0].ID != "a" {
		t.Errorf("ChangedNodes[0].ID = %q, want %q", d.ChangedNodes[0].ID, "a")
	}
}

func TestCompareAddedEdges(t *testing.T) {
	nodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a"},
		{ID: "b", Kind: model.KindFunc, Name: "b"},
	}

	oldIX := makeIndex(nodes, nil)
	newIX := makeIndex(nodes, []model.Edge{
		{Src: "a", Dst: "b", Type: model.EdgeCalls},
	})

	d := Compare(oldIX, newIX)
	if d.Stats.EdgesAdded != 1 {
		t.Errorf("EdgesAdded = %d, want 1", d.Stats.EdgesAdded)
	}
}

func TestCompareRemovedEdges(t *testing.T) {
	nodes := []*model.Node{
		{ID: "a", Kind: model.KindFunc, Name: "a"},
		{ID: "b", Kind: model.KindFunc, Name: "b"},
	}

	oldIX := makeIndex(nodes, []model.Edge{
		{Src: "a", Dst: "b", Type: model.EdgeCalls},
	})
	newIX := makeIndex(nodes, nil)

	d := Compare(oldIX, newIX)
	if d.Stats.EdgesRemoved != 1 {
		t.Errorf("EdgesRemoved = %d, want 1", d.Stats.EdgesRemoved)
	}
}

func TestFormatKVX(t *testing.T) {
	d := &Delta{
		AddedNodes: []*model.Node{
			{ID: "x", Kind: model.KindFunc},
		},
		Stats: DeltaStats{NodesAdded: 1},
	}

	kvx := d.FormatKVX()
	if kvx == "" {
		t.Error("FormatKVX returned empty")
	}
	if len(kvx) < 10 {
		t.Error("FormatKVX output too short")
	}
}
