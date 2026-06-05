// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// modules.go — the source-graph pass over the HyperPax-OS tree. Each Cosmos
// SDK module / EVM precompile becomes a Fact node; each module that owns a
// Keeper additionally gets a Capability node (keeper-of => part-of edge).
// Inter-module imports become depends-on (references) edges.
//
// Summaries are derived heuristically and deterministically — package doc
// comment, README, a Keeper/AppModule type doc comment, or a package-clause
// fallback. No LLM is consulted.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/cortex/memory"
)

// moduleInfo is the gathered, pre-write description of one source module.
type moduleInfo struct {
	relpath   string // e.g. "x/evm" or "precompiles/staking" or "app"
	base      string // last path element, e.g. "evm"
	abs       string
	pkg       string
	summary   string
	hasKeeper bool
	factID    memory.ID
	deps      map[string]struct{} // relpaths of imported sibling modules
}

func (ig *ingester) ingestModules(root string) error {
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("HyperPax-OS tree not found at %s: %w", root, err)
	}

	relpaths := discoverModules(root)
	known := map[string]struct{}{}
	for _, r := range relpaths {
		known[r] = struct{}{}
	}

	mods := make([]*moduleInfo, 0, len(relpaths))
	for _, rel := range relpaths {
		mi, err := gatherModule(root, rel, known)
		if err != nil {
			return err
		}
		if mi != nil {
			mods = append(mods, mi)
		}
	}

	// First pass: write all Facts + keeper Capabilities so depends-on edges
	// can reference any target regardless of order.
	for _, mi := range mods {
		fact := memory.FactData{
			SchemaVersion: 1,
			Statement:     truncate(condense(mi.summary), 1000),
			Subject:       "matrix://hyperpax/" + mi.relpath,
			Predicate:     "module",
			Source:        "knowledge/HyperPax-OS/" + mi.relpath,
		}
		id, err := ig.upsertNode("gideon:module:"+mi.relpath, fact, 5, 0.8,
			"hyperpax-os", "module", "module:"+mi.relpath)
		if err != nil {
			return err
		}
		mi.factID = id
		if _, ok := ig.moduleFacts[mi.base]; !ok { // first-wins, deterministic
			ig.moduleFacts[mi.base] = id
		}

		if mi.hasKeeper {
			cap := memory.CapabilityData{
				SchemaVersion: 1,
				Subject:       "matrix://hyperpax/" + mi.relpath,
				Capability:    "keeper: owns and mutates the " + mi.base + " module state",
				Verified:      true,
				LastObserved:  stableObservedAt,
			}
			kid, err := ig.upsertNode("gideon:keeper:"+mi.relpath, cap, 5, 0.8,
				"hyperpax-os", "keeper", "module:"+mi.relpath)
			if err != nil {
				return err
			}
			if err := ig.linkEdge(kid, memory.EdgePartOf, id, "keeper-of"); err != nil {
				return err
			}
		}
	}

	// Second pass: depends-on edges.
	byRel := map[string]memory.ID{}
	for _, mi := range mods {
		byRel[mi.relpath] = mi.factID
	}
	for _, mi := range mods {
		deps := make([]string, 0, len(mi.deps))
		for d := range mi.deps {
			deps = append(deps, d)
		}
		sort.Strings(deps) // deterministic edge order
		for _, dep := range deps {
			if dep == mi.relpath {
				continue
			}
			if dstID, ok := byRel[dep]; ok {
				if err := ig.linkEdge(mi.factID, memory.EdgeReferences, dstID, "depends-on"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// discoverModules returns the sorted relpaths of every module target:
// every subdir of x/ and precompiles/, plus the app/rpc/indexer singletons.
func discoverModules(root string) []string {
	var rels []string
	for _, parent := range []string{"x", "precompiles"} {
		entries, err := os.ReadDir(filepath.Join(root, parent))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				rels = append(rels, parent+"/"+e.Name())
			}
		}
	}
	for _, single := range []string{"app", "rpc", "indexer"} {
		if fi, err := os.Stat(filepath.Join(root, single)); err == nil && fi.IsDir() {
			rels = append(rels, single)
		}
	}
	sort.Strings(rels)
	return rels
}

// gatherModule scans one module directory: package name, derived summary,
// keeper presence, and the set of sibling modules it imports.
func gatherModule(root, rel string, known map[string]struct{}) (*moduleInfo, error) {
	abs := filepath.Join(root, rel)
	mi := &moduleInfo{
		relpath: rel,
		base:    filepath.Base(rel),
		abs:     abs,
		deps:    map[string]struct{}{},
	}

	// Top-level files drive the package name and primary summary candidates;
	// keeper/keeper.go is read too when present.
	topFiles, err := readGoFiles(abs, false)
	if err != nil {
		return nil, err
	}
	if len(topFiles) == 0 {
		// No top-level Go (some dirs are docs/solidity only); skip silently.
		// Still emit a Fact if the dir clearly is a module — but with no Go
		// there is nothing to describe, so skip.
		if !hasAnyGo(abs) {
			return nil, nil
		}
	}

	// keeper detection.
	if fi, err := os.Stat(filepath.Join(abs, "keeper")); err == nil && fi.IsDir() {
		mi.hasKeeper = true
	} else {
		for name := range topFiles {
			if strings.Contains(strings.ToLower(name), "keeper") {
				mi.hasKeeper = true
				break
			}
		}
	}

	// package name: prefer module.go, else any file.
	order := orderedNames(topFiles, "module.go")
	for _, n := range order {
		if pkg, _ := scanGoFile(topFiles[n]); pkg != "" {
			mi.pkg = pkg
			break
		}
	}

	mi.summary = ig_deriveSummary(abs, topFiles, order, mi)

	// depends-on: walk the whole subtree's imports.
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		_, imports := scanGoFile(string(b))
		for _, imp := range imports {
			if dep, ok := resolveModuleRel(imp, known); ok {
				mi.deps[dep] = struct{}{}
			}
		}
		return nil
	})
	delete(mi.deps, rel)
	return mi, nil
}

