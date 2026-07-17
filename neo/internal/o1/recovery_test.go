// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestRecoveryNetworkAutomaton(t *testing.T) {
	autos := PredefinedRecoveryAutomata()
	net := autos["network"]
	ri := NewRecoveryInstance(net)
	if ri.Current != StateInitial {
		t.Fatal("should start at initial")
	}
	// Probe → probing
	if err := ri.AdvanceWhen(TransitionProbe, map[string]bool{"network_available": true}); err != nil {
		t.Fatalf("probe should succeed: %v", err)
	}
	if ri.Current != StateProbing {
		t.Fatalf("expected probing, got %s", ri.Current)
	}
	// Retry → initial (retry loop)
	if err := ri.AdvanceWhen(TransitionRetry, map[string]bool{"backoff_elapsed": true}); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if ri.Current != StateInitial {
		t.Fatalf("expected initial after retry, got %s", ri.Current)
	}
}

func TestRecoveryGuardIsEnforced(t *testing.T) {
	auto := PredefinedRecoveryAutomata()["network"]
	ri := NewRecoveryInstance(auto)
	if err := ri.Advance(TransitionProbe); err == nil {
		t.Fatal("guarded transition must not advance without authoritative facts")
	}
	if ri.Current != StateInitial || ri.Attempts != 0 {
		t.Fatal("failed guard must preserve recovery state")
	}
}

func TestPredefinedRecoveryAutomataValidate(t *testing.T) {
	for name, auto := range PredefinedRecoveryAutomata() {
		if err := ValidateRecoveryAutomaton(auto); err != nil {
			t.Fatalf("%s automaton invalid: %v", name, err)
		}
	}
}

func TestRecoveryBoundExhaustion(t *testing.T) {
	auto := RecoveryAutomaton{
		Operation: "test",
		Initial:   StateInitial,
		Bound:     2,
		Transitions: []RecoveryTransition{
			{From: StateInitial, To: StateProbing, Kind: TransitionProbe},
			{From: StateProbing, To: StateInitial, Kind: TransitionRetry},
		},
	}
	ri := NewRecoveryInstance(auto)
	ri.Advance(TransitionProbe) // attempt 1
	ri.Advance(TransitionRetry) // attempt 2
	// Bound exhausted
	err := ri.Advance(TransitionProbe)
	if err == nil {
		t.Fatal("should fail after bound exhausted")
	}
}

func TestRecoveryInvalidTransition(t *testing.T) {
	auto := RecoveryAutomaton{
		Operation: "test",
		Initial:   StateInitial,
		Bound:     5,
		Transitions: []RecoveryTransition{
			{From: StateInitial, To: StateProbing, Kind: TransitionProbe},
		},
	}
	ri := NewRecoveryInstance(auto)
	err := ri.Advance(TransitionRepair)
	if err == nil {
		t.Fatal("invalid transition should fail")
	}
}

func TestRecoveryTerminalState(t *testing.T) {
	auto := RecoveryAutomaton{
		Operation: "test",
		Initial:   StateInitial,
		Bound:     5,
		Transitions: []RecoveryTransition{
			{From: StateInitial, To: StateTerminal, Kind: TransitionStop},
			{From: StateInitial, To: StateComplete, Kind: TransitionProbe},
		},
	}
	ri := NewRecoveryInstance(auto)
	ri.Advance(TransitionStop)
	if !ri.IsTerminal() {
		t.Fatal("terminal state should be terminal")
	}
	ri2 := NewRecoveryInstance(auto)
	ri2.Advance(TransitionProbe)
	if !ri2.IsTerminal() {
		t.Fatal("complete state should be terminal")
	}
}

func TestPredefinedAutomataExist(t *testing.T) {
	autos := PredefinedRecoveryAutomata()
	required := []string{"network", "process", "validation", "conflict"}
	for _, name := range required {
		if _, ok := autos[name]; !ok {
			t.Fatalf("missing predefined automaton: %s", name)
		}
	}
}

func TestSelectRecoveryTransitionNeverBlindRetriesUncertainWrite(t *testing.T) {
	manifest := OperationManifest{
		Operation: "fs__write_file", Effects: EffectWrite,
		Recovery: RecoverySpec{Bound: 3},
	}
	outcome := NewOutcome(manifest.Operation).Fail(LayerTransport, "connection_lost").
		Retryable(true).AllowTransition(TransitionReconcile).Evidence("connection closed").MustBuild()
	got := SelectRecoveryTransition(manifest, outcome, 1)
	if got.Transition != TransitionReconcile || got.Terminal {
		t.Fatalf("uncertain write must reconcile, got %#v", got)
	}
}

func TestSelectRecoveryTransitionRetriesBoundedRead(t *testing.T) {
	manifest := OperationManifest{
		Operation: "fetch__fetch", Effects: EffectReadOnly,
		Recovery: RecoverySpec{Bound: 2},
	}
	outcome := NewOutcome(manifest.Operation).Fail(LayerTransport, "timeout").
		Retryable(true).AllowTransition(TransitionRetry).Evidence("timeout").MustBuild()
	if got := SelectRecoveryTransition(manifest, outcome, 1); got.Transition != TransitionRetry {
		t.Fatalf("transient read should retry, got %#v", got)
	}
	if got := SelectRecoveryTransition(manifest, outcome, 2); !got.Terminal || got.Transition != TransitionStop {
		t.Fatalf("bound exhaustion must stop, got %#v", got)
	}
}
