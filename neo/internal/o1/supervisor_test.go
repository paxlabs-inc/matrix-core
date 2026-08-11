// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestSupervisorCompleteWhenAllCriteriaClosed(t *testing.T) {
	s := &Supervisor{}
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1", Closed: true, Kinds: []VerifierKind{VerifierSemantic},
			Results: []VerifierResult{{CriterionID: "c1", VerifierKind: VerifierSemantic, Passed: true, Evidence: "checked"}}},
	}}
	ledger := NewRunLedger("r1", "c1", 1)
	proof := graph.GenerateProofManifest("r1", "c1", true, true)
	d := s.Evaluate(Compile(CompileInput{Request: "do something"}), ledger, &graph, &proof, nil)
	if d.Action != SupComplete {
		t.Fatalf("expected complete, got %s: %s", d.Action, d.Reason)
	}
	if d.Terminal == nil || *d.Terminal != TermComplete {
		t.Fatal("terminal should be complete")
	}
}

func TestSupervisorRepairOnRepairableOutcome(t *testing.T) {
	s := &Supervisor{}
	o := NewOutcome("fetch__fetch").Fail(LayerTransport, "timeout").
		Retryable(true).AllowTransition(TransitionRepair).Evidence("504").MustBuild()
	d := s.Evaluate(Compile(CompileInput{Request: "fetch data"}), NewRunLedger("r1", "c1", 1), nil, nil, &o)
	if d.Action != SupRepair {
		t.Fatalf("expected repair, got %s", d.Action)
	}
}

func TestSupervisorAskOnAmbiguity(t *testing.T) {
	s := &Supervisor{}
	contract := Compile(CompileInput{})
	d := s.Evaluate(contract, NewRunLedger("r1", "c1", 1), nil, nil, nil)
	if d.Action != SupAskUser {
		t.Fatalf("expected ask_user, got %s", d.Action)
	}
}

func TestSupervisorDoesNotMaskAmbiguityWithGenericRetry(t *testing.T) {
	contract := Compile(CompileInput{})
	outcome := NewOutcome("agent_turn").Fail(LayerApplication, "internal_convergence_failure").
		Retryable(true).AllowTransition(TransitionRetry).Evidence("step budget").MustBuild()
	d := (&Supervisor{}).Evaluate(contract, NewRunLedger("r1", contract.ID, 1), nil, nil, &outcome)
	if d.Action != SupAskUser || d.Reason != "task contract has unresolved material ambiguity" {
		t.Fatalf("ambiguity was masked by retry outcome: %#v", d)
	}
}

func TestSupervisorStopOnTerminalFailure(t *testing.T) {
	s := &Supervisor{}
	o := NewOutcome("deus__pay").Fail(LayerPolicy, "no_leash").
		Evidence("refused").MustBuild() // no transitions → terminal
	d := s.Evaluate(Compile(CompileInput{Request: "pay"}), NewRunLedger("r1", "c1", 1), nil, nil, &o)
	if d.Action != SupStop {
		t.Fatalf("expected stop, got %s", d.Action)
	}
	if d.Terminal == nil {
		t.Fatal("terminal class should be set")
	}
}

func TestSupervisorContinueDefault(t *testing.T) {
	s := &Supervisor{}
	graph := VerifierGraph{Verifiers: []CriterionVerifier{
		{CriterionID: "c1"}, // open
	}}
	contract := Compile(CompileInput{Request: "build something"})
	d := s.Evaluate(contract, NewRunLedger("r1", "c1", 1), &graph, nil, nil)
	if d.Action != SupContinue {
		t.Fatalf("expected continue, got %s", d.Action)
	}
}

func TestSupervisorCannotCompleteWithoutProofManifest(t *testing.T) {
	s := &Supervisor{}
	graph := VerifierGraph{Verifiers: []CriterionVerifier{{CriterionID: "c1", Closed: true}}}
	d := s.Evaluate(Compile(CompileInput{Request: "do something"}), NewRunLedger("r1", "c1", 1), &graph, nil, nil)
	if d.Action == SupComplete {
		t.Fatal("closed booleans without a complete proof manifest must not complete")
	}
}

func TestRenderTerminalMessage(t *testing.T) {
	complete := TermComplete
	msg := RenderTerminalMessage(SupervisorDecision{Terminal: &complete, Reason: "done"}, nil)
	if msg != "Task complete." {
		t.Fatalf("unexpected complete message: %s", msg)
	}
	auth := TermMissingAuth
	msg = RenderTerminalMessage(SupervisorDecision{Terminal: &auth, Reason: "need key"}, nil)
	if msg == "" {
		t.Fatal("auth terminal should have a message")
	}
}