// ig_deriveSummary applies the deterministic heuristic cascade.
func ig_deriveSummary(abs string, topFiles map[string]string, order []string, mi *moduleInfo) string {
	// 1. package doc comment.
	for _, n := range order {
		if doc := docAbovePackage(topFiles[n]); doc != "" {
			return doc
		}
	}
	// 2. README.md first paragraph.
	if b, err := os.ReadFile(filepath.Join(abs, "README.md")); err == nil {
		if para := firstParagraph(string(b)); para != "" {
			return para
		}
	}
	// 3. Keeper / AppModule type doc comment (incl. keeper/keeper.go).
	candidates := order
	if kb, err := os.ReadFile(filepath.Join(abs, "keeper", "keeper.go")); err == nil {
		topFiles["keeper/keeper.go"] = string(kb)
		candidates = append([]string{"keeper/keeper.go"}, candidates...)
	}
	for _, n := range candidates {
		if doc := docAboveType(topFiles[n], []string{"Keeper", "AppModule", "AppModuleBasic", "Module"}); doc != "" {
			return doc
		}
	}
	// 4. fallback.
	pkg := mi.pkg
	if pkg == "" {
		pkg = mi.base
	}
	return fmt.Sprintf("HyperPax-OS %s module (package %s)", mi.relpath, pkg)
}

// --- low-level Go-source scanning -----------------------------------------

// readGoFiles returns name->content for .go files in dir. When recurse is
// false only the top level is read; _test.go files are always skipped.
func readGoFiles(dir string, recurse bool) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		out[name] = string(b)
	}
	return out, nil
}

