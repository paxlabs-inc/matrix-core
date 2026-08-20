package extract

import (
	"os"
	"path/filepath"
	"sort"

	"centra/protocol/codegraph/merkle"
	"centra/protocol/codegraph/model"
	"centra/protocol/codegraph/store"
)

// Build runs a full extraction and returns the graph plus its repo Merkle root.
func Build(cfg Config) (*Extractor, string, error) {
	e := New(cfg)
	if err := e.Run(); err != nil {
		return nil, "", err
	}
	return e, e.Merkle(), nil
}

// WriteStore serializes the graph as sharded .kvx — graph/<pkgPath>.kvx per
// package plus graph/manifest.kvx for the repo and module container nodes — each
// carrying the GENERATED banner with merkle. outDir is the graph root; every
// path written inside is already repo-relative.
func (e *Extractor) WriteStore(outDir, merkle string) error {
	shards := map[string][]*model.Node{}
	var manifest []*model.Node
	for _, n := range e.ix.Nodes() { // globally id-sorted; filtering preserves order
		if pkg := e.pkgOf[n.Id]; pkg != "" {
			shards[pkg] = append(shards[pkg], n)
		} else {
			manifest = append(manifest, n)
		}
	}

	pkgs := make([]string, 0, len(shards))
	for pkg := range shards {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		path := filepath.Join(outDir, filepath.FromSlash(pkg)+".kvx")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeFile(path, e.ix, shards[pkg], merkle); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := e.writeMerkle(filepath.Join(outDir, "merkle.kvx")); err != nil {
		return err
	}
	return writeFile(filepath.Join(outDir, "manifest.kvx"), e.ix, manifest, merkle)
}

// writeMerkle persists the repo file Merkle tree so a later incremental run can
// diff against it to find exactly the changed files.
func (e *Extractor) writeMerkle(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := merkle.Write(f, e.FileMerkle()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeFile(path string, ix *model.Index, nodes []*model.Node, merkle string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := store.WriteNodes(f, ix, nodes, merkle); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
