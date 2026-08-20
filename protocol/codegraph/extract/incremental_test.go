package extract

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"centra/protocol/codegraph/model"
	"centra/protocol/codegraph/store"
)

// writeModule lays down a small multi-package module and returns its root.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func serialize(t *testing.T, e *Extractor) string {
	t.Helper()
	var b bytes.Buffer
	if err := store.Write(&b, e.Index(), e.Merkle()); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const (
	modGoMod = "module prog\n\ngo 1.25\n"
	pkgA_v1  = "package a\n\n// Add adds one.\nfunc Add(x int) int { return x + 1 }\n"
	pkgA_v2  = "package a\n\n// Add adds two now.\nfunc Add(x int) int { return x + 2 }\n"
	pkgB     = "package b\n\nimport \"prog/a\"\n\n// Use calls Add.\nfunc Use() int { return a.Add(2) }\n"
	pkgC     = "package c\n\n// Solo is unrelated to a and b.\nfunc Solo() int { return 42 }\n"
)

func TestIncremental_UntouchedIntact_DependentsInvalidated(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
		"b/b.go": pkgB,
		"c/c.go": pkgC,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}

	prior, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	priorTree := prior.FileMerkle()

	// Snapshot package c's node pointers to prove they are carried over intact.
	priorSolo := prior.Index().Node("prog/c.Solo")
	if priorSolo == nil {
		t.Fatal("fixture missing prog/c.Solo")
	}

	// Edit a/a.go (body change; id stable, digest moves).
	if err := os.WriteFile(filepath.Join(root, "a", "a.go"), []byte(pkgA_v2), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := IncrementalUpdate(prior, priorTree)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}

	// (10.1) The diff pinpointed a/a.go.
	if got := res.Changes.Changed; len(got) != 1 || got[0] != "a/a.go" {
		t.Fatalf("changed = %v, want [a/a.go]", got)
	}

	// (10.4) prog/a.Add's digest moved; prog/c.Solo did not.
	if !contains(res.Moved, "prog/a.Add") {
		t.Fatalf("moved = %v, want to include prog/a.Add", res.Moved)
	}
	if contains(res.Moved, "prog/c.Solo") {
		t.Fatalf("prog/c.Solo must not move: %v", res.Moved)
	}

	// (10.3) Reverse-edge invalidation found prog/b.Use (it calls a.Add).
	if !contains(res.Dependents, "prog/b.Use") {
		t.Fatalf("dependents = %v, want to include prog/b.Use", res.Dependents)
	}

	// (10.2) The unrelated package c is not re-extracted and its node is the
	// very same object carried over from prior.
	if contains(res.AffectedPkgs, "prog/c") {
		t.Fatalf("prog/c must not be affected: %v", res.AffectedPkgs)
	}
	if got := res.Extractor.Index().Node("prog/c.Solo"); got != priorSolo {
		t.Fatalf("prog/c.Solo was re-extracted (pointer changed) — not carried intact")
	}

	// Strongest check: the incremental store equals a full rebuild of the edited
	// tree, byte for byte.
	full, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	if inc, fr := serialize(t, res.Extractor), serialize(t, full); inc != fr {
		t.Fatalf("incremental != full rebuild\n--- incremental ---\n%s\n--- full ---\n%s", inc, fr)
	}
}

func TestIncremental_NoChangeIsNoOp(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
		"b/b.go": pkgB,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}
	prior, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	res, err := IncrementalUpdate(prior, prior.FileMerkle())
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if !res.Changes.Empty() || res.Extractor != prior {
		t.Fatalf("no-op update did work: changes=%+v", res.Changes)
	}
}

