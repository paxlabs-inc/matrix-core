// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

// Architect O1 — task 4.3: the integrated local qualification gate. It composes
// the real evidence produced by waves 1-3 and tasks 4.1/4.2 into ONE assertion
// of Property O1-LOCAL (validates requirements 1-12):
//
//	every representative task completes or stops from authoritative typed
//	evidence, with no known preventable error reaching the model and no
//	fake-driven proof.
//
// Composition, all real:
//   - hazard coverage: the live registry classifies every shipped operation;
//   - provider conformance: every configured dialect accepts a well-formed
//     generation and rejects a malformed (reasoning-only) one before execution;
//   - completion: a complete initial request finishes with a verifiable proof
//     manifest through the real turn loop, with no user rescue;
//   - prevention: a preventable operation is rejected as a non-retryable typed
//     outcome on the real dispatch path;
//   - no fakes: the qualification evaluator refuses a static Passed boolean and
//     admits only a genuine, hash-consistent proof manifest.
//
// The broader real-path suites this gate depends on (the full o1 package unit
// suite, the failure-replay corpus, morpheus cold-start detach/reload, and the
// OHLC/epistemic replay determinism suites) are run alongside it by
// `go test ./...`; this test is the single composed property over their seams.

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/o1"
	"centra/agents/neo/internal/tools"
)

func TestO1LocalQualificationGate(t *testing.T) {
	// 1. Hazard coverage: the live registry classifies every shipped operation.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	registry, err := o1.Load(filepath.Join(root, "protocol", "spec", "architect-o1", "hazards.json"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	operations, err := o1.ManifestOperations(filepath.Join(root, "agents", "default.json"))
	if err != nil {
		t.Fatalf("load manifest operations: %v", err)
	}
	if err := registry.Check(operations); err != nil {
		t.Fatalf("registry coverage check failed: %v", err)
	}

	// 2. Provider conformance: every configured dialect accepts a well-formed
	// generation and rejects a malformed (reasoning-only) one before execution.
	for _, dialect := range []o1.ProviderDialect{
		o1.DialectOpenAI, o1.DialectFireworks, o1.DialectMimo,
		o1.DialectDeepSeek, o1.DialectXAI, o1.DialectBaseten, o1.DialectGeneric,
	} {
		conformer := o1.NewProviderConformer(dialect)
		good := &o1.NormalizedGeneration{Content: "here is the result", FinishReason: "stop", Dialect: dialect}
		if err := conformer.Validate(good); err != nil {
			t.Fatalf("dialect %s rejected a well-formed generation: %v", dialect, err)
		}
		bad := &o1.NormalizedGeneration{Content: "", FinishReason: "length", Dialect: dialect}
		if err := conformer.Validate(bad); err == nil {
			t.Fatalf("dialect %s admitted a malformed reasoning-only generation", dialect)
		}
	}

	// 3. Completion: a complete initial request finishes with a verifiable proof
	// manifest through the real turn loop, with no user rescue.
	const answer = "Ready to help — tell me what you'd like to do."
	var (
		mu    sync.Mutex
		calls int
	)
	srv := o1ModelServer(t, []o1StepFrame{{content: answer, finish: "stop"}}, &calls, &mu)
	client := revisionTestClient(t, srv.URL)
	rep := &o1SayReporter{}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: &tools.Manager{}, Reporter: rep})
	if err := a.Chat(context.Background(), "Greet me and say you're ready."); err != nil {
		t.Fatalf("completion chat: %v", err)
	}
	proof := assertO1Complete(t, a, answer, rep)
	completion := qualificationCaseFrom(t, "gate-completion", "conversational",
		"complete initial request", "Greet me and say you're ready.", nil, proof)
	if r := o1.EvaluateQualification([]o1.QualificationCase{completion}); !r.AllPassed {
		t.Fatalf("genuine completion proof must clear qualification: %#v", r)
	}

	// 4. Prevention: a preventable operation is rejected as a non-retryable
	// typed outcome on the real dispatch path.
	var (
		pmu    sync.Mutex
		pcalls int
	)
	psrv := o1ModelServer(t, []o1StepFrame{
		{toolCall: o1ToolCall("x1", "make_widget", map[string]any{}), finish: "tool_calls"},
		{content: "That isn't something I can do here.", finish: "stop"},
	}, &pcalls, &pmu)
	pa := New(Options{Config: o1FailureCfg(), Main: revisionTestClient(t, psrv.URL),
		Cheap: revisionTestClient(t, psrv.URL), Tools: &tools.Manager{}})
	if err := pa.Chat(context.Background(), "Make a widget for me."); err != nil {
		t.Fatalf("prevention chat: %v", err)
	}
	prevented := ledgerOutcome(t, pa, "make_widget")
	if prevented.Success || prevented.Retryable || prevented.FailureLayer != o1.LayerInvocation {
		t.Fatalf("preventable operation must be a non-retryable invocation outcome: %+v", prevented)
	}
	if r := o1.EvaluateFailureReplay([]o1.FailureReplayCase{{
		ID: "gate-prevention", ExpectedBehavior: "prevented", Outcome: prevented,
	}}); !r.AllPassed {
		t.Fatalf("preventable outcome must clear the failure evaluator: %#v", r)
	}

	// 5. No fakes: a static Passed boolean cannot manufacture qualification.
	if r := o1.EvaluateQualification([]o1.QualificationCase{{
		ID: "gate-fake", Category: "conversational", Name: "static claim",
		InitialPrompt: "x", Passed: true, Evidence: "claimed",
	}}); r.AllPassed || r.Passed != 0 {
		t.Fatalf("a static Passed boolean must never qualify: %#v", r)
	}
}
