// Package extract defines the interface and common types for language-specific
// code extractors. Each language extractor turns source into canonical
// model.Node and model.Edge objects — pure functions of source with no LLM or
// embedding dependency.
package extract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codegraph-ultra/internal/model"
)

// Config describes an extraction run for a single repository.
type Config struct {
	RepoRoot string   // absolute repo root; emitted file paths are relative to it
	RepoName string   // e.g. "myproject"
	Modules  []string // absolute module/package directories (language-specific meaning)
}

// Result holds the output of an extraction run.
type Result struct {
	Nodes []*model.Node
	Edges []model.Edge
}

// Extractor is the interface every language extractor must satisfy.
type Extractor interface {
	// Extract runs the extraction and returns nodes and edges.
	Extract(cfg Config) (*Result, error)
}

// Registry maps language names to extractor constructors.
var Registry = map[string]func() Extractor{}

// Register adds a language extractor to the global registry.
func Register(lang string, fn func() Extractor) {
	Registry[lang] = fn
}

// Run dispatches extraction to the registered extractor for the given language.
func Run(lang string, cfg Config) (*Result, error) {
	fn, ok := Registry[lang]
	if !ok {
		return nil, fmt.Errorf("extract: no extractor registered for %q", lang)
	}
	return fn().Extract(cfg)
}

// DiscoverGoModules finds all directories containing a go.mod file under root.
func DiscoverGoModules(root string) ([]string, error) {
	var modules []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "go.mod" && !d.IsDir() {
			modules = append(modules, filepath.Dir(path))
		}
		// Skip vendor directories and hidden directories.
		if d.IsDir() && (d.Name() == "vendor" || (len(d.Name()) > 0 && d.Name()[0] == '.')) {
			return filepath.SkipDir
		}
		return nil
	})
	return modules, err
}

// IsRepoPkg reports whether pkgPath belongs to the given repo by prefix matching.
func IsRepoPkg(repoName, pkgPath string) bool {
	return pkgPath == repoName || strings.HasPrefix(pkgPath, repoName+"/")
}
