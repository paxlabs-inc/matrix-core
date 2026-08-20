package mcp

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"centra/protocol/codegraph/extract"
)

func fixtureServer(t *testing.T) *Server {
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

// roundtrip drives the server with newline-delimited requests and returns the
// decoded responses in order.
func roundtrip(t *testing.T, s *Server, reqs ...map[string]any) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, r := range reqs {
		b, _ := json.Marshal(r)
		in.Write(b)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := s.Serve(&in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func callText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", resp)
	}
	isErr, _ := res["isError"].(bool)
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("response missing content: %v", resp)
	}
	first := content[0].(map[string]any)
	return first["text"].(string), isErr
}

func TestInitializeAndToolsList(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != protocolVersion {
		t.Fatalf("bad protocol version: %v", init["protocolVersion"])
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	got := map[string]bool{}
	for _, tt := range tools {
		got[tt.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"symbol_lookup", "neighbors", "impact"} {
		if !got[want] {
			t.Fatalf("tools/list missing %s: %v", want, got)
		}
	}
}

func TestNotificationsAndPingAreSilent(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s,
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ping"},
	)
	if len(resps) != 0 {
		t.Fatalf("notifications/ping must produce no response, got %v", resps)
	}
}

func TestToolsCall_SymbolLookupReturnsFragment(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "symbol_lookup", "arguments": map[string]any{"name": "Run"}},
	})
	text, isErr := callText(t, resps[0])
	if isErr {
		t.Fatalf("symbol_lookup errored: %s", text)
	}
	if !strings.Contains(text, "# FRAGMENT tool=symbol_lookup") || !strings.Contains(text, "NODE id=prog.Run") {
		t.Fatalf("unexpected fragment: %s", text)
	}
	if strings.Contains(text, "return out") {
		t.Fatalf("fragment leaked raw source: %s", text)
	}
}

func TestToolsCall_ImpactReturnsFragment(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "impact", "arguments": map[string]any{"id": "prog.NewEnglish"}},
	})
	text, isErr := callText(t, resps[0])
	if isErr {
		t.Fatalf("impact errored: %s", text)
	}
	if !strings.Contains(text, "NODE id=prog.Run") {
		t.Fatalf("impact fragment missing dependent: %s", text)
	}
}

func TestToolsCall_NeighborsDepthRejected(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "neighbors", "arguments": map[string]any{"id": "prog.EnglishGreeter", "depth": 4}},
	})
	text, isErr := callText(t, resps[0])
	if !isErr {
		t.Fatalf("neighbors depth 4 must be a tool error, got: %s", text)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	s := fixtureServer(t)
	resps := roundtrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "no_such_tool", "arguments": map[string]any{}},
	})
	text, isErr := callText(t, resps[0])
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("expected unknown tool error, got isErr=%v text=%s", isErr, text)
	}
}

func TestToolsCall_DiffOverTwoStores(t *testing.T) {
	root, err := filepath.Abs("../extract/testdata/prog")
	if err != nil {
		t.Fatal(err)
	}
	e, merkle, err := extract.Build(extract.Config{RepoRoot: root, RepoName: "prog", Modules: []string{root}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Write the same store to two dirs so the diff is empty but well-formed,
	// exercising the load+diff+guard path end to end.
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := e.WriteStore(dirA, merkle); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteStore(dirB, merkle); err != nil {
		t.Fatal(err)
	}

	s := New(e.Index())
	resps := roundtrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "diff", "arguments": map[string]any{"a": dirA, "b": dirB}},
	})
	text, isErr := callText(t, resps[0])
	if isErr {
		t.Fatalf("diff errored: %s", text)
	}
	if !strings.Contains(text, "# FRAGMENT tool=diff") {
		t.Fatalf("diff fragment malformed: %s", text)
	}
	for _, want := range []string{"nodes_added=0", "nodes_removed=0", "nodes_changed=0"} {
		if !strings.Contains(text, want) {
			t.Fatalf("identical stores should diff empty (%q): %s", want, text)
		}
	}
	// The result passed the guard by virtue of not being isError.
}

func TestGuardRejectsRawSource(t *testing.T) {
	if err := guardFragment("# FRAGMENT tool=x\nNODE id=a kind=func loc=f.go:1:3\n\treturn out\n"); err == nil {
		t.Fatal("guard must reject a tab-indented source body line")
	}
	if err := guardFragment("func Run() {}\n"); err == nil {
		t.Fatal("guard must reject an unindented source declaration")
	}
	if err := guardFragment("# FRAGMENT tool=x\nNODE id=a kind=func loc=f.go:1:3\n  sig=func Run() error\n"); err != nil {
		t.Fatalf("guard must accept a well-formed fragment: %v", err)
	}
}
