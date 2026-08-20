// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestCompileVerifiersFromCriteria(t *testing.T) {
	criteria := []Criterion{
		{ID: "c1", Statement: "The requested file output is created and matches semantics."},
		{ID: "c2", Statement: "All framework validation passes."},
	}
	graph := CompileVerifiers(criteria)
	if len(graph.Verifiers) != 2 {
		t.Fatalf("expected 2 verifiers, got %d", len(graph.Verifiers))
	}
	v1 := graph.Verifiers[0]
	if v1.CriterionID != "c1" {
		t.Fatal("verifier should match criterion ID")
	}
	hasArtifact := false
	for _, k := range v1.Kinds {
		if k == VerifierArtifact {
			hasArtifact = true
		}
	}
	if !hasArtifact {
		t.Fatal("file-output criterion should have artifact verifier")
	}
}

func TestRecordResultClosesCriterion(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Kinds: []VerifierKind{VerifierArtifact, VerifierSemantic}},
	}}
	graph.RecordResult(VerifierResult{CriterionID: "c1", VerifierKind: VerifierArtifact, Passed: true, Evidence: "file exists"})
	if graph.AllClosed() {
		t.Fatal("should not be all closed with one verifier pending")
	}
	graph.RecordResult(VerifierResult{CriterionID: "c1", VerifierKind: VerifierSemantic, Passed: true, Evidence: "matches request"})
	if !graph.AllClosed() {
		t.Fatal("should be all closed when all verifiers pass")
	}
}

func TestRecordResultFailsStaysOpen(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Kinds: []VerifierKind{VerifierArtifact}},
	}}
	graph.RecordResult(VerifierResult{CriterionID: "c1", VerifierKind: VerifierArtifact, Passed: false, Evidence: "missing"})
	if graph.AllClosed() {
		t.Fatal("failed verifier should not close criterion")
	}
}

func TestOpenCriteria(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Kinds: []VerifierKind{VerifierArtifact}, Closed: true},
		{CriterionID: "c2", Kinds: []VerifierKind{VerifierSemantic}},
	}}
	open := graph.OpenCriteria()
	if len(open) != 1 || open[0] != "c2" {
		t.Fatalf("expected [c2], got %v", open)
	}
}

func TestProofManifestCompletion(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Kinds: []VerifierKind{VerifierArtifact}, Closed: true,
			Results: []VerifierResult{{CriterionID: "c1", VerifierKind: VerifierArtifact, Passed: true, Evidence: "ok"}}},
	}}
	pm := graph.GenerateProofManifest("r1", "c1", true, true)
	if !pm.IsComplete() {
		t.Fatal("all criteria closed, effects reconciled, delivered → complete")
	}
	if pm.ProofHash == "" {
		t.Fatal("proof manifest should have hash")
	}
}

func TestProofManifestIncomplete(t *testing.T) {
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Kinds: []VerifierKind{VerifierArtifact}},
	}}
	pm := graph.GenerateProofManifest("r1", "c1", false, false)
	if pm.IsComplete() {
		t.Fatal("open criteria + un-reconciled + not delivered → incomplete")
	}
	unmet := pm.UnmetRequirements()
	if len(unmet) != 3 {
		t.Fatalf("expected 3 unmet, got %d: %v", len(unmet), unmet)
	}
}

func TestContainsAnyInference(t *testing.T) {
	// artifact verifier for file-related criteria
	kinds := inferVerifierKinds(Criterion{Statement: "The file output is correct and delivered to the user."})
	hasArtifact := false
	hasDelivery := false
	for _, k := range kinds {
		if k == VerifierArtifact {
			hasArtifact = true
		}
		if k == VerifierDelivery {
			hasDelivery = true
		}
	}
	if !hasArtifact || !hasDelivery {
		t.Fatalf("expected artifact+delivery, got %v", kinds)
	}
}
