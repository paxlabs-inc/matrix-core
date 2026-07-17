// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestNewRunLedger(t *testing.T) {
	l := NewRunLedger("run_1", "contract_1", 1)
	if l.RunID != "run_1" || l.ContractVer != 1 || len(l.Attempts) != 0 {
		t.Fatal("ledger not initialized correctly")
	}
}

func TestRecordAttemptCausalDelta(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	delta, err := l.RecordAttempt(AttemptRecord{
		Operation: "fs__write_file",
		PreState:  "file_missing",
		PostState: "file_exists",
		Outcome:   NewOutcome("fs__write_file").Success().Evidence("created").MustBuild(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !delta {
		t.Fatal("state change should be a causal delta")
	}
	if l.AttemptCount() != 1 {
		t.Fatal("attempt not recorded")
	}
}

func TestRecordAttemptNoCausalDelta(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	// First attempt
	l.RecordAttempt(AttemptRecord{
		Operation: "fetch__fetch",
		PreState:  "url_loaded",
		PostState: "url_loaded",
		Evidence:  "same content",
		Outcome:   NewOutcome("fetch__fetch").Fail(LayerTransport, "timeout").Evidence("same content").MustBuild(),
	})
	// Same state, same evidence, not retryable → rejected
	_, err := l.RecordAttempt(AttemptRecord{
		Operation: "fetch__fetch",
		PreState:  "url_loaded",
		PostState: "url_loaded",
		Evidence:  "same content",
		Outcome:   NewOutcome("fetch__fetch").Fail(LayerTransport, "timeout").Evidence("same content").MustBuild(),
	})
	if err == nil {
		t.Fatal("identical-state repetition should be rejected")
	}
}

func TestCriterionLifecycle(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	l.CloseCriterion("crit_1", "test passed")
	if !l.IsCriterionClosed("crit_1") {
		t.Fatal("criterion should be closed")
	}
	if l.IsCriterionClosed("crit_2") {
		t.Fatal("unclosed criterion should not appear closed")
	}
}

func TestHazardLifecycle(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	l.AddHazard("hz_1")
	l.AddHazard("hz_2")
	if len(l.OpenHazards) != 2 {
		t.Fatal("expected 2 hazards")
	}
	l.CloseHazard("hz_1")
	if len(l.OpenHazards) != 1 {
		t.Fatal("expected 1 hazard after close")
	}
	// SetTerminal should fail with open hazard
	err := l.SetTerminal(TerminalProof{Success: true, Summary: "done"})
	if err == nil {
		t.Fatal("should fail with open hazards")
	}
	l.CloseHazard("hz_2")
	l.CloseCriterion("crit_1", "verified")
	err = l.SetTerminal(TerminalProof{Success: true, Summary: "done", Evidence: []string{"verified"}})
	if err != nil {
		t.Fatalf("should succeed with no hazards: %v", err)
	}
	if l.TerminalProof == nil || l.TerminalProof.ProofHash == "" {
		t.Fatal("terminal proof should have a hash")
	}
}

func TestTerminalRequiresCriteriaEvidenceAndReconciledEffects(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	if err := l.SetTerminal(TerminalProof{Success: true, Evidence: []string{"claimed"}}); err == nil {
		t.Fatal("completion without criterion evidence must fail")
	}
	l.CloseCriterion("crit_1", "real verifier output")
	l.RecordEffect("write", "file:/tmp/result", true)
	if err := l.SetTerminal(TerminalProof{Success: true, Evidence: []string{"verified"}}); err == nil {
		t.Fatal("completion with unreconciled effect must fail")
	}
	if err := l.ReconcileEffect("write", "file:/tmp/result"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetTerminal(TerminalProof{Success: true, Evidence: []string{"verified"}}); err != nil {
		t.Fatalf("fully proved completion should pass: %v", err)
	}
}

func TestFailureLayerAloneIsNotCausalProgress(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	first, err := l.RecordAttempt(AttemptRecord{
		Operation: "fetch", PreState: "unchanged", PostState: "unchanged",
		Outcome: NewOutcome("fetch").Fail(LayerTransport, "timeout").
			Retryable(true).AllowTransition(TransitionRetry).Evidence("timeout").MustBuild(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first stable failure classification is discriminating evidence")
	}
	delta, err := l.RecordAttempt(AttemptRecord{
		Operation: "fetch", PreState: "unchanged", PostState: "unchanged",
		Outcome: NewOutcome("fetch").Fail(LayerTransport, "timeout").
			Retryable(true).AllowTransition(TransitionRetry).Evidence("timeout again").MustBuild(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if delta {
		t.Fatal("same stable failure class with cosmetic prose variation is not progress")
	}
}

func TestSuccessfulAttemptJoinsSameOperationAndPreState(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	outcome := NewOutcome("write").Success().Evidence("hash=abc").MustBuild()
	if _, err := l.RecordAttempt(AttemptRecord{
		Operation: "write", PreState: "args-hash", PostState: "abc",
		Outcome: outcome, Evidence: "written",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := l.SuccessfulAttempt("write", "args-hash")
	if !ok || got.Evidence != "written" {
		t.Fatalf("idempotency join missed authoritative success: %#v %v", got, ok)
	}
	if _, ok := l.SuccessfulAttempt("write", "different-args"); ok {
		t.Fatal("different expected pre-state must not join")
	}
}

func TestRunLedgerSnapshotRestoresCausalState(t *testing.T) {
	l := NewRunLedger("r1", "c1", 2)
	l.CloseCriterion("criterion", "verified")
	snapshot, err := l.SnapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRunLedger(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RunID != "r1" || restored.ContractVer != 2 || restored.CriterionProof["criterion"] != "verified" {
		t.Fatalf("restored ledger lost causal state: %#v", restored)
	}
}

func TestCausalDeltaCount(t *testing.T) {
	l := NewRunLedger("r1", "c1", 1)
	// State change = causal delta
	l.RecordAttempt(AttemptRecord{
		Operation: "op1", PreState: "a", PostState: "b",
		Outcome: NewOutcome("op1").Success().Evidence("ok").MustBuild(),
	})
	// Same pre/post, not success, no effects = not a delta (the attempt is recorded but doesn't count as progress)
	l.RecordAttempt(AttemptRecord{
		Operation: "op2", PreState: "x", PostState: "x",
		Outcome: NewOutcome("op2").Fail(LayerTransport, "timeout").Retryable(true).AllowTransition(TransitionRetry).Evidence("tried").MustBuild(),
	})
	// Success IS a causal delta regardless of state change
	l.RecordAttempt(AttemptRecord{
		Operation: "op3", PreState: "y", PostState: "y",
		Outcome: NewOutcome("op3").Success().Evidence("verified").MustBuild(),
	})
	if l.CausalDeltas() != 2 {
		t.Fatalf("expected 2 causal deltas (state change + success), got %d", l.CausalDeltas())
	}
	if l.AttemptCount() != 3 {
		t.Fatalf("expected 3 attempts, got %d", l.AttemptCount())
	}
}
