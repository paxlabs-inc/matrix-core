package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cortex"
)

func TestSelfModelRoundTripThroughRealCortex(t *testing.T) {
	cfg := testCfg(t)
	cfg.ContinuousMemory = false
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	structural := StructuralSelf{
		Summary:      "Neo assembles system, transcript, then a trailing memory tail; coding and execution are faculties; core_execute is the value-transfer wall.",
		GraphURI:     "matrix://self-graph/neo",
		Scope:        []string{"neo", "cody", "executor/cmd/mcl-execute", "cortex", "neo/internal/tools"},
		TokenBudget:  800,
		ContextLimit: 131072,
	}
	structuralURI, err := p.WriteStructuralSelf(ctx, structural)
	if err != nil {
		t.Fatalf("WriteStructuralSelf: %v", err)
	}
	if structuralURI == "" {
		t.Fatal("WriteStructuralSelf returned an empty URI")
	}
	failureURI, err := p.WriteFailurePattern(ctx, "I loop when a trailing memory tail is treated as the live request", []string{"matrix://cortex/Event/death#1"})
	if err != nil {
		t.Fatalf("WriteFailurePattern: %v", err)
	}
	if failureURI == "" {
		t.Fatal("WriteFailurePattern returned an empty URI")
	}

	model, err := p.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	if model.Identity != "Neo" || model.Structural.Summary != structural.Summary || model.Structural.GraphURI != structural.GraphURI {
		t.Fatalf("SelfModel structural round trip mismatch: %#v", model)
	}
	if len(model.FailurePatterns) != 1 || model.FailurePatterns[0].Statement == "" || len(model.FailurePatterns[0].DerivedFrom) != 1 {
		t.Fatalf("SelfModel failure-pattern round trip mismatch: %#v", model.FailurePatterns)
	}

	recalled, err := p.Recall(ctx, "", []string{"capability", "belief"}, 8, nil)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(recalled, structuralSelfCapability) || !strings.Contains(recalled, model.FailurePatterns[0].Statement) {
		t.Fatalf("Recall did not surface both self-model facets:\n%s", recalled)
	}

	bundle, err := p.Activate("self-model-roundtrip", "", cortex.Budget{Tokens: 4096})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if bundle == nil {
		t.Fatal("Activate returned nil bundle")
	}
}

func TestGeneratedStructuralSelfLoadsAndPagesKnownSymbol(t *testing.T) {
	cfg := testCfg(t)
	cfg.SelfModelGraph = filepath.Join("..", "..", "..", "graph", "self-model")
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	model, err := p.SelfModel(context.Background())
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	if model.Structural.Summary == "" || len(strings.Fields(model.Structural.Summary)) > 800 {
		t.Fatalf("generated resident summary token count=%d", len(strings.Fields(model.Structural.Summary)))
	}
	fragment, err := p.LookupStructuralSelf("AssertNoValueTransferTools")
	if err != nil {
		t.Fatalf("LookupStructuralSelf: %v", err)
	}
	// The paged lookup returns a COMPACT graph fragment (a located node + a
	// length-capped signature), never raw source (req.2.2). Prove it resolves
	// to the real self-package symbol and stays compact: it must carry the
	// location and be a handful of lines, not a pulled-back source range.
	if !strings.Contains(fragment, "neo/internal/tools/automatrix.go") || !strings.Contains(fragment, "loc=") {
		t.Fatalf("structural fragment missing the located self-package symbol:\n%s", fragment)
	}
	if lines := strings.Count(strings.TrimSpace(fragment), "\n") + 1; lines > 6 {
		t.Fatalf("structural fragment is not compact (%d lines) — raw source may be leaking:\n%s", lines, fragment)
	}
}

// TestStructuralSelfRecordsContextLimit proves the derived self-model records
// the agent's OWN context limit from the live config (self-model task 4.2): this
// is the self-model-grounded limit the decomposition router surfaces at the
// spawn_subagents decision point, so the router's self-knowledge and the
// structural self cannot disagree. Never a magic constant — it is exactly
// cfg.ContextWindowTokens.
func TestStructuralSelfRecordsContextLimit(t *testing.T) {
	cfg := testCfg(t)
	cfg.SelfModelGraph = filepath.Join("..", "..", "..", "graph", "self-model")
	cfg.ContextWindowTokens = 256000
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if _, err := p.LoadStructuralSelf(context.Background()); err != nil {
		t.Fatalf("LoadStructuralSelf: %v", err)
	}
	model, err := p.SelfModel(context.Background())
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	if model.Structural.ContextLimit != cfg.ContextWindowTokens {
		t.Fatalf("structural self must record the real context limit %d; got %d", cfg.ContextWindowTokens, model.Structural.ContextLimit)
	}
}

func TestRecallSelfPagesGraphThroughRecallVerb(t *testing.T) {
	cfg := testCfg(t)
	cfg.SelfModelGraph = filepath.Join("..", "..", "..", "graph", "self-model")
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// A bare "self:" query returns the resident structural self-summary.
	if _, err := p.LoadStructuralSelf(context.Background()); err != nil {
		t.Fatalf("LoadStructuralSelf: %v", err)
	}
	summary, err := p.Recall(context.Background(), "self:", nil, 0, nil)
	if err != nil {
		t.Fatalf("Recall(self:): %v", err)
	}
	if !strings.Contains(summary, "Structural self-summary") {
		t.Fatalf("self: recall did not return the resident summary:\n%s", summary)
	}

	// A "self:<Symbol>" query pages the full graph fragment for a real
	// self-package symbol through the SAME retrieval verb.
	frag, err := p.Recall(context.Background(), "self:AssertNoValueTransferTools", nil, 0, nil)
	if err != nil {
		t.Fatalf("Recall(self:Symbol): %v", err)
	}
	if !strings.Contains(frag, "Self-model fragment") || !strings.Contains(frag, "neo/internal/tools") {
		t.Fatalf("self:Symbol recall did not page a real fragment:\n%s", frag)
	}

	// A miss degrades to a readable in-band message, not a transport error.
	miss, err := p.Recall(context.Background(), "self:NotARealSelfSymbolXYZ", nil, 0, nil)
	if err != nil {
		t.Fatalf("Recall(self:miss) returned an error: %v", err)
	}
	if !strings.Contains(miss, "No self-model fragment matched") {
		t.Fatalf("self: miss did not degrade cleanly:\n%s", miss)
	}
}

func TestWriteStructuralSelfUpdatesSingleArtifact(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	first, err := p.WriteStructuralSelf(ctx, StructuralSelf{Summary: "first"})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	second, err := p.WriteStructuralSelf(ctx, StructuralSelf{Summary: "second"})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if first == second {
		t.Fatalf("update should advance the cortex version: %q", first)
	}
	model, err := p.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	if model.Structural.Summary != "second" {
		t.Fatalf("latest structural summary = %q", model.Structural.Summary)
	}
}
