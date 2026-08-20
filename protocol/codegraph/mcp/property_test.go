package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/protocol/codegraph/extract"
	"centra/protocol/codegraph/model"
)

// invoke drives one tool through the real guarded boundary (callTool runs
// guardFragment) and returns the result text plus whether it was a tool error.
func invoke(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	ab, _ := json.Marshal(args)
	pb, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(ab)})
	resp := s.callTool(nil, pb)
	res := resp["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	txt := res["content"].([]map[string]any)[0]["text"].(string)
	return txt, isErr
}

// TestProperty_FragmentsNeverContainRawSource is Property 4 (fragments-not-
// source). For every node in the sample graph it exercises symbol_lookup,
// impact, and neighbors across all directions, depths (1..3), and edge types,
// and asserts every non-error result is a well-formed .kvx fragment that never
// contains a raw source range from the fixture's bodies. Validates 6.2, 6.5.
func TestProperty_FragmentsNeverContainRawSource(t *testing.T) {
	root, err := filepath.Abs("../extract/testdata/prog")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := extract.Build(extract.Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ix := e.Index()
	s := New(ix)

	// Raw-source substrings drawn from the fixture's function bodies. None may
	// ever surface in a fragment (signatures and doc comments are allowed; body
	// statements are not).
	src, err := os.ReadFile(filepath.Join(root, "greeter.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		`g.Prefix + "Hello, "`,
		"append(out, g.Hello(n))",
		"make([]string, 0, len(names))",
		"EnglishGreeter{base: base{Prefix: DefaultPrefix}}",
		"for _, n := range names",
	}
	for _, f := range forbidden {
		if !strings.Contains(string(src), f) {
			t.Fatalf("test bug: forbidden token %q not present in fixture source", f)
		}
	}

	check := func(where, text string) {
		if err := guardFragment(text); err != nil {
			t.Fatalf("%s: result is not a well-formed fragment: %v\n%s", where, err, text)
		}
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Fatalf("%s: fragment leaked raw source %q:\n%s", where, f, text)
			}
		}
	}

	dirs := []string{"out", "in", "both"}
	edges := append([]model.EdgeType{""}, model.EdgeTypes...)

	nodes := ix.Nodes()
	if len(nodes) == 0 {
		t.Fatal("empty sample graph")
	}
	for _, n := range nodes {
		if txt, isErr := invoke(t, s, "symbol_lookup", map[string]any{"name": n.Name}); !isErr {
			check("symbol_lookup "+n.Name, txt)
		}
		if txt, isErr := invoke(t, s, "impact", map[string]any{"id": n.Id}); !isErr {
			check("impact "+n.Id, txt)
		}
		for depth := 1; depth <= 3; depth++ {
			for _, dir := range dirs {
				for _, edge := range edges {
					txt, isErr := invoke(t, s, "neighbors", map[string]any{
						"id": n.Id, "edge_type": string(edge), "direction": dir, "depth": depth,
					})
					if isErr {
						continue
					}
					check("neighbors "+n.Id, txt)
				}
			}
		}
	}
}
