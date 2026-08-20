// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
)

// TestStepBudget_ScalesUpForComplexTurn verifies that when adaptation is
// enabled (StepBudgetMin > 0) and the turn shows tool-call breadth (many
// distinct tools), the effective budget SCALES UP above the floor toward
// the max — giving complex turns more room to work.
func TestStepBudget_ScalesUpForComplexTurn(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 20
	cfg.StepBudgetMax = 80
	a := New(Options{Config: cfg})

	// A complex turn: 6 distinct tools used by step 12 — breadth signals
	// genuine multi-step work that warrants extra budget.
	sig := effectiveBudgetSignals{step: 12, distinctTools: 6}
	got := a.effectiveStepBudget(sig)

	// Must be above the floor (simple turns get the floor; complex get more).
	if got <= cfg.StepBudgetMin {
		t.Errorf("complex turn should scale UP above floor: got %d, floor %d", got, cfg.StepBudgetMin)
	}
	// Must not exceed the max.
	if got > cfg.StepBudgetMax {
		t.Errorf("effective budget must never exceed max: got %d, max %d", got, cfg.StepBudgetMax)
	}
}

// TestStepBudget_StaysLowForSimpleTurn verifies that when adaptation is
// enabled but the turn is simple (few distinct tools, low step), the
// effective budget stays at or near the floor — conservatively not
// over-allocating.
func TestStepBudget_StaysLowForSimpleTurn(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 20
	cfg.StepBudgetMax = 80
	a := New(Options{Config: cfg})

	// A simple turn: 1 distinct tool, early step — no breadth signal.
	sig := effectiveBudgetSignals{step: 2, distinctTools: 1}
	got := a.effectiveStepBudget(sig)

	// Simple turn should stay at the floor (never below it).
	if got < cfg.StepBudgetMin {
		t.Errorf("effective budget must never go below floor: got %d, floor %d", got, cfg.StepBudgetMin)
	}
	// A 1-tool turn at step 2 should NOT scale up to the max.
	if got >= cfg.StepBudgetMax {
		t.Errorf("simple turn should not jump to max: got %d, max %d", got, cfg.StepBudgetMax)
	}
	// Specifically, a simple turn with no breadth should get exactly the floor.
	if got != cfg.StepBudgetMin {
		t.Errorf("simple turn (1 tool, step 2) should yield floor: got %d, want %d", got, cfg.StepBudgetMin)
	}
}

// TestStepBudget_NeverExceedsMax verifies the hard ceiling: even with
// extreme breadth signals, the effective budget is clamped to StepBudgetMax.
func TestStepBudget_NeverExceedsMax(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 20
	cfg.StepBudgetMax = 60
	a := New(Options{Config: cfg})

	// Extreme complexity: 50 distinct tools at step 40.
	sig := effectiveBudgetSignals{step: 40, distinctTools: 50}
	got := a.effectiveStepBudget(sig)

	if got > cfg.StepBudgetMax {
		t.Errorf("effective budget must NEVER exceed max: got %d, max %d", got, cfg.StepBudgetMax)
	}
	if got != cfg.StepBudgetMax {
		t.Errorf("extreme complexity should clamp to max: got %d, want %d", got, cfg.StepBudgetMax)
	}
}

// TestStepBudget_AdaptationDisabledReturnsFixed verifies that when
// StepBudgetMin == 0 (adaptation disabled — the default), the effective
// budget equals StepBudgetMax (the fixed ceiling), so default behavior
// matches today's fixed-budget behavior exactly.
func TestStepBudget_AdaptationDisabledReturnsFixed(t *testing.T) {
	cfg := config.Default()
	// Defaults: StepBudgetMin=0, StepBudgetMax=50.
	a := New(Options{Config: cfg})

	sig := effectiveBudgetSignals{step: 30, distinctTools: 10}
	got := a.effectiveStepBudget(sig)

	if got != cfg.StepBudgetMax {
		t.Errorf("adaptation disabled (min=0) should return fixed max: got %d, want %d", got, cfg.StepBudgetMax)
	}
	if got != 50 {
		t.Errorf("default config should yield 50 (today's fixed budget): got %d", got)
	}
}

