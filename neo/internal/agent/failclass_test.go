// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"errors"
	"testing"

	"matrix/neo/internal/delegate"
)

// TestNoteFailureClassRecoveryClears pins the supervisor-attribution fix: the
// class the supervisor reads after Chat returns is the failure the turn is
// CURRENTLY stuck on, not any failure that ever happened this turn. A
// recovered deterministic miss (a 404 read of a guessed path followed by a
// successful dispatch) must not convert a later unrelated death into a
// no-respawn deterministic stop — that was the false "permission or limit"
// terminal delivered over a healthy turn (brand-kit incident, 2026-07-12).
func TestNoteFailureClassRecoveryClears(t *testing.T) {
	steps := []struct {
		name  string
		class delegate.FailureClass
		isErr bool
		want  delegate.FailureClass
	}{
		{"clean start", delegate.ClassNone, false, delegate.ClassNone},
		{"deterministic miss records", delegate.ClassDeterministic, true, delegate.ClassDeterministic},
		{"content-level error preserves the recorded wall", delegate.ClassNone, true, delegate.ClassDeterministic},
		{"successful dispatch clears — recovery is progress", delegate.ClassNone, false, delegate.ClassNone},
		{"transient failure records", delegate.ClassTransient, true, delegate.ClassTransient},
		{"a later deterministic failure supersedes", delegate.ClassDeterministic, true, delegate.ClassDeterministic},
		{"success clears again", delegate.ClassNone, false, delegate.ClassNone},
	}
	a := &Agent{turn: newTurn()}
	for _, s := range steps {
		a.noteFailureClass(s.class, s.isErr)
		if got := a.LastFailureClass(); got != s.want {
			t.Fatalf("%s: LastFailureClass = %v, want %v", s.name, got, s.want)
		}
	}
}

// TestEscalateGuidanceDeathIsDeterministicUnproductive pins the pairing the
// honest stop copy depends on: the unproductive-cap escalation returns
// ErrIncomplete, marks the turn's class deterministic (stop-and-ask, no
// respawn), AND records a structured death with reason=unproductive_cap so
// deliverDeterministicStop can say "I need your direction" instead of blaming
// a permission wall that does not exist.
func TestEscalateGuidanceDeathIsDeterministicUnproductive(t *testing.T) {
	a := &Agent{turn: newTurn()}
	err := a.escalateGuidance(4)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("escalateGuidance must wrap ErrIncomplete, got %v", err)
	}
	if got := a.LastFailureClass(); got != delegate.ClassDeterministic {
		t.Fatalf("escalateGuidance must mark ClassDeterministic, got %v", got)
	}
	d, ok := a.LastDeath()
	if !ok {
		t.Fatal("escalateGuidance must record a structured death")
	}
	if d.Reason != DeathReasonUnproductive {
		t.Fatalf("death reason = %q, want %q", d.Reason, DeathReasonUnproductive)
	}
}
