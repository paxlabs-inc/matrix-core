package store

import (
	"os"
	"path/filepath"
	"testing"

	"centra/protocol/codegraph-ultra/internal/model"
)

func setupTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, func() { db.Close(); os.Remove(dbPath) }
}

func TestOpenAndClose(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	if db.Path() == "" {
		t.Error("Path() returned empty string")
	}
}

func TestUpsertAndGetNode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	n := &model.Node{
		ID:   "pkg.Foo",
		Kind: model.KindFunc,
		Name: "Foo",
		QName: "pkg.Foo",
		Lang: "go",
		File: "foo.go",
		Range: model.Range{StartLine: 10, EndLine: 20},
		Digest: "sha256:abc123",
		Sig:   "func Foo() error",
		Exported: true,
		Doc:  "Foo does foo things",
	}

	if err := db.UpsertNode(n); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	got := db.GetNode("pkg.Foo")
	if got == nil {
		t.Fatal("GetNode returned nil")
	}
	if got.ID != "pkg.Foo" {
		t.Errorf("ID = %q, want %q", got.ID, "pkg.Foo")
	}
	if got.Kind != model.KindFunc {
		t.Errorf("Kind = %q, want %q", got.Kind, model.KindFunc)
	}
	if got.Sig != "func Foo() error" {
		t.Errorf("Sig = %q, want %q", got.Sig, "func Foo() error")
	}
	if !got.Exported {
		t.Error("Exported = false, want true")
	}
}

func TestUpsertNodeUpdate(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	n1 := &model.Node{ID: "a.B", Kind: model.KindFunc, Name: "B", Digest: "v1"}
	db.UpsertNode(n1)

	n2 := &model.Node{ID: "a.B", Kind: model.KindFunc, Name: "B", Digest: "v2", Doc: "updated"}
	db.UpsertNode(n2)

	got := db.GetNode("a.B")
	if got.Digest != "v2" {
		t.Errorf("Digest = %q, want %q", got.Digest, "v2")
	}
	if got.Doc != "updated" {
		t.Errorf("Doc = %q, want %q", got.Doc, "updated")
	}
}

func TestGetNodeNotFound(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	got := db.GetNode("nonexistent")
	if got != nil {
		t.Error("GetNode should return nil for nonexistent node")
	}
}

func TestUpsertAndGetEdge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})

	e := model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls, Site: "a.go:10"}
	if err := db.UpsertEdge(e); err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}

	fwd := db.Forward("a", model.EdgeCalls)
	if len(fwd) != 1 || fwd[0] != "b" {
		t.Errorf("Forward = %v, want [b]", fwd)
	}

	rev := db.Reverse("b", model.EdgeCalls)
	if len(rev) != 1 || rev[0] != "a" {
		t.Errorf("Reverse = %v, want [a]", rev)
	}
}

func TestForwardAll(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertNode(&model.Node{ID: "c", Kind: model.KindFunc, Name: "c"})

	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "c", Type: model.EdgeReferences})

	all := db.ForwardAll("a")
	if len(all) != 2 {
		t.Errorf("ForwardAll = %v, want 2 entries", all)
	}
}

func TestSearch(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "pkg.Handler", Kind: model.KindFunc, Name: "Handler", Doc: "handles HTTP requests"})
	db.UpsertNode(&model.Node{ID: "pkg.Server", Kind: model.KindType, Name: "Server", Doc: "the main server"})

	results := db.Search("Handler", 10)
	if len(results) != 1 {
		t.Fatalf("Search = %d results, want 1", len(results))
	}
	if results[0].ID != "pkg.Handler" {
		t.Errorf("Search result ID = %q, want %q", results[0].ID, "pkg.Handler")
	}
}

func TestStats(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a", Lang: "go", File: "a.go"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindType, Name: "b", Lang: "go", File: "b.go"})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})

	st := db.Stats()
	if st.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", st.TotalNodes)
	}
	if st.TotalEdges != 1 {
		t.Errorf("TotalEdges = %d, want 1", st.TotalEdges)
	}
	if st.NodesByKind[model.KindFunc] != 1 {
		t.Errorf("NodesByKind[func] = %d, want 1", st.NodesByKind[model.KindFunc])
	}
}

