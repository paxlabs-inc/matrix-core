package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/protocol/codegraph/model"
)

func buildFixture(t *testing.T) *Extractor {
	t.Helper()
	root, err := filepath.Abs("testdata/prog")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := Build(Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return e
}

func hasFwd(ix *model.Index, src string, typ model.EdgeType, dst string) bool {
	for _, d := range ix.Forward(src, typ) {
		if d == dst {
			return true
		}
	}
	return false
}

// Correctness set (req 4.1/4.2/4.3): a fixed set of (symbol, expected fact)
// asserted against the type-resolved extraction.
func TestExtract_CorrectnessSet(t *testing.T) {
	ix := buildFixture(t).Index()

	nodeChecks := []struct {
		id   string
		kind model.Kind
	}{
		{"repo:prog", model.KindRepo},
		{"mod:prog", model.KindModule},
		{"prog", model.KindPackage},
		{"prog/greeter.go", model.KindFile},
		{"prog.Greeter", model.KindInterface},
		{"prog.EnglishGreeter", model.KindType},
		{"prog.EnglishGreeter.Hello", model.KindMethod},
		{"prog.Run", model.KindFunc},
		{"prog.NewEnglish", model.KindFunc},
		{"prog.DefaultPrefix", model.KindConst},
		{"prog.base#Prefix", model.KindField},
	}
	for _, c := range nodeChecks {
		n := ix.Node(c.id)
		if n == nil {
			t.Errorf("missing node %q", c.id)
			continue
		}
		if n.Kind != c.kind {
			t.Errorf("node %q kind = %q, want %q", c.id, n.Kind, c.kind)
		}
	}

	edgeChecks := []struct {
		name string
		src  string
		typ  model.EdgeType
		dst  string
	}{
		{"implements (from checker)", "prog.EnglishGreeter", model.EdgeImplements, "prog.Greeter"},
		{"embeds", "prog.EnglishGreeter", model.EdgeEmbeds, "prog.base"},
		{"resolved call", "prog.Run", model.EdgeCalls, "prog.NewEnglish"},
		{"references (signature type)", "prog.Run", model.EdgeReferences, "prog.Greeter"},
		{"file defines decl", "prog/greeter.go", model.EdgeDefines, "prog.Run"},
		{"type contains method", "prog.EnglishGreeter", model.EdgeContains, "prog.EnglishGreeter.Hello"},
		{"type contains field", "prog.base", model.EdgeContains, "prog.base#Prefix"},
		{"package contains file", "prog", model.EdgeContains, "prog/greeter.go"},
		{"module contains package", "mod:prog", model.EdgeContains, "prog"},
		{"repo contains module", "repo:prog", model.EdgeContains, "mod:prog"},
	}
	for _, c := range edgeChecks {
		if !hasFwd(ix, c.src, c.typ, c.dst) {
			t.Errorf("%s: missing %s --%s--> %s\n  forward %s: %v",
				c.name, c.src, c.typ, c.dst, c.typ, ix.Forward(c.src, c.typ))
		}
	}

	// reverse index is queryable both ways
	found := false
	for _, s := range ix.Reverse("prog.Greeter", model.EdgeImplements) {
		if s == "prog.EnglishGreeter" {
			found = true
		}
	}
	if !found {
		t.Errorf("reverse implements index missing EnglishGreeter -> Greeter")
	}
}

// resolved calls must be to the actual callee, not a name match (req 4.2): the
// call site is recorded.
func TestExtract_CallSiteRecorded(t *testing.T) {
	ix := buildFixture(t).Index()
	if !hasFwd(ix, "prog.Run", model.EdgeCalls, "prog.NewEnglish") {
		t.Fatal("expected Run -> NewEnglish call edge")
	}
	if site := ix.Site("prog.Run", model.EdgeCalls, "prog.NewEnglish"); !strings.HasPrefix(site, "greeter.go:") {
		t.Fatalf("call site = %q, want greeter.go:<line>", site)
	}
}

// Property 1: build determinism (req 1.1/1.2/1.3) — build+serialize twice,
// assert byte-identical, and assert no absolute path leaks into the store.
func TestExtract_BuildDeterministic(t *testing.T) {
	root, _ := filepath.Abs("testdata/prog")
	cfg := Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}}

	write := func(dir string) string {
		e, merkle, err := Build(cfg)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if err := e.WriteStore(dir, merkle); err != nil {
			t.Fatalf("write: %v", err)
		}
		return merkle
	}

	a, b := t.TempDir(), t.TempDir()
	m1, m2 := write(a), write(b)
	if m1 != m2 {
		t.Fatalf("merkle not deterministic: %q vs %q", m1, m2)
	}

	files := listFiles(t, a)
	if len(files) == 0 {
		t.Fatal("no store files written")
	}
	for _, rel := range files {
		ba, err := os.ReadFile(filepath.Join(a, rel))
		if err != nil {
			t.Fatal(err)
		}
		bb, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Fatalf("second build missing %s: %v", rel, err)
		}
		if string(ba) != string(bb) {
			t.Fatalf("%s not byte-identical across builds", rel)
		}
		if strings.Contains(string(ba), root) {
			t.Fatalf("%s leaks absolute repo path %q", rel, root)
		}
	}
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
