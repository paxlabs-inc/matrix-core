package extract

import (
	"fmt"
	"os"
	"path/filepath"

	"centra/protocol/codegraph/merkle"
)

// CheckStale reports whether the graph store in graphDir is out of date relative
// to the current source. It is the cheap CI staleness gate: it reads the stored
// repo file Merkle tree (graph/merkle.kvx) and compares it to a freshly computed
// one — a single root comparison, no full graph rebuild. A missing or corrupt
// store is treated as stale. When stale, the returned Changes name exactly which
// files drifted, for an actionable failure message. (Requirements 5.2, 11.1,
// 11.2.)
func CheckStale(cfg Config, graphDir string) (stale bool, changes merkle.Changes, err error) {
	f, err := os.Open(filepath.Join(graphDir, "merkle.kvx"))
	if err != nil {
		if os.IsNotExist(err) {
			return true, merkle.Changes{}, fmt.Errorf("no graph store at %s (run: codegraph build)", graphDir)
		}
		return true, merkle.Changes{}, err
	}
	defer f.Close()

	stored, err := merkle.Read(f)
	if err != nil {
		return true, merkle.Changes{}, err
	}
	cur, err := FileTree(cfg)
	if err != nil {
		return false, merkle.Changes{}, err
	}
	changes = merkle.Diff(stored, cur)
	return !changes.Empty(), changes, nil
}