func TestClear(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})

	if err := db.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	st := db.Stats()
	if st.TotalNodes != 0 {
		t.Errorf("After clear: TotalNodes = %d", st.TotalNodes)
	}
	if st.TotalEdges != 0 {
		t.Errorf("After clear: TotalEdges = %d", st.TotalEdges)
	}
}

func TestLoadIndex(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})

	ix := db.LoadIndex()
	if ix.GetNode("a") == nil {
		t.Error("LoadIndex missing node a")
	}
	if ix.GetNode("b") == nil {
		t.Error("LoadIndex missing node b")
	}
	fwd := ix.ForwardNodes("a", model.EdgeCalls)
	if len(fwd) != 1 || fwd[0].ID != "b" {
		t.Errorf("ForwardNodes = %v, want [b]", fwd)
	}
}

func TestMeta(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	if err := db.SetMeta("repo", "myrepo"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	val, err := db.GetMeta("repo")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "myrepo" {
		t.Errorf("GetMeta = %q, want %q", val, "myrepo")
	}
}

func TestExportJSON(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	data, err := db.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	if len(data) == 0 {
		t.Error("ExportJSON returned empty data")
	}
}

func TestNodes(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindType, Name: "b"})

	all := db.Nodes("")
	if len(all) != 2 {
		t.Errorf("Nodes('') = %d, want 2", len(all))
	}

	funcs := db.Nodes(model.KindFunc)
	if len(funcs) != 1 {
		t.Errorf("Nodes(func) = %d, want 1", len(funcs))
	}
}

func TestNeighbors(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertNode(&model.Node{ID: "c", Kind: model.KindFunc, Name: "c"})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})
	db.UpsertEdge(model.Edge{Src: "b", Dst: "c", Type: model.EdgeCalls})

	// depth=1
	neighbors := db.Neighbors("a", model.EdgeCalls, 1)
	if len(neighbors) < 2 { // a + b
		t.Errorf("Neighbors depth=1 = %d nodes, want >=2", len(neighbors))
	}

	// depth=2
	neighbors2 := db.Neighbors("a", model.EdgeCalls, 2)
	if len(neighbors2) < 3 { // a + b + c
		t.Errorf("Neighbors depth=2 = %d nodes, want >=3", len(neighbors2))
	}
}

func TestImpact(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertNode(&model.Node{ID: "c", Kind: model.KindFunc, Name: "c"})
	db.UpsertEdge(model.Edge{Src: "b", Dst: "a", Type: model.EdgeCalls})
	db.UpsertEdge(model.Edge{Src: "c", Dst: "b", Type: model.EdgeCalls})

	impact := db.Impact("a", 0) // 0 = full closure
	if len(impact) < 2 { // b calls a, c calls b
		t.Errorf("Impact = %d nodes, want >=2", len(impact))
	}
}

func TestCascadeDelete(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	db.UpsertNode(&model.Node{ID: "a", Kind: model.KindFunc, Name: "a"})
	db.UpsertNode(&model.Node{ID: "b", Kind: model.KindFunc, Name: "b"})
	db.UpsertEdge(model.Edge{Src: "a", Dst: "b", Type: model.EdgeCalls})

	// Delete node a should cascade delete the edge
	conn := db.conn
	_, err := conn.Exec(`DELETE FROM nodes WHERE id = 'a'`)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify edge was cascade deleted
	fwd := db.Forward("a", model.EdgeCalls)
	if len(fwd) != 0 {
		t.Errorf("After cascade delete: Forward = %v, want empty", fwd)
	}
}

func TestEnrichment(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	n := &model.Node{
		ID:   "pkg.Foo",
		Kind: model.KindFunc,
		Name: "Foo",
		Enrich: model.Enrichment{
			Summary:  "Does foo things",
			Salience: 0.75,
			EmbedRef: "embed:123",
		},
	}
	db.UpsertNode(n)

	got := db.GetNode("pkg.Foo")
	if got.Enrich.Summary != "Does foo things" {
		t.Errorf("Summary = %q, want %q", got.Enrich.Summary, "Does foo things")
	}
	if got.Enrich.Salience != 0.75 {
		t.Errorf("Salience = %f, want 0.75", got.Enrich.Salience)
	}
}