func TestIncremental_RemovedFilePrunesNodesAndEdges(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
		"a/x.go": "package a\n\n// Helper is standalone.\nfunc Helper() int { return Add(0) }\n",
		"b/b.go": pkgB,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}
	prior, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	priorTree := prior.FileMerkle()
	if prior.Index().Node("prog/a.Helper") == nil {
		t.Fatal("fixture missing prog/a.Helper")
	}

	// Delete a/x.go entirely.
	if err := os.Remove(filepath.Join(root, "a", "x.go")); err != nil {
		t.Fatal(err)
	}
	res, err := IncrementalUpdate(prior, priorTree)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if !contains(res.Removed, "prog/a.Helper") {
		t.Fatalf("removed = %v, want to include prog/a.Helper", res.Removed)
	}
	if res.Extractor.Index().Node("prog/a.Helper") != nil {
		t.Fatal("prog/a.Helper still present after its file was removed")
	}
	// Equals a full rebuild of the reduced tree.
	full, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	if inc, fr := serialize(t, res.Extractor), serialize(t, full); inc != fr {
		t.Fatalf("incremental != full rebuild after removal\n--- incremental ---\n%s\n--- full ---\n%s", inc, fr)
	}
}

// TestIncremental_ReprocessesExactlyDependents is task 5.6's dedicated proof:
// editing one function's body re-processes EXACTLY its own package plus the
// packages of its true reverse-dependents — no more — and the resulting store
// is byte-identical to a full rebuild of the edited tree.
//
// Topology: a.Add is called by both b.Use and d.Dup; c.Solo is unrelated.
// Editing a.Add must pull in a (changed), b and d (reverse call-dependents),
// and must leave c wholly untouched (carried over by pointer identity).
func TestIncremental_ReprocessesExactlyDependents(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod": modGoMod,
		"a/a.go": pkgA_v1,
		"b/b.go": pkgB,
		"d/d.go": "package d\n\nimport \"prog/a\"\n\n// Dup also calls Add.\nfunc Dup() int { return a.Add(9) }\n",
		"c/c.go": pkgC,
	})
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}

	prior, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	priorTree := prior.FileMerkle()

	// Pointer-identity snapshots of the packages that must NOT be re-extracted.
	priorSolo := prior.Index().Node("prog/c.Solo")
	priorCPkg := prior.Index().Node("prog/c")
	if priorSolo == nil || priorCPkg == nil {
		t.Fatal("fixture missing package c nodes")
	}

	if err := os.WriteFile(filepath.Join(root, "a", "a.go"), []byte(pkgA_v2), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := IncrementalUpdate(prior, priorTree)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}

	// The affected package set is EXACTLY {a, b, d}: a changed, b and d are its
	// reverse call-dependents. Nothing else is re-extracted.
	wantAffected := []string{"prog/a", "prog/b", "prog/d"}
	if !equalStrings(res.AffectedPkgs, wantAffected) {
		t.Fatalf("affected pkgs = %v, want exactly %v", res.AffectedPkgs, wantAffected)
	}

	// Both true dependents were found by reverse-edge invalidation; c.Solo was not.
	if !contains(res.Dependents, "prog/b.Use") || !contains(res.Dependents, "prog/d.Dup") {
		t.Fatalf("dependents = %v, want prog/b.Use and prog/d.Dup", res.Dependents)
	}
	if contains(res.Dependents, "prog/c.Solo") {
		t.Fatalf("prog/c.Solo is not a dependent: %v", res.Dependents)
	}

	// Package c is carried over untouched — same pointers, proving no re-extraction.
	if got := res.Extractor.Index().Node("prog/c.Solo"); got != priorSolo {
		t.Fatal("prog/c.Solo re-extracted (pointer changed) — not carried intact")
	}
	if got := res.Extractor.Index().Node("prog/c"); got != priorCPkg {
		t.Fatal("prog/c package node re-extracted (pointer changed) — not carried intact")
	}

	// The incremental store equals a full rebuild of the edited tree, byte for byte.
	full, _, err := Build(cfg)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	if inc, fr := serialize(t, res.Extractor), serialize(t, full); inc != fr {
		t.Fatalf("incremental != full rebuild\n--- incremental ---\n%s\n--- full ---\n%s", inc, fr)
	}
}

func equalStrings(a, b []string) bool {
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

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var _ = model.KindFunc
