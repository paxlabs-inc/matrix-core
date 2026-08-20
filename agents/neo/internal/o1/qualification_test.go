// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"testing"
)

func TestStandardQualificationCorpus(t *testing.T) {
	corpus := StandardQualificationCorpus()
	if len(corpus) < 8 {
		t.Fatalf("expected at least 8 qualification cases, got %d", len(corpus))
	}
	categories := map[string]bool{}
	for _, c := range corpus {
		if c.ID == "" || c.Name == "" || c.Category == "" || c.InitialPrompt == "" {
			t.Fatalf("case %s missing required fields", c.ID)
		}
		categories[c.Category] = true
	}
	required := []string{"code", "browser", "memory", "scheduling", "cancellation"}
	for _, r := range required {
		if !categories[r] {
			t.Fatalf("corpus missing required category: %s", r)
		}
	}
}

func TestQualificationCannotPassFromStaticBoolean(t *testing.T) {
	result := EvaluateQualification([]QualificationCase{{
		ID: "case", Name: "case", Category: "code", InitialPrompt: "do it",
		Passed: true, Evidence: "claimed",
	}})
	if result.AllPassed || result.Passed != 0 {
		t.Fatalf("static pass manufactured qualification: %#v", result)
	}
}

func TestQualificationRequiresCompleteProofManifest(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{{
		CriterionID: "c1", Closed: true, Kinds: []VerifierKind{VerifierSemantic},
		Results: []VerifierResult{{CriterionID: "c1", VerifierKind: VerifierSemantic, Passed: true, Evidence: "checked"}},
	}}}
	proof := graph.GenerateProofManifest("run", "contract", true, true)
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	result := EvaluateQualification([]QualificationCase{{
		ID: "case", Name: "case", Category: "code", InitialPrompt: "do it",
		ProofManifest: string(encoded), Evidence: "real run evidence",
	}})
	if !result.AllPassed || result.Passed != 1 {
		t.Fatalf("complete proof did not qualify: %#v", result)
	}
}

func TestFailureReplayRequiresTypedOutcome(t *testing.T) {
	static := EvaluateFailureReplay([]FailureReplayCase{{
		ID: "fail", ExpectedBehavior: "prevented", Passed: true,
	}})
	if static.AllPassed {
		t.Fatal("static failure replay pass must be ignored")
	}
	outcome := NewOutcome("write").Fail(LayerValidation, "oversized").
		Evidence("payload exceeded bound").MustBuild()
	real := EvaluateFailureReplay([]FailureReplayCase{{
		ID: "fail", ExpectedBehavior: "prevented", Outcome: outcome,
	}})
	if !real.AllPassed {
		t.Fatalf("typed prevented outcome should pass: %#v", real)
	}
}

func TestStandardFailureCorpus(t *testing.T) {
	corpus := StandardFailureCorpus()
	if len(corpus) < 6 {
		t.Fatalf("expected at least 6 failure cases, got %d", len(corpus))
	}
	for _, c := range corpus {
		if c.ID == "" || c.FailureClass == "" || c.ExpectedBehavior == "" {
			t.Fatalf("failure case %s missing fields", c.ID)
		}
		switch c.ExpectedBehavior {
		case "prevented", "typed_recovery":
		default:
			t.Fatalf("failure case %s has invalid expected_behavior: %s", c.ID, c.ExpectedBehavior)
		}
	}
}

func TestExtensionGateDerivesClearanceFromEvidence(t *testing.T) {
	gate := completeExtensionGate(t)
	if !gate.IsClear() {
		t.Fatalf("complete evidence should clear the gate: %v", gate.Unmet())
	}
	if len(gate.Unmet()) != 0 {
		t.Fatal("clear gate should have no unmet requirements")
	}
}

func TestExtensionGateCannotPassFromStaticOrMismatchedEvidence(t *testing.T) {
	gate := completeExtensionGate(t)
	gate.Preflight.Operation = "another_operation"
	gate.Outcomes[0].Operation = "another_operation"
	gate.Conformance.RequiredTools = []string{"another_operation"}
	if gate.IsClear() {
		t.Fatal("evidence for another operation cleared the extension gate")
	}
	unmet := gate.Unmet()
	for _, want := range []string{"preflight", "typed_outcomes", "conformance_evidence"} {
		if !containsString(unmet, want) {
			t.Fatalf("missing %s failure in %v", want, unmet)
		}
	}
}

func completeExtensionGate(t *testing.T) ExtensionGate {
	t.Helper()
	const operation = "fs__write_file"
	graph := VerifierGraph{Verifiers: []CriterionVerifier{{
		CriterionID: "extension", Closed: true, Kinds: []VerifierKind{VerifierArtifact},
		Results: []VerifierResult{{
			CriterionID: "extension", VerifierKind: VerifierArtifact,
			Passed: true, Evidence: "real operation conformance passed",
		}},
	}}}
	proof := graph.GenerateProofManifest("run-extension", "contract-extension", true, true)
	proofJSON, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	return ExtensionGate{
		Manifest: OperationManifest{
			Operation: operation, Description: "write a bounded file",
			InputBounds:   map[string]string{"path": "required", "content": "max_bytes:1024"},
			Preconditions: []string{"tool:" + operation}, Effects: EffectWrite,
			Idempotency: "operation_id", Reversibility: Conditional,
			OutcomeVariants: []string{"success", "typed_failure"},
			Recovery: RecoverySpec{
				Initial: "initial", Allowed: []string{"stop"},
				Terminal: []string{"success", "typed_failure"}, Bound: 1,
			},
			Verifier: "read-after-write hash", ContextPolicy: "structured_result",
		},
		Registry: &Registry{
			Version: "1",
			Hazards: []Hazard{{
				ID: "HZ-EXT-1", Operation: operation, Source: "file boundary",
				Trigger: "oversized content", Evidence: []string{"size check"},
				Class: Preventable, Owner: "filesystem", Outcomes: []string{"typed_failure"},
				Control:  Control{Kind: "payload_bound", Phase: "pre_execution", Ref: "writer"},
				Recovery: Recovery{Initial: "initial", Terminal: []string{"typed_failure"}, Bound: 1},
				Verifier: "size rejection", Status: "closed",
			}},
		},
		Preflight: PreflightResult{OK: true, Operation: operation},
		Outcomes: []OperationOutcome{
			NewOutcome(operation).Success().Evidence("real write completed").MustBuild(),
			NewOutcome(operation).Fail(LayerValidation, "oversized").
				Evidence("real payload rejected").MustBuild(),
		},
		Recovery: RecoveryAutomaton{
			Operation: operation, Initial: StateInitial, Bound: 1,
			Transitions: []RecoveryTransition{{
				From: StateInitial, To: StateTerminal,
				Kind: TransitionStop, Guard: "typed_failure",
			}},
		},
		Conformance: QualificationCase{
			ID: "extension-real-path", Name: "filesystem extension",
			Category: "code", InitialPrompt: "write the file",
			RequiredTools: []string{operation}, ProofManifest: string(proofJSON),
			Evidence: "real filesystem process and read-after-write verifier",
		},
	}
}
