package model

import "testing"

func TestId_EveryKind(t *testing.T) {
	const (
		mod = "matrix/cortex"
		pkg = "matrix/cortex"
	)
	r := Range{StartLine: 142, EndLine: 210}
	cases := []struct {
		name string
		kind Kind
		recv string
		sym  string
		want string
	}{
		{"repo", KindRepo, "", "matrix", "repo:matrix"},
		{"module", KindModule, "", "cortex", "matrix/cortex"},
		{"package", KindPackage, "", "cortex", "matrix/cortex"},
		{"file", KindFile, "", "snapshot.go", "matrix/cortex/snapshot.go"},
		{"func", KindFunc, "", "NewStore", "matrix/cortex.NewStore"},
		{"method", KindMethod, "Snapshot", "Root", "matrix/cortex.Snapshot.Root"},
		{"type", KindType, "", "Snapshot", "matrix/cortex.Snapshot"},
		{"interface", KindInterface, "", "Persistable", "matrix/cortex.Persistable"},
		{"const", KindConst, "", "MaxDepth", "matrix/cortex.MaxDepth"},
		{"var", KindVar, "", "ErrClosed", "matrix/cortex.ErrClosed"},
		{"field", KindField, "matrix/cortex.Snapshot", "Root", "matrix/cortex.Snapshot#Root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Id(c.kind, mod, pkg, c.recv, c.sym, "cortex/snapshot.go", r)
			if got != c.want {
				t.Fatalf("Id(%s) = %q, want %q", c.kind, got, c.want)
			}
		})
	}
}

func TestDisambiguator_Deterministic(t *testing.T) {
	r := Range{StartLine: 10, EndLine: 20}
	a := Disambiguator("a/b.go", r)
	b := Disambiguator("a/b.go", r)
	if a != b {
		t.Fatalf("disambiguator not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("disambiguator len = %d, want 8", len(a))
	}
	if same := Disambiguator("a/c.go", r); same == a {
		t.Fatalf("distinct files produced identical disambiguator %q", a)
	}
	if same := Disambiguator("a/b.go", Range{11, 20}); same == a {
		t.Fatalf("distinct ranges produced identical disambiguator %q", a)
	}
}

func TestDisambiguate_AppendsSuffix(t *testing.T) {
	r := Range{StartLine: 1, EndLine: 2}
	id := "matrix/cortex.Snapshot"
	got := Disambiguate(id, "cortex/snapshot.go", r)
	want := id + "~" + Disambiguator("cortex/snapshot.go", r)
	if got != want {
		t.Fatalf("Disambiguate = %q, want %q", got, want)
	}
}
