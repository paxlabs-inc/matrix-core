package selfmodel

import (
	"strings"
	"testing"

	"matrix/codegraph/model"
)

func TestDistillIsBoundedAndLookupReturnsCompactFragment(t *testing.T) {
	ix := model.NewIndex()
	ix.AddNode(&model.Node{Id: "matrix/neo/internal/agent.Agent.Chat", Kind: model.KindMethod, Name: "Chat", File: "neo/internal/agent/agent.go", Sig: "func (a *Agent) Chat(ctx context.Context, input string) error"})
	artifact := Distill(ix, "b3:test", []string{"neo/internal/agent"}, 32)
	if len(strings.Fields(artifact.Summary)) > 32 {
		t.Fatalf("summary has %d tokens, budget 32", len(strings.Fields(artifact.Summary)))
	}
	if artifact.Merkle != "b3:test" || len(artifact.Scope) != 1 {
		t.Fatalf("artifact provenance mismatch: %#v", artifact)
	}
	fragment, err := Lookup(ix, "Chat")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !strings.Contains(fragment, "neo/internal/agent/agent.go") || strings.Contains(fragment, "return ") {
		t.Fatalf("lookup was not a compact location fragment:\n%s", fragment)
	}
}
