package extract

import (
	"io/fs"
	"os"
	"path/filepath"

	"centra/protocol/codegraph/merkle"
)

// RepoFiles returns the canonical source-file set the graph is built over:
// every non-test .go file under the configured module dirs, keyed by repo-
// relative slash path -> raw bytes. It skips _test.go files and the vendor,
// testdata, node_modules, and .git trees — the same set go/packages loads for a
// non-test build — so the file Merkle tree computed from it stays in lockstep
// with the extracted graph across full and incremental builds.
func RepoFiles(cfg Config) (map[string][]byte, error) {
	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	seen := map[string]bool{}
	for _, mod := range cfg.Modules {
		err := filepath.WalkDir(mod, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "third_party", "testdata":
					return fs.SkipDir
				}
				return nil
			}
			if !isGraphSource(d.Name()) || seen[path] {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			seen[path] = true
			out[filepath.ToSlash(rel)] = b
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func isGraphSource(name string) bool {
	if len(name) < 4 || name[len(name)-3:] != ".go" {
		return false
	}
	if len(name) >= 8 && name[len(name)-8:] == "_test.go" {
		return false
	}
	return true
}

// FileTree builds the repo file Merkle tree from the canonical source-file set.
func FileTree(cfg Config) (*merkle.Tree, error) {
	files, err := RepoFiles(cfg)
	if err != nil {
		return nil, err
	}
	return merkle.FromContentMap(files), nil
}
