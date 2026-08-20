// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"testing"

	"centra/agents/neo/internal/config"
)

// TestAutomatrix_IdleSuppressesOutput verifies the core invariant: when the
// agent's final answer is the AUTOMATRIX_IDLE sentinel on an Automatrix wake,
// the turn is SUPPRESSED — a.out.Say is NOT called, so AUTOMATRIX_IDLE is never
// surfaced to the user. The model emits this sentinel after reviewing its
// pending non-financial opportunities on a Chronos Automatrix wake and finding
// nothing worth doing right now.
func TestAutomatrix_IdleSuppressesOutput(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	a.finishTurn(context.Background(), AutomatrixIdle, nil, nil, AutomatrixWakeMessage, false)

	if len(rep.said) != 0 {
		t.Fatalf("AUTOMATRIX_IDLE must be suppressed (never surfaced); Say was called %d time(s): %v", len(rep.said), rep.said)
	}
}

// TestAutomatrix_NonIdleDelivers verifies that anything other than the
// AUTOMATRIX_IDLE sentinel is delivered normally: a.out.Say IS called with the
// answer. An Automatrix wake that completes a worthwhile opportunity produces
// real content, which must reach the user.
func TestAutomatrix_NonIdleDelivers(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	answer := "I drafted the quarterly update doc you mentioned — it's ready for your review."
	a.finishTurn(context.Background(), answer, nil, nil, AutomatrixWakeMessage, false)

	if len(rep.said) != 1 {
		t.Fatalf("non-idle answer must be delivered exactly once; got %d Say calls", len(rep.said))
	}
	if rep.said[0] != answer {
		t.Errorf("delivered text must match the answer; got %q", rep.said[0])
	}
}

// TestAutomatrix_IdleWithWhitespaceSuppresses ensures the sentinel match trims
// surrounding whitespace — a model may emit " AUTOMATRIX_IDLE\n" and the
// suppression must still fire.
func TestAutomatrix_IdleWithWhitespaceSuppresses(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	a.finishTurn(context.Background(), "  AUTOMATRIX_IDLE\n", nil, nil, AutomatrixWakeMessage, false)

	if len(rep.said) != 0 {
		t.Fatalf("AUTOMATRIX_IDLE with whitespace must still be suppressed; Say called %d time(s)", len(rep.said))
	}
}

// TestAutomatrix_IdleNotSuppressedOnNormalTurn ensures the suppression is
// specific to an Automatrix wake — a normal turn (not an Automatrix wake) that
// happens to mention "automatrix" in the answer is still delivered.
func TestAutomatrix_IdleNotSuppressedOnNormalTurn(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	// A normal user message (not an Automatrix wake) with a non-sentinel answer
	// must be delivered.
	answer := "I've enabled Automatrix for your account."
	a.finishTurn(context.Background(), answer, nil, nil, "turn on proactive tasks", false)

	if len(rep.said) != 1 || rep.said[0] != answer {
		t.Errorf("non-sentinel answer on a normal turn must be delivered; got %v", rep.said)
	}
}

// TestIsAutomatrixWake verifies the detection of a Chronos Automatrix
// wake_message: the input must contain the AUTOMATRIX marker.
func TestIsAutomatrixWake(t *testing.T) {
	if !isAutomatrixWake(AutomatrixWakeMessage) {
		t.Error("the canonical wake_message must be detected as an Automatrix wake")
	}
	if !isAutomatrixWake("AUTOMATRIX: review pending opportunities") {
		t.Error("a message containing the AUTOMATRIX marker must be detected")
	}
	if isAutomatrixWake("what's the weather?") {
		t.Error("a normal user message must NOT be detected as an Automatrix wake")
	}
	if isAutomatrixWake("") {
		t.Error("empty input must not be an Automatrix wake")
	}
}

// TestShouldSuppressAutomatrix pins the sentinel match: only the exact
// AUTOMATRIX_IDLE token (after trimming) triggers suppression.
func TestShouldSuppressAutomatrix(t *testing.T) {
	if !shouldSuppressAutomatrix(AutomatrixIdle) {
		t.Error("exact AUTOMATRIX_IDLE must suppress")
	}
	if !shouldSuppressAutomatrix(" AUTOMATRIX_IDLE ") {
		t.Error("whitespace-padded AUTOMATRIX_IDLE must suppress")
	}
	if shouldSuppressAutomatrix("AUTOMATRIX_IDLE and something else") {
		t.Error("a message containing but not equal to AUTOMATRIX_IDLE must NOT suppress")
	}
	if shouldSuppressAutomatrix("automatrix_idle") {
		t.Error("case must matter — lowercase must NOT suppress")
	}
	if shouldSuppressAutomatrix("") {
		t.Error("empty answer must not suppress")
	}
}

// TestAutomatrixSentinelsAreDistinct ensures the wake marker and the idle
// sentinel are different strings (a wake marker must not BE the suppression
// sentinel). The wake_message itself may mention AUTOMATRIX_IDLE (it instructs
// the agent to reply with it), but the MARKER used to detect a wake is distinct
// from the sentinel, so a wake input is never mistaken for a suppression
// answer.
func TestAutomatrixSentinelsAreDistinct(t *testing.T) {
	if AutomatrixWakeMarker == AutomatrixIdle {
		t.Fatal("wake marker and idle sentinel must be distinct strings")
	}
	// The marker used to detect an Automatrix wake must NOT itself be the
	// suppression sentinel — otherwise the wake input would match the
	// suppression check. The marker is "AUTOMATRIX"; the sentinel is
	// "AUTOMATRIX_IDLE" — distinct.
	if shouldSuppressAutomatrix(AutomatrixWakeMarker) {
		t.Fatal("the wake marker must not trigger suppression (it is not the sentinel)")
	}
}

// TestAutomatrixWakeMessageMatchesChronos pins the Neo-side wake_message to the
// canonical text shared with the Chronos automatrix package (byte-identity).
// The marker and idle sentinel must both be embedded so the wake is detectable
// and the suppression contract is self-describing.
func TestAutomatrixWakeMessageMatchesChronos(t *testing.T) {
	const canonical = "AUTOMATRIX: you have idle time. Review your pending non-financial opportunities and, if a worthwhile one exists, pick ONE and complete it end-to-end, then notify the user with the result. If nothing is worth doing right now, reply with exactly AUTOMATRIX_IDLE."
	if AutomatrixWakeMessage != canonical {
		t.Fatalf("AutomatrixWakeMessage must stay byte-identical to the Chronos WakeMessage; got %q", AutomatrixWakeMessage)
	}
	if !isAutomatrixWake(AutomatrixWakeMessage) {
		t.Error("the canonical wake_message must carry the AUTOMATRIX marker")
	}
}
