package extract

import (
	"path/filepath"
	"sort"

	"golang.org/x/tools/go/packages"

	"matrix/codegraph/merkle"
	"matrix/codegraph/model"
)

// invalidationEdges are the edge types over which a change propagates to
// dependents: if a node is changed or removed, whatever calls it, references it,
// or implements it may need its own edges revalidated.
var invalidationEdges = []model.EdgeType{model.EdgeCalls, model.EdgeReferences, model.EdgeImplements}

// IncrementalResult reports what an incremental update touched.
type IncrementalResult struct {
	Changes      merkle.Changes // file-level delta that drove the update
	AffectedPkgs []string       // package import paths re-extracted (sorted)
	Dependents   []string       // dependent node ids pulled in via reverse-edge invalidation (sorted)
	Moved        []string       // node ids whose digest changed or that were added (sorted)
	Removed      []string       // node ids that disappeared (sorted)
	Extractor    *Extractor     // the updated graph
	Tree         *merkle.Tree   // the current file Merkle tree
}

// IncrementalUpdate applies a source change to a prior in-memory extraction
// without a full rebuild. priorTree is the file Merkle tree captured at the
// prior build (before the change). It:
//
//  1. recomputes the current file tree and diffs it (Requirement 10.1);
//  2. finds the nodes owned by changed/removed files, and via the reverse-edge
//     index the true dependents whose edges must be revalidated (10.3);
//  3. reloads and re-extracts ONLY the affected packages, recomputing their
//     nodes and outgoing edges, and carries every untouched package's nodes and
//     edges over unchanged (10.2);
//  4. recomputes container digests and reports which node digests moved (10.4).
//
// Re-extraction is package-granular because Go type resolution is: a file's
// edges are computed from its type-checked package. Untouched packages are never
// reloaded. This is modular, not whole-program — a change that alters
// interface-dispatch resolution in an unrelated package is out of scope here and
// is what the full-rebuild equivalence test (task 5.6) guards.
func IncrementalUpdate(prior *Extractor, priorTree *merkle.Tree) (*IncrementalResult, error) {
	cfg := prior.cfg
	curTree, err := FileTree(cfg)
	if err != nil {
		return nil, err
	}
	changes := merkle.Diff(priorTree, curTree)
	res := &IncrementalResult{Changes: changes, Extractor: prior, Tree: curTree}
	if changes.Empty() {
		return res, nil
	}

	// (2) Seed nodes: everything owned by a changed or removed file.
	touched := map[string]bool{}
	for _, p := range changes.Changed {
		touched[p] = true
	}
	for _, p := range changes.Removed {
		touched[p] = true
	}
	affected := map[string]bool{} // package import paths to re-extract
	dependents := map[string]bool{}
	for _, n := range prior.ix.Nodes() {
		if !touched[n.File] {
			continue
		}
		if pkg := prior.pkgOf[n.Id]; pkg != "" {
			affected[pkg] = true
		}
		// Reverse-edge invalidation: dependents whose incoming edges targeted a
		// touched node must have their outgoing edges revalidated.
		for _, t := range invalidationEdges {
			for _, dep := range prior.ix.Reverse(n.Id, t) {
				dependents[dep] = true
				if pkg := prior.pkgOf[dep]; pkg != "" {
					affected[pkg] = true
				}
			}
		}
	}

	// (3) Reload only the affected packages. Changed/added files are located by
	// file= query (this also discovers packages new to the graph); known
	// affected package paths are loaded by import path (covers removed-file
	// packages that still exist and reverse-dependents with no changed file).
	var filePatterns []string
	for _, rel := range append(append([]string{}, changes.Added...), changes.Changed...) {
		filePatterns = append(filePatterns, "file="+filepath.Join(cfg.RepoRoot, filepath.FromSlash(rel)))
	}
	importPaths := make([]string, 0, len(affected))
	for pkg := range affected {
		importPaths = append(importPaths, pkg)
	}
	sort.Strings(importPaths)

	pkgs, err := loadPackages(cfg, filePatterns, importPaths)
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		affected[p.PkgPath] = true
	}

	sub := New(cfg)
	sub.reextract(pkgs)

	// (3 cont.) Merge: prior's untouched packages + sub's re-extracted packages.
	out := New(cfg)
	for _, n := range prior.ix.Nodes() {
		pkg := prior.pkgOf[n.Id]
		if pkg == "" {
			out.ix.AddNode(n) // repo/module containers — recomputed below
			continue
		}
		if affected[pkg] {
			continue // comes from sub
		}
		out.ix.AddNode(n)
		out.pkgOf[n.Id] = pkg
	}
	for _, n := range sub.ix.Nodes() {
		if pkg := sub.pkgOf[n.Id]; pkg != "" {
			out.ix.AddNode(n)
			out.pkgOf[n.Id] = pkg
		}
	}
	for modPath, dir := range prior.modules {
		out.modules[modPath] = dir
	}

	// Edges: keep prior edges whose source is an untouched package (or a
	// container); drop those whose source was re-extracted (sub re-emits them).
	// A removed node's outgoing edges vanish with it (its package is affected),
	// and any dependent that pointed at it was pulled into the affected set by
	// reverse-edge invalidation and re-extracted, so no carried edge dangles in a
	// valid build — matching a full rebuild edge-for-edge without extra pruning.
	for _, e := range prior.ix.Edges() {
		if pkg := prior.pkgOf[e.Src]; pkg != "" && affected[pkg] {
			continue
		}
		out.ix.AddEdge(e)
	}
	for _, e := range sub.ix.Edges() {
		out.ix.AddEdge(e)
	}

	out.finalizeContainers()

	// (4) Report moved and removed nodes by digest.
	var moved, removed []string
	for _, n := range out.ix.Nodes() {
		if old := prior.ix.Node(n.Id); old == nil || old.Digest != n.Digest {
			moved = append(moved, n.Id)
		}
	}
	for _, n := range prior.ix.Nodes() {
		if out.ix.Node(n.Id) == nil {
			removed = append(removed, n.Id)
		}
	}
	sort.Strings(moved)
	sort.Strings(removed)

	affectedList := make([]string, 0, len(affected))
	for pkg := range affected {
		affectedList = append(affectedList, pkg)
	}
	sort.Strings(affectedList)
	depList := make([]string, 0, len(dependents))
	for id := range dependents {
		depList = append(depList, id)
	}
	sort.Strings(depList)

	res.Extractor = out
	res.Tree = curTree
	res.AffectedPkgs = affectedList
	res.Dependents = depList
	res.Moved = moved
	res.Removed = removed
	return res, nil
}

