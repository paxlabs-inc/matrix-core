package extract

import (
	"fmt"
	"os"
	"path/filepath"
)

var SelfModelModules = []string{
	"neo",
	"cody",
	"executor",
	"cortex",
}

var SelfModelRequiredPackages = []string{
	"neo/internal/agent",
	"neo/internal/memory",
	"neo/internal/server",
	"neo/internal/tools",
	"cody/internal/worker",
	"cody/internal/orchestrator",
	"executor/cmd/mcl-execute",
	"cortex",
}

func SelfModelConfig(repoRoot string) (Config, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return Config{}, err
	}
	modules := make([]string, 0, len(SelfModelModules))
	for _, rel := range SelfModelModules {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		info, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr != nil || info.IsDir() {
			return Config{}, fmt.Errorf("self-model module %s: %w", rel, statErr)
		}
		modules = append(modules, dir)
	}
	for _, rel := range SelfModelRequiredPackages {
		info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if statErr != nil || !info.IsDir() {
			return Config{}, fmt.Errorf("self-model package %s: %w", rel, statErr)
		}
	}
	return Config{RepoRoot: root, RepoName: "matrix-self-model", Modules: modules}, nil
}