func hasAnyGo(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// orderedNames returns the file names with `prefer` first (if present), the
// rest sorted for determinism.
func orderedNames(files map[string]string, prefer string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if _, ok := files[prefer]; ok {
		out := []string{prefer}
		for _, n := range names {
			if n != prefer {
				out = append(out, n)
			}
		}
		return out
	}
	return names
}

// scanGoFile pulls the package name and the import paths out of Go source via
// line scanning (no go/parser dependency, robust to fork-specific syntax).
func scanGoFile(content string) (pkg string, imports []string) {
	inBlock := false
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		if inBlock {
			if t == ")" {
				inBlock = false
				continue
			}
			if imp, ok := extractImport(t); ok {
				imports = append(imports, imp)
			}
			continue
		}
		if pkg == "" && strings.HasPrefix(t, "package ") {
			if f := strings.Fields(t); len(f) >= 2 {
				pkg = f[1]
			}
			continue
		}
		if t == "import (" || t == "import(" {
			inBlock = true
			continue
		}
		if strings.HasPrefix(t, "import \"") || strings.HasPrefix(t, "import _ \"") || strings.HasPrefix(t, "import . \"") {
			if imp, ok := extractImport(t); ok {
				imports = append(imports, imp)
			}
		}
	}
	return pkg, imports
}

func extractImport(t string) (string, bool) {
	i := strings.IndexByte(t, '"')
	if i < 0 {
		return "", false
	}
	j := strings.LastIndexByte(t, '"')
	if j <= i {
		return "", false
	}
	return t[i+1 : j], true
}

// resolveModuleRel maps an import path to a known module relpath, if any.
func resolveModuleRel(importPath string, known map[string]struct{}) (string, bool) {
	segs := strings.Split(importPath, "/")
	for i, s := range segs {
		if (s == "x" || s == "precompiles") && i+1 < len(segs) {
			cand := s + "/" + segs[i+1]
			if _, ok := known[cand]; ok {
				return cand, true
			}
		}
	}
	for _, single := range []string{"app", "rpc", "indexer"} {
		if _, ok := known[single]; !ok {
			continue
		}
		for _, s := range segs {
			if s == single {
				return single, true
			}
		}
	}
	return "", false
}

// docAbovePackage returns the contiguous //-comment block directly above the
// `package` clause, ignoring license/copyright boilerplate.
func docAbovePackage(content string) string {
	lines := strings.Split(content, "\n")
	pkgIdx := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "package ") {
			pkgIdx = i
			break
		}
	}
	if pkgIdx <= 0 {
		return ""
	}
	var rev []string
	for i := pkgIdx - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			break
		}
		if !strings.HasPrefix(t, "//") {
			break
		}
		rev = append(rev, strings.TrimSpace(strings.TrimPrefix(t, "//")))
	}
	if len(rev) == 0 {
		return ""
	}
	// reverse into reading order.
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	doc := condense(strings.Join(rev, " "))
	if looksLikeLicense(doc) {
		return ""
	}
	return truncate(doc, 400)
}

// docAboveType returns the first-sentence doc comment attached to one of the
// named exported types (e.g. Keeper).
func docAboveType(content string, names []string) string {
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		for _, name := range names {
			if strings.HasPrefix(t, "type "+name+" ") {
				var rev []string
				for j := i - 1; j >= 0; j-- {
					ct := strings.TrimSpace(lines[j])
					if ct == "" || !strings.HasPrefix(ct, "//") {
						break
					}
					rev = append(rev, strings.TrimSpace(strings.TrimPrefix(ct, "//")))
				}
				if len(rev) == 0 {
					continue
				}
				for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
					rev[l], rev[r] = rev[r], rev[l]
				}
				doc := condense(strings.Join(rev, " "))
				if looksLikeLicense(doc) || doc == "" {
					continue
				}
				return truncate(firstSentence(doc), 400)
			}
		}
	}
	return ""
}

func firstParagraph(md string) string {
	var buf []string
	started := false
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "![") || strings.HasPrefix(t, "[!") {
			if started {
				break
			}
			continue
		}
		if t == "" {
			if started {
				break
			}
			continue
		}
		buf = append(buf, t)
		started = true
	}
	return truncate(condense(strings.Join(buf, " ")), 400)
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func looksLikeLicense(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{"copyright", "spdx", "license", "http://", "https://", "all rights reserved"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