// reextract runs the full per-package extraction pipeline over an arbitrary set
// of packages (which may span modules), mirroring loadModule for the reloaded
// subset.
func (e *Extractor) reextract(pkgs []*packages.Package) {
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })
	for _, p := range pkgs {
		moduleID := ""
		if p.Module != nil {
			moduleID = "mod:" + p.Module.Path
			e.modules[p.Module.Path] = e.rel(p.Module.Dir)
		}
		e.extractPackage(p, moduleID)
	}
	e.extractCalls(pkgs)
	e.extractTypeEdges(pkgs)
}

// loadPackages loads a bounded set of packages by file= query and/or import path
// pattern, returning the deduplicated repo packages that type-checked.
func loadPackages(cfg Config, filePatterns, importPaths []string) ([]*packages.Package, error) {
	patterns := append(append([]string{}, filePatterns...), importPaths...)
	if len(patterns) == 0 {
		return nil, nil
	}
	pc := &packages.Config{Mode: loadMode, Dir: cfg.RepoRoot}
	loaded, err := packages.Load(pc, patterns...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*packages.Package
	for _, p := range loaded {
		if p.Types == nil || p.PkgPath == "" || seen[p.PkgPath] {
			continue
		}
		seen[p.PkgPath] = true
		out = append(out, p)
	}
	return out, nil
}
