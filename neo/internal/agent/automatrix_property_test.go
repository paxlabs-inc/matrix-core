// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"testing"

	"matrix/neo/internal/config"
)

// Property 3 (agent-owned clauses) — wakes are idle-only and self-identifying.
//
// Task 7.3 asks for a PROPERTY-level proof tying the wake clauses together as a
// single coherent property rather than a grab-bag of single-input cases. The
// agent package owns two of Property 3's clauses:
//
//   - the AUTOMATRIX marker is DETECTED (isAutomatrixWake), distinguishing a
//     Chronos proactive wake from a normal user message (req 4.2, the detection
//     prerequisite for the idle clause);
//   - an AUTOMATRIX_IDLE answer on such a wake SUPPRESSES the turn — Say is
//     never called, exactly like HEARTBEAT_OK (req 4.6).
//
// This file proves the *combined* suppression invariant as a quantified sweep
// over the REAL finishTurn (the production terminal path) with no doubles other
// than a Reporter that records Say calls. The invariant is the precise iff:
//
//	Say is withheld  ⇔  (input is an AUTOMATRIX wake) ∧ (answer == AUTOMATRIX_IDLE, trimmed)
//
// Any other combination — a non-wake turn, a real-content answer, or the idle
// sentinel arriving outside an Automatrix wake — is delivered verbatim exactly
// once. This is the single coherent statement of the agent side of Property 3.

// suppressed reports the production suppression decision the way finishTurn
// computes it, so the property asserts the REAL predicate rather than a
// re-derivation.
func automatrixSuppressed(input, answer string) bool {
	return isAutomatrixWake(input) && shouldSuppressAutomatrix(answer)
}

// TestProperty3_Agent_IdleSuppressionInvariant sweeps the cross-product of
// representative wake/non-wake inputs against idle/real/near-miss answers and
// asserts, for EVERY pair, that the REAL finishTurn surfaces output iff the
// turn is not a suppressed idle wake. This is the agent-owned half of
// Property 3 (marker-detect + idle-suppress) proven as one invariant.
//
// Validates: Requirements 4.6 (idle suppression), 4.2 (marker detection).
func TestProperty3_Agent_IdleSuppressionInvariant(t *testing.T) {
	// Inputs: a mix that exercises marker detection. Wakes carry the AUTOMATRIX
	// marker; non-wakes do not (even when they mention the feature by name).
	inputs := []struct {
		name string
		text string
		wake bool
	}{
		{"canonical-wake", AutomatrixWakeMessage, true},
		{"bare-marker-wake", "AUTOMATRIX: review pending opportunities", true},
		{"marker-midsentence", "context\nAUTOMATRIX you have idle time", true},
		{"normal-user-turn", "what's the weather?", false},
		{"normal-mentions-feature", "turn on proactive tasks for me", false},
		{"empty-input", "", false},
	}
	// Answers: the idle sentinel (exact + whitespace-padded), real content, and
	// near-misses that must NOT suppress (superstring, lowercase).
	answers := []struct {
		name string
		text string
		idle bool
	}{
		{"exact-idle", AutomatrixIdle, true},
		{"padded-idle", "  AUTOMATRIX_IDLE\n", true},
		{"real-result", "I drafted the quarterly update doc — ready for review.", false},
		{"idle-superstring", "AUTOMATRIX_IDLE and also did a thing", false},
		{"lowercase-idle", "automatrix_idle", false},
		{"empty-answer", "", false},
	}

	for _, in := range inputs {
		for _, ans := range answers {
			t.Run(fmt.Sprintf("%s/%s", in.name, ans.name), func(t *testing.T) {
				rep := &captureReporter{}
				a := New(Options{Config: config.Default(), Reporter: rep})

				a.finishTurn(context.Background(), ans.text, nil, nil, in.text, false)

				wantSuppressed := in.wake && ans.idle
				// Cross-check the property's predicate against the production
				// helper composition so the expectation cannot silently drift.
				if got := automatrixSuppressed(in.text, ans.text); got != wantSuppressed {
					t.Fatalf("suppression predicate mismatch: helpers=%v, expected=%v", got, wantSuppressed)
				}

				if wantSuppressed {
					if len(rep.said) != 0 {
						t.Fatalf("idle AUTOMATRIX wake must be suppressed; Say called %d time(s): %v", len(rep.said), rep.said)
					}
					return
				}
				if len(rep.said) != 1 {
					t.Fatalf("non-suppressed turn must deliver exactly once; got %d Say calls: %v", len(rep.said), rep.said)
				}
				if rep.said[0] != ans.text {
					t.Errorf("delivered text must be the verbatim answer; got %q want %q", rep.said[0], ans.text)
				}
			})
		}
	}
}

// TestProperty3_Agent_WakeNeverSelfSuppresses pins the structural guarantee that
// the wake MARKER and the idle SENTINEL are distinct enough that a wake input
// can never, by itself, satisfy the suppression predicate: detection of a wake
// is independent of the answer that ends it. Without this, an AUTOMATRIX wake
// could be silently swallowed regardless of what Neo produced.
//
// Validates: Requirements 4.2, 4.6.
func TestProperty3_Agent_WakeNeverSelfSuppresses(t *testing.T) {
	// The wake marker, the full wake message, and a marker-bearing prefix are
	// all recognized as wakes, yet none of them is the idle sentinel, so none
	// suppresses when used as the ANSWER.
	for _, s := range []string{AutomatrixWakeMarker, AutomatrixWakeMessage, "AUTOMATRIX: do the thing"} {
		if !isAutomatrixWake(s) {
			t.Errorf("%q must be detected as an AUTOMATRIX wake", s)
		}
		if shouldSuppressAutomatrix(s) {
			t.Errorf("%q must NOT trigger suppression (only the exact sentinel may)", s)
		}
	}
	// And the sentinel must suppress only the exact (trimmed) token.
	if !shouldSuppressAutomatrix(AutomatrixIdle) || !shouldSuppressAutomatrix("\tAUTOMATRIX_IDLE ") {
		t.Error("the exact (trimmed) AUTOMATRIX_IDLE sentinel must suppress")
	}
}
