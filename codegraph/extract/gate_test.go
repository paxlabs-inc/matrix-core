package extract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckStale_FreshThenStale(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
		"b/b.go": pkgB,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}
	graphDir := filepath.Join(root, "graph")

	e, merkle, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := e.WriteStore(graphDir, merkle); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Freshly built store is up to date.
	stale, changes, err := CheckStale(cfg, graphDir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if stale {
		t.Fatalf("freshly built store reported stale: %+v", changes)
	}

	// Editing a source file makes it stale, and the changed file is named.
	if err := os.WriteFile(filepath.Join(root, "a", "a.go"), []byte(pkgA_v2), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, changes, err = CheckStale(cfg, graphDir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !stale {
		t.Fatal("edited source did not trip the staleness gate")
	}
	if !contains(changes.Changed, "a/a.go") {
		t.Fatalf("stale changes did not name a/a.go: %+v", changes)
	}
}

func TestCheckStale_AddedAndRemovedFiles(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}
	graphDir := filepath.Join(root, "graph")
	e, merkle, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := e.WriteStore(graphDir, merkle); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Add a new source file.
	if err := os.WriteFile(filepath.Join(root, "a", "extra.go"),
		[]byte("package a\n\n// Extra is new.\nfunc Extra() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, changes, err := CheckStale(cfg, graphDir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !stale || !contains(changes.Added, "a/extra.go") {
		t.Fatalf("adding a file did not trip the gate: stale=%v changes=%+v", stale, changes)
	}
}

func TestCheckStale_MissingStoreIsStale(t *testing.T) {
	root := writeModule(t, map[string]string{"go.mod": modGoMod, "a/a.go": pkgA_v1})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}
	stale, _, err := CheckStale(cfg, filepath.Join(root, "graph"))
	if !stale || err == nil {
		t.Fatalf("missing store must be stale with an error, got stale=%v err=%v", stale, err)
	}
}
