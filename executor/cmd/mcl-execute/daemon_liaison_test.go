// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "testing"

// TestSelectProgressLines_CollapsesGenericSpam verifies the fix for the
// repetitive "setting up… still setting up… working on it…" narration:
// a stream of generic pipeline milestones produces exactly ONE progress
// turn; subsequent generic-only batches produce none.
func TestSelectProgressLines_CollapsesGenericSpam(t *testing.T) {
	narrated := map[string]bool{}

	// First batch: several generic milestones -> all feed ONE turn.
	batch1 := []sseEvent{
		{Type: "message.start"},
		{Type: "skill.loaded"},
		{Type: "synth.done"},
	}
	got := selectProgressLines(batch1, narrated, false)
	if len(got) == 0 {
		t.Fatalf("first generic batch should produce narration lines, got none")
	}

	// Second batch: more generic milestones -> suppressed (no new turn).
	batch2 := []sseEvent{
		{Type: "lifecycle.transition", Fields: map[string]interface{}{"to": "accepted"}},
		{Type: "lifecycle.transition", Fields: map[string]interface{}{"to": "executing"}},
		{Type: "step.text"},
	}
	if got := selectProgressLines(batch2, narrated, false); len(got) != 0 {
		t.Fatalf("second generic batch should be suppressed, got %v", got)
	}
}

// TestSelectProgressLines_EmptyBatchSilent verifies events that humanize
// to "" never produce a hollow filler turn (the root cause of the model
// hallucinating "I'm setting up your wallet" from an empty event list).
func TestSelectProgressLines_EmptyBatchSilent(t *testing.T) {
	narrated := map[string]bool{}
	batch := []sseEvent{
		{Type: "some.unknown.event"},
		{Type: "lifecycle.transition", Fields: map[string]interface{}{"to": "draining"}},
	}
	if got := selectProgressLines(batch, narrated, false); len(got) != 0 {
		t.Fatalf("batch with no meaningful lines should be silent, got %v", got)
	}
}

// TestSelectProgressLines_SalientAlwaysThrough verifies approval and
// on-chain events are real news: each gets a turn even after the generic
// key is already spent, and they dedup on their own text.
func TestSelectProgressLines_SalientAlwaysThrough(t *testing.T) {
	narrated := map[string]bool{genericProgressKey: true}

	salient := []sseEvent{
		{Type: "gate.invoked"},
		{Type: "paxeer.spend.executed"},
	}
	got := selectProgressLines(salient, narrated, false)
	if len(got) != 2 {
		t.Fatalf("both salient events should produce lines, got %v", got)
	}

	// The same salient event again -> deduped to nothing.
	if got := selectProgressLines([]sseEvent{{Type: "gate.invoked"}}, narrated, false); len(got) != 0 {
		t.Fatalf("repeat salient event should be deduped, got %v", got)
	}
}

// TestSelectProgressLines_Heartbeat verifies a single generic "still
// working" update is permitted again after a long quiet gap.
func TestSelectProgressLines_Heartbeat(t *testing.T) {
	narrated := map[string]bool{genericProgressKey: true}
	batch := []sseEvent{{Type: "step.text"}}

	if got := selectProgressLines(batch, narrated, false); len(got) != 0 {
		t.Fatalf("generic batch without heartbeat should be suppressed, got %v", got)
	}
	if got := selectProgressLines(batch, narrated, true); len(got) == 0 {
		t.Fatalf("generic batch WITH heartbeat should re-open one update, got none")
	}
}

// TestIsSalientEvent pins the salient classification.
func TestIsSalientEvent(t *testing.T) {
	for _, ty := range []string{"gate.invoked", "gate.decided", "paxeer.spend.executed"} {
		if !isSalientEvent(ty) {
			t.Errorf("%s should be salient", ty)
		}
	}
	for _, ty := range []string{"message.start", "skill.loaded", "synth.done", "step.text", "lifecycle.transition"} {
		if isSalientEvent(ty) {
			t.Errorf("%s should NOT be salient", ty)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