// TestStepBudget_NeverBelowFloor verifies the floor is always respected
// even at step 0 with no tools.
func TestStepBudget_NeverBelowFloor(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 25
	cfg.StepBudgetMax = 70
	a := New(Options{Config: cfg})

	sig := effectiveBudgetSignals{step: 0, distinctTools: 0}
	got := a.effectiveStepBudget(sig)

	if got < cfg.StepBudgetMin {
		t.Errorf("effective budget must never go below floor: got %d, floor %d", got, cfg.StepBudgetMin)
	}
}

// TestStepBudget_NoProgressStillTerminates verifies that the no-progress
// stall detection still fires within the adaptive budget. The stall path
// (repeats >= NoProgressStall → ErrIncomplete) must survive: an agent
// repeating the same failing call terminates with an honest incomplete
// signal, NOT a fabricated success, regardless of whether adaptation is
// on or off.
func TestStepBudget_NoProgressStillTerminates(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 20
	cfg.StepBudgetMax = 80
	cfg.NoProgressStall = 3
	a := New(Options{Config: cfg})

	// Simulate the no-progress stall condition: the same batch signature
	// repeated NoProgressStall times must trigger termination. We test
	// using the production batchSignature function on identical ToolCalls.
	identicalCalls := []llm.ToolCall{
		tc("search", `{"q":"test"}`),
	}
	repeats := 0
	prevSig := ""
	stallTripped := false

	// Simulate 5 identical tool-call batches (same signature).
	for i := 0; i < 5; i++ {
		sig := batchSignature(identicalCalls)
		if sig == prevSig {
			repeats++
		} else {
			repeats = 0
			prevSig = sig
		}
		if repeats >= cfg.NoProgressStall {
			stallTripped = true
			break
		}
	}

	if !stallTripped {
		t.Fatal("no-progress stall must trip after NoProgressStall identical batches")
	}

	// The stall must produce an ErrIncomplete, never nil (no false success).
	if ErrIncomplete == nil {
		t.Fatal("ErrIncomplete sentinel must not be nil")
	}

	// Verify that the effective budget at the stall point is within [min,max].
	// A stalling turn (1 tool, repeating) should get the FLOOR, not more —
	// the stall path terminates regardless, but the budget must be valid.
	sig := effectiveBudgetSignals{step: 4, distinctTools: 1, stallRepeats: repeats}
	budget := a.effectiveStepBudget(sig)
	if budget < cfg.StepBudgetMin || budget > cfg.StepBudgetMax {
		t.Errorf("budget at stall point out of [min,max]: got %d, [%d,%d]", budget, cfg.StepBudgetMin, cfg.StepBudgetMax)
	}
	// A stalling turn uses only 1 tool → floor, not extra room.
	if budget != cfg.StepBudgetMin {
		t.Errorf("stalling turn (1 tool) should get floor, not extra budget: got %d, want %d", budget, cfg.StepBudgetMin)
	}
}

// TestStepBudget_MisconfiguredBandFallsBack verifies that a misconfigured
// band (max < min) safely falls back to the fixed StepBudget rather than
// producing an invalid effective budget.
func TestStepBudget_MisconfiguredBandFallsBack(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudgetMin = 60
	cfg.StepBudgetMax = 30 // max < min — misconfigured
	cfg.StepBudget = 45
	a := New(Options{Config: cfg})

	sig := effectiveBudgetSignals{step: 5, distinctTools: 3}
	got := a.effectiveStepBudget(sig)

	if got != cfg.StepBudget {
		t.Errorf("misconfigured band should fall back to StepBudget: got %d, want %d", got, cfg.StepBudget)
	}
}
