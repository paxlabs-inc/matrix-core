// Package merkle maintains a deterministic, directory-structured Merkle tree
// over the repo's source files. Each file is a leaf keyed by its repo-relative
// slash path and hashed over LF-normalized content; each directory is an
// internal node whose hash commits to the sorted, type-tagged (name, child-hash)
// pairs of its immediate files and subdirectories.
//
// Two trees built from identical file contents have identical roots, so a
// single root comparison detects any change (Requirement 10.1). Diff walks the
// two trees in tandem and skips any subtree whose directory hash is unchanged,
// so it reports the changed file set in proportion to what moved rather than
// re-reading every file. This is the same content-addressed discipline the
// cortex snapshot layer uses, sized down to file-change detection.
package merkle

import (
	"encoding/hex"
	"path"
	"sort"
	"strings"

	"lukechampine.com/blake3"

	"centra/protocol/codegraph/model"
)

// Domain-separation prefixes keep leaf and node hashes from ever colliding.
const (
	leafDomain = "matrix.codegraph.merkle.leaf.v1\x00"
	nodeDomain = "matrix.codegraph.merkle.node.v1\x00"
)

// Tree is a built Merkle tree. It is immutable after construction.
type Tree struct {
	Root       string              // "b3:<hex>" over the root directory (".")
	files      map[string]string   // repo-relative slash path -> leaf hash
	dirs       map[string]string   // dir path ("." = root) -> subtree hash
	childFiles map[string][]string // dir -> immediate file paths (sorted)
	childDirs  map[string][]string // dir -> immediate subdir paths (sorted)
}

// Empty is the tree over zero files; its Root is the hash of an empty root dir.
func Empty() *Tree { return FromLeaves(nil) }

// LeafHash is the content hash of one file: blake3 over its LF-normalized,
// per-line trailing-whitespace-stripped bytes, domain-separated. Rendered as
// "b3:<hex>".
func LeafHash(content []byte) string {
	return hashBytes(leafDomain, model.NormalizeSource(content))
}

// FromContentMap builds a tree from repo-relative slash path -> file content.
func FromContentMap(contents map[string][]byte) *Tree {
	leaves := make(map[string]string, len(contents))
	for p, b := range contents {
		leaves[path.Clean(p)] = LeafHash(b)
	}
	return FromLeaves(leaves)
}

// FromLeaves builds a tree from precomputed repo-relative slash path -> leaf
// hash. It is the shared constructor: callers that already hold content hashes
// (the extractor, the store loader) avoid re-hashing.
func FromLeaves(leaves map[string]string) *Tree {
	t := &Tree{
		files:      map[string]string{},
		dirs:       map[string]string{},
		childFiles: map[string][]string{},
		childDirs:  map[string][]string{},
	}
	for p, h := range leaves {
		t.files[path.Clean(p)] = h
	}

	dirSet := map[string]bool{".": true}
	childDirSet := map[string]map[string]bool{}
	for p := range t.files {
		d := path.Dir(p)
		t.childFiles[d] = append(t.childFiles[d], p)
		for d != "." {
			dirSet[d] = true
			parent := path.Dir(d)
			if childDirSet[parent] == nil {
				childDirSet[parent] = map[string]bool{}
			}
			childDirSet[parent][d] = true
			d = parent
		}
	}
	for d := range dirSet {
		for cd := range childDirSet[d] {
			t.childDirs[d] = append(t.childDirs[d], cd)
		}
		sort.Strings(t.childDirs[d])
		sort.Strings(t.childFiles[d])
	}

	// Compute directory hashes deepest-first so children are ready before parents.
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := depth(dirs[i]), depth(dirs[j])
		if di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	for _, d := range dirs {
		var sb strings.Builder
		for _, fp := range t.childFiles[d] {
			sb.WriteString("f\x00")
			sb.WriteString(path.Base(fp))
			sb.WriteByte(0)
			sb.WriteString(t.files[fp])
			sb.WriteByte('\n')
		}
		for _, cd := range t.childDirs[d] {
			sb.WriteString("d\x00")
			sb.WriteString(path.Base(cd))
			sb.WriteByte(0)
			sb.WriteString(t.dirs[cd])
			sb.WriteByte('\n')
		}
		t.dirs[d] = hashBytes(nodeDomain, []byte(sb.String()))
	}
	t.Root = t.dirs["."]
	return t
}

// Files returns the repo-relative slash paths of every leaf, sorted.
func (t *Tree) Files() []string {
	out := make([]string, 0, len(t.files))
	for p := range t.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// FileHash returns the leaf hash of a path, or "" if absent.
func (t *Tree) FileHash(p string) string { return t.files[path.Clean(p)] }

// Changes is the file-level delta between two trees. Every slice is sorted.
type Changes struct {
	Added   []string
	Removed []string
	Changed []string
}

// Empty reports whether the delta is empty.
func (c Changes) Empty() bool {
	return len(c.Added) == 0 && len(c.Removed) == 0 && len(c.Changed) == 0
}

// All returns every affected path (added ∪ changed ∪ removed), sorted.
func (c Changes) All() []string {
	out := append(append(append([]string{}, c.Added...), c.Changed...), c.Removed...)
	sort.Strings(out)
	return out
}

// Diff returns the file-level delta from old to new. A nil tree is treated as
// Empty(). When the roots match it returns immediately with no delta; otherwise
// it walks the two trees in tandem, skipping any subtree whose directory hash is
// unchanged, so unchanged files are never revisited.
func Diff(old, new *Tree) Changes {
	if old == nil {
		old = Empty()
	}
	if new == nil {
		new = Empty()
	}
	var c Changes
	if old.Root != new.Root {
		diffDir(old, new, ".", &c)
	}
	sort.Strings(c.Added)
	sort.Strings(c.Removed)
	sort.Strings(c.Changed)
	return c
}

func diffDir(old, new *Tree, dir string, c *Changes) {
	oh, ohok := old.dirs[dir]
	nh, nhok := new.dirs[dir]
	if ohok && nhok && oh == nh {
		return // whole subtree unchanged — the point of the tree
	}

	// Files directly in this dir.
	seen := map[string]bool{}
	for _, fp := range old.childFiles[dir] {
		seen[fp] = true
		nf, ok := new.files[fp]
		switch {
		case !ok:
			c.Removed = append(c.Removed, fp)
		case nf != old.files[fp]:
			c.Changed = append(c.Changed, fp)
		}
	}
	for _, fp := range new.childFiles[dir] {
		if !seen[fp] {
			if _, ok := old.files[fp]; !ok {
				c.Added = append(c.Added, fp)
			}
		}
	}

	// Recurse into the union of immediate subdirs.
	subs := map[string]bool{}
	for _, cd := range old.childDirs[dir] {
		subs[cd] = true
	}
	for _, cd := range new.childDirs[dir] {
		subs[cd] = true
	}
	ordered := make([]string, 0, len(subs))
	for cd := range subs {
		ordered = append(ordered, cd)
	}
	sort.Strings(ordered)
	for _, cd := range ordered {
		diffDir(old, new, cd, c)
	}
}

func depth(dir string) int {
	if dir == "." {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

func hashBytes(domain string, b []byte) string {
	buf := make([]byte, 0, len(domain)+len(b))
	buf = append(buf, domain...)
	buf = append(buf, b...)
	sum := blake3.Sum256(buf)
	return "b3:" + hex.EncodeToString(sum[:])
}
