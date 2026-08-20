package extract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelfModelScopeResolvesRealPackages(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cfg, err := SelfModelConfig(root)
	if err != nil {
		t.Fatalf("SelfModelConfig: %v", err)
	}
	if cfg.RepoName != "matrix-self-model" || len(cfg.Modules) != len(SelfModelModules) {
		t.Fatalf("unexpected self-model config: %#v", cfg)
	}
	for _, rel := range SelfModelRequiredPackages {
		info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("required self package %s does not resolve: %v", rel, statErr)
		}
	}
}
