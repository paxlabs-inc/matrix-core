package retrieve

import (
	"path/filepath"
	"strings"
	"testing"

	"matrix/codegraph/extract"
	"matrix/codegraph/model"
)

func fixtureAPI(t *testing.T) *API {
	t.Helper()
	root, err := filepath.Abs("../extract/testdata/prog")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := extract.Build(extract.Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return New(e.Index())
}

func TestSymbolLookup_ExactAndFragmentShape(t *testing.T) {
	a := fixtureAPI(t)
	frag := a.SymbolLookup("Run", "")
	if !strings.Contains(frag, "NODE id=prog.Run kind=func") {
		t.Fatalf("lookup Run missing node:\n%s", frag)
	}
	if !strings.Contains(frag, "loc=greeter.go:") {
		t.Fatalf("lookup Run missing location:\n%s", frag)
	}
	// fragments carry no raw source body
	if strings.Contains(frag, "return out") || strings.Contains(frag, "append(out") {
		t.Fatalf("fragment leaked raw source:\n%s", frag)
	}
}

func TestSymbolLookup_KindFilter(t *testing.T) {
	a := fixtureAPI(t)
	frag := a.SymbolLookup("Greeter", model.KindInterface)
	if !strings.Contains(frag, "NODE id=prog.Greeter kind=interface") {
		t.Fatalf("kind-filtered lookup wrong:\n%s", frag)
	}
	if strings.Contains(frag, "kind=type") {
		t.Fatalf("kind filter leaked non-interface nodes:\n%s", frag)
	}
}

func TestNeighbors_BoundedAndRejectsDeep(t *testing.T) {
	a := fixtureAPI(t)
	frag, err := a.Neighbors("prog.EnglishGreeter", "", model.Both, 1)
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if !strings.Contains(frag, "root=prog.EnglishGreeter") {
		t.Fatalf("neighbors missing root header:\n%s", frag)
	}
	// depth-1 both-directions neighborhood includes the interface it implements
	if !strings.Contains(frag, "prog.Greeter") {
		t.Fatalf("neighbors missing implements target:\n%s", frag)
	}

	if _, err := a.Neighbors("prog.EnglishGreeter", "", model.Both, 4); err == nil {
		t.Fatal("neighbors depth 4 must be rejected")
	}
}

func TestImpact_ReverseClosureExcludesRoot(t *testing.T) {
	a := fixtureAPI(t)
	frag := a.Impact("prog.NewEnglish", 0)
	if !strings.Contains(frag, "tool=impact root=prog.NewEnglish") {
		t.Fatalf("impact missing header:\n%s", frag)
	}
	// Run calls NewEnglish, so changing NewEnglish impacts Run (reverse calls).
	if !strings.Contains(frag, "NODE id=prog.Run") {
		t.Fatalf("impact missing reverse-call dependent Run:\n%s", frag)
	}
	// The changed node itself is excluded from the affected set.
	if strings.Contains(frag, "NODE id=prog.NewEnglish ") {
		t.Fatalf("impact must exclude the root node:\n%s", frag)
	}
	// A fragment never carries raw source.
	if strings.Contains(frag, "return out") || strings.Contains(frag, "g.Prefix") {
		t.Fatalf("impact leaked raw source:\n%s", frag)
	}
}

func TestImpact_InterfaceImplementers(t *testing.T) {
	a := fixtureAPI(t)
	// EnglishGreeter implements Greeter; changing the interface impacts it.
	frag := a.Impact("prog.Greeter", 0)
	if !strings.Contains(frag, "NODE id=prog.EnglishGreeter") {
		t.Fatalf("impact missing reverse-implements dependent:\n%s", frag)
	}
}
