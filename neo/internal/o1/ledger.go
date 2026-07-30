// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SnapshotJSON returns a consistent durable representation of the ledger.
func (l *RunLedger) SnapshotJSON() ([]byte, error) {
	if l == nil {
		return nil, fmt.Errorf("nil run ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return json.Marshal(l)
}

func RestoreRunLedger(snapshot []byte) (*RunLedger, error) {
	var ledger RunLedger
	if err := json.Unmarshal(snapshot, &ledger); err != nil {
		return nil, fmt.Errorf("decode run ledger: %w", err)
	}
	if ledger.RunID == "" || ledger.ContractID == "" || ledger.ContractVer == 0 {
		return nil, fmt.Errorf("run ledger identity is incomplete")
	}
	if ledger.Satisfied == nil {
		ledger.Satisfied = map[string]bool{}
	}
	if ledger.CriterionProof == nil {
		ledger.CriterionProof = map[string]string{}
	}
	if ledger.Recovery == nil {
		ledger.Recovery = map[string]RecoveryRecord{}
	}
	return &ledger, nil
}

// RunLedger is the authoritative state ledger for a single run.
// Per O1 req.7 ac.1: persists contract version, operation attempts, observed
// pre/post state, evidence, effects, open hazards, satisfied criteria,
// recovery state, and terminal proof under stable run/operation ids.
type RunLedger struct {
	mu                sync.Mutex
	RunID             string                    `json:"run_id"`
	ContractID        string                    `json:"contract_id"`
	ContractVer       uint64                    `json:"contract_version"`
	Attempts          []AttemptRecord           `json:"attempts"`
	Satisfied         map[string]bool           `json:"satisfied_criteria"`
	CriterionProof    map[string]string         `json:"criterion_evidence"`
	Effects           []EffectRecord            `json:"effects"`
	EffectsReconciled bool                      `json:"effects_reconciled"`
	Recovery          map[string]RecoveryRecord `json:"recovery_state"`
	OpenHazards       []string                  `json:"open_hazards"`
	TerminalProof     *TerminalProof            `json:"terminal_proof,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	LastModified      time.Time                 `json:"last_modified"`
}

// AttemptRecord logs one operation attempt with its pre/post state and outcome.
type AttemptRecord struct {
	Operation string           `json:"operation"`
	AttemptID string           `json:"attempt_id"`
	PreState  string           `json:"pre_state"`
	PostState string           `json:"post_state"`
	Outcome   OperationOutcome `json:"outcome"`
	Evidence  string           `json:"evidence"`
	Timestamp time.Time        `json:"timestamp"`
}

// EffectRecord captures a state-changing effect for reconciliation.
type EffectRecord struct {
	Operation  string    `json:"operation"`
	Effect     string    `json:"effect"`
	Reversible bool      `json:"reversible"`
	Reconciled bool      `json:"reconciled"`
	Timestamp  time.Time `json:"timestamp"`
}

type RecoveryRecord struct {
	Operation  string         `json:"operation"`
	Transition TransitionKind `json:"transition"`
	Attempts   int            `json:"attempts"`
	Terminal   bool           `json:"terminal"`
	Reason     string         `json:"reason"`
}

// TerminalProof captures the final run result.
type TerminalProof struct {
	Success   bool     `json:"success"`
	Summary   string   `json:"summary"`
	Evidence  []string `json:"evidence"`
	ProofHash string   `json:"proof_hash"`
}

// EvidenceSnapshot returns a detached, consistent copy of the ledger evidence
// that is safe to hand to background memory consolidation.
func (l *RunLedger) EvidenceSnapshot() ([]AttemptRecord, *TerminalProof) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	attempts := append([]AttemptRecord(nil), l.Attempts...)
	if l.TerminalProof == nil {
		return attempts, nil
	}
	proof := *l.TerminalProof
	proof.Evidence = append([]string(nil), l.TerminalProof.Evidence...)
	return attempts, &proof
}

// NewRunLedger creates a fresh ledger for a run.
func NewRunLedger(runID, contractID string, contractVer uint64) *RunLedger {
	now := time.Now()
	return &RunLedger{
		RunID:          runID,
		ContractID:     contractID,
		ContractVer:    contractVer,
		Satisfied:      make(map[string]bool),
		CriterionProof: make(map[string]string),
		Recovery:       make(map[string]RecoveryRecord),
		CreatedAt:      now,
		LastModified:   now,
	}
}

// RecordAttempt logs an operation attempt and returns whether it represents
// a causal delta (new state, not a repetition of unchanged state).
// Per O1 req.7 ac.2: progress requires at least one causal delta.
// Per O1 req.7 ac.4: repeating from the same state with the same evidence
// and no allowed transition is rejected.
func (l *RunLedger) RecordAttempt(a AttemptRecord) (causalDelta bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a.Timestamp = time.Now()
	if a.AttemptID == "" {
		a.AttemptID = fmt.Sprintf("att_%d_%s", len(l.Attempts)+1, a.Operation)
	}
	// Check for unchanged-state repetition (req.7 ac.4)
	for _, prev := range l.Attempts {
		if prev.Operation == a.Operation && prev.PreState == a.PreState &&
			prev.PostState == a.PostState && prev.Evidence == a.Evidence &&
			prev.Outcome.FailureLayer == a.Outcome.FailureLayer &&
			!a.Outcome.Retryable {
			return false, fmt.Errorf("rejection: %s attempted from identical state with identical evidence and no recovery transition", a.Operation)
		}
	}
	newFailureEvidence := false
	if !a.Outcome.Success && a.Outcome.FailureClass != "" {
		newFailureEvidence = true
		for i := len(l.Attempts) - 1; i >= 0; i-- {
			prev := l.Attempts[i]
			if prev.Operation == a.Operation && prev.PreState == a.PreState {
				newFailureEvidence = prev.Outcome.FailureClass != a.Outcome.FailureClass
				break
			}
		}
	}
	// Determine causal delta
	causalDelta = a.PostState != a.PreState ||
		a.Outcome.Success ||
		len(a.Outcome.EffectsObserved) > 0 ||
		newFailureEvidence
	l.Attempts = append(l.Attempts, a)
	l.LastModified = a.Timestamp
	return causalDelta, nil
}

func (l *RunLedger) RecordRecovery(record RecoveryRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if record.Operation == "" {
		return
	}
	l.Recovery[record.Operation] = record
	l.LastModified = time.Now()
}

// CloseCriterion marks an acceptance criterion as satisfied.
func (l *RunLedger) CloseCriterion(criterionID, evidence string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if criterionID == "" || evidence == "" {
		return
	}
	l.Satisfied[criterionID] = true
	l.CriterionProof[criterionID] = evidence
	l.LastModified = time.Now()
}

// IsCriterionClosed checks whether a criterion has been closed.
func (l *RunLedger) IsCriterionClosed(criterionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Satisfied[criterionID]
}

// RecordEffect logs a state-changing effect for reconciliation.
func (l *RunLedger) RecordEffect(op, effect string, reversible bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Effects = append(l.Effects, EffectRecord{
		Operation:  op,
		Effect:     effect,
		Reversible: reversible,
		Timestamp:  time.Now(),
	})
	l.LastModified = time.Now()
	l.EffectsReconciled = false
}

// ReconcileEffect marks a recorded effect reconciled from authoritative
// postcondition evidence. Unknown operations/effects fail closed.
func (l *RunLedger) ReconcileEffect(op, effect string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	found := false
	for i := range l.Effects {
		if l.Effects[i].Operation == op && l.Effects[i].Effect == effect {
			l.Effects[i].Reconciled = true
			found = true
		}
	}
	if !found {
		return fmt.Errorf("cannot reconcile unknown effect %s/%s", op, effect)
	}
	l.EffectsReconciled = true
	for _, recorded := range l.Effects {
		if !recorded.Reconciled {
			l.EffectsReconciled = false
			break
		}
	}
	l.LastModified = time.Now()
	return nil
}

func (l *RunLedger) HasUnreconciledEffect(op, effect string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, recorded := range l.Effects {
		if recorded.Operation == op && recorded.Effect == effect && !recorded.Reconciled {
			return true
		}
	}
	return false
}

// AddHazard records an open hazard that must be closed before completion.
func (l *RunLedger) AddHazard(hazardID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.OpenHazards {
		if h == hazardID {
			return
		}
	}
	l.OpenHazards = append(l.OpenHazards, hazardID)
	l.LastModified = time.Now()
}

// CloseHazard removes a resolved hazard.
func (l *RunLedger) CloseHazard(hazardID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, h := range l.OpenHazards {
		if h == hazardID {
			l.OpenHazards = append(l.OpenHazards[:i], l.OpenHazards[i+1:]...)
			break
		}
	}
	l.LastModified = time.Now()
}

// SetTerminal records the final proof and returns an error if the run
// cannot complete (unclosed hazards, unsatisfied criteria, un-reconciled effects).
func (l *RunLedger) SetTerminal(proof TerminalProof) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.OpenHazards) > 0 {
		return fmt.Errorf("cannot complete: %d open hazards remain", len(l.OpenHazards))
	}
	if proof.Success {
		if len(l.Satisfied) == 0 {
			return fmt.Errorf("cannot complete: no acceptance criteria have verified evidence")
		}
		for criterionID := range l.Satisfied {
			if l.CriterionProof[criterionID] == "" {
				return fmt.Errorf("cannot complete: criterion %s has no evidence", criterionID)
			}
		}
		if len(l.Effects) > 0 && !l.EffectsReconciled {
			return fmt.Errorf("cannot complete: effects are not reconciled")
		}
		if len(proof.Evidence) == 0 {
			return fmt.Errorf("cannot complete: terminal proof has no evidence")
		}
	}
	proof.ProofHash = ContentHash([]byte(fmt.Sprintf("%s:%v:%v", l.RunID, proof.Success, proof.Evidence)))
	l.TerminalProof = &proof
	l.LastModified = time.Now()
	return nil
}

// AttemptCount returns the total number of recorded attempts.
func (l *RunLedger) AttemptCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Attempts)
}

// SuccessfulAttempt returns the authoritative prior success for the same
// operation and expected pre-state. It is the idempotency join used before a
// duplicate state-changing dispatch.
func (l *RunLedger) SuccessfulAttempt(operation, preState string) (AttemptRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.Attempts) - 1; i >= 0; i-- {
		attempt := l.Attempts[i]
		if attempt.Operation == operation && attempt.PreState == preState && attempt.Outcome.Success {
			return attempt, true
		}
	}
	return AttemptRecord{}, false
}

func (l *RunLedger) HasSuccessfulOperation(prefixes ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, attempt := range l.Attempts {
		if !attempt.Outcome.Success {
			continue
		}
		for _, prefix := range prefixes {
			if attempt.Operation == prefix || strings.HasPrefix(attempt.Operation, prefix) {
				return true
			}
		}
	}
	return false
}

// CausalDeltas returns the count of attempts that represented actual progress.
func (l *RunLedger) CausalDeltas() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, a := range l.Attempts {
		if a.PostState != a.PreState || a.Outcome.Success || len(a.Outcome.EffectsObserved) > 0 {
			count++
		}
	}
	return count
}
