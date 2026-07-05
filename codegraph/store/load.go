package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/codegraph/model"
)

// Load reads a whole sharded graph store from dir — every *.kvx shard plus
// manifest.kvx — and reconstructs the combined Index. merkle.kvx (the repo file
// Merkle tree, not a graph shard) is skipped. The returned merkle is the banner
// root shared by the shards. Occurrence sites are not serialized, so a loaded
// graph carries edges without sites — matching what a fresh Write emits.
func Load(dir string) (*model.Index, string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == "merkle.kvx" || !strings.HasSuffix(path, ".kvx") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(files)

	combined := model.NewIndex()
	merkle := ""
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		ix, m, err := Parse(f)
		f.Close()
		if err != nil {
			return nil, "", err
		}
		if merkle == "" {
			merkle = m
		}
		for _, n := range ix.Nodes() {
			combined.AddNode(n)
		}
		for _, e := range ix.Edges() {
			combined.AddEdge(e)
		}
	}
	return combined, merkle, nil
}
