package extract_test

import (
	"os"
	"path/filepath"
	"testing"

	"codegraph-ultra/internal/extract"
	goextract "codegraph-ultra/internal/extract/go"

	// Register language extractors via init().
	_ "codegraph-ultra/internal/extract/python"
	_ "codegraph-ultra/internal/extract/typescript"
)

func TestGoExtractorSatisfiesInterface(t *testing.T) {
	// Compile-time check: GoExtractor must implement Extractor
	var _ extract.Extractor = &goextract.GoExtractor{}
}

func TestConfigFields(t *testing.T) {
	cfg := extract.Config{
		RepoRoot: "/tmp/test",
		RepoName: "testrepo",
		Modules:  []string{"/tmp/mod1"},
	}
	if cfg.RepoRoot != "/tmp/test" {
		t.Fatal("RepoRoot")
	}
	if cfg.RepoName != "testrepo" {
		t.Fatal("RepoName")
	}
	if len(cfg.Modules) != 1 {
		t.Fatal("Modules")
	}
}

func TestResultFields(t *testing.T) {
	r := &extract.Result{}
	if r.Nodes != nil {
		t.Fatal("Nodes should be nil")
	}
	if r.Edges != nil {
		t.Fatal("Edges should be nil")
	}
}

func TestGoExtractorRegistered(t *testing.T) {
	fn, ok := extract.Registry["go"]
	if !ok {
		t.Fatal("go extractor not registered")
	}
	ex := fn()
	if ex == nil {
		t.Fatal("registered factory returned nil")
	}
}

func TestDiscoverGoModules(t *testing.T) {
	// The test's working dir is internal/extract/ — go.mod lives ../../ up.
	// DiscoverGoModules walks downward, so we need to start from a directory
	// that contains go.mod or is above it.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// Walk up two levels: internal/extract/ -> codegraph-ultra/
	root := filepath.Join(cwd, "..", "..")
	modules, err := extract.DiscoverGoModules(root)
	if err != nil {
		t.Fatalf("DiscoverGoModules(%s): %v", root, err)
	}
	if len(modules) == 0 {
		t.Fatal("expected at least one module")
	}
}

func TestIsRepoPkg(t *testing.T) {
	if !extract.IsRepoPkg("foo", "foo") {
		t.Error("exact match should be true")
	}
	if !extract.IsRepoPkg("foo", "foo/bar") {
		t.Error("sub-package should be true")
	}
	if extract.IsRepoPkg("foo", "bar") {
		t.Error("different pkg should be false")
	}
	if extract.IsRepoPkg("foo", "foobar") {
		t.Error("prefix without / should be false")
	}
}

func TestAllExtractorsRegistered(t *testing.T) {
	for _, lang := range []string{"go", "python", "typescript"} {
		fn, ok := extract.Registry[lang]
		if !ok {
			t.Errorf("%s extractor not registered", lang)
			continue
		}
		if fn() == nil {
			t.Errorf("%s extractor factory returned nil", lang)
		}
	}
}
