// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 8.1 — Property 1: the transcript store and the
// deterministic ladder floor are durable and reproducible (validates
// req.1.1, 1.3, 2.1, 4.1, 4.2, 12.1, 12.2).
//
// This file binds the property statement to REAL code in ONE test each,
// composing the session store (session.go, task 1.1) and the rollup builder
// (rollup.go, task 2.1) over the SAME real journal — a combination none of
// the per-task unit tests (session_test.go, rollup_test.go) exercise
// together: a rollup window that contains BOTH AppendMessage-originated
// journal.KindSession entries and ordinary memory writes. No stub/mock/fake
// stands in for any code path or type (req.12.7).
package cortex_test

import (
	"bytes"
	"testing"
	"time"

	"matrix/cortex"
)

// TestProperty1_SessionRoundTrip proves req.1.1/1.3/12.1: real AppendMessage
// calls round-trip through a real Transcript read, in order, with correct
// (gap-free, monotonic) per-conversation sequencing — interleaved across two
// conversations to prove the ordering is per-conversation, not global.
func TestProperty1_SessionRoundTrip(t *testing.T) {
	c, _ := openRollupCortex(t)

	type step struct {
		conv string
		body string
	}
	steps := []step{
		{"conv-1", "u0"}, {"conv-2", "v0"}, {"conv-1", "u1"},
		{"conv-1", "u2"}, {"conv-2", "v1"}, {"conv-1", "u3"},
	}
	for i, s := range steps {
		if _, err := c.AppendMessage(cortex.Message{
			ConversationID: s.conv, Role: cortex.RoleUser, Content: s.body,
		}); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	conv1, err := c.Transcript("conv-1", 0, 100)
	if err != nil {
		t.Fatalf("Transcript(conv-1): %v", err)
	}
	wantConv1 := []string{"u0", "u1", "u2", "u3"}
	if len(conv1) != len(wantConv1) {
		t.Fatalf("conv-1 len = %d, want %d", len(conv1), len(wantConv1))
	}
	for i, m := range conv1 {
		if m.Seq != uint64(i) {
			t.Fatalf("conv-1[%d] seq = %d, want %d (gap/out-of-order)", i, m.Seq, i)
		}
		if m.Content != wantConv1[i] {
			t.Fatalf("conv-1[%d] content = %q, want %q", i, m.Content, wantConv1[i])
		}
	}

	conv2, err := c.Transcript("conv-2", 0, 100)
	if err != nil {
		t.Fatalf("Transcript(conv-2): %v", err)
	}
	wantConv2 := []string{"v0", "v1"}
	if len(conv2) != len(wantConv2) {
		t.Fatalf("conv-2 len = %d, want %d", len(conv2), len(wantConv2))
	}
	for i, m := range conv2 {
		if m.Seq != uint64(i) {
			t.Fatalf("conv-2[%d] seq = %d, want %d (gap/out-of-order)", i, m.Seq, i)
		}
		if m.Content != wantConv2[i] {
			t.Fatalf("conv-2[%d] content = %q, want %q", i, m.Content, wantConv2[i])
		}
	}
}

// TestProperty1_RollupReproducibleOverSessionAndMemoryEntries proves
// req.2.1/4.1/4.2/12.2: a real journal window that contains BOTH
// AppendMessage-originated journal.KindSession entries AND an ordinary
// anchored memory write builds a rollup whose ShortForm (and full encoded
// record) is byte-identical when rebuilt later, after the wall clock has
// moved on — the deterministic extractive floor is durable and reproducible
// across the whole ladder's real inputs, not just memory writes in
// isolation (rollup_test.go's TestBuildRollupReproducible covers memory
// writes alone).
func TestProperty1_RollupReproducibleOverSessionAndMemoryEntries(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "prop1", Role: cortex.RoleUser, Content: "summarize the last hour",
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	clk.t = baseHour.Add(1 * time.Minute)
	topURI := writePrefAt(t, c, "alpha", 8)
	clk.t = baseHour.Add(2 * time.Minute)
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "prop1", Role: cortex.RoleAssistant, Content: "done",
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Build #1, then advance the wall clock far ahead and rebuild — the
	// window's journal facts are fixed, so the record must not depend on
	// when BuildRollup runs (req.4.1/4.2).
	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup #1: %v", err)
	}
	rec1, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup #1: %v", err)
	}
	// Session entries contribute to the kind tally but are NOT members
	// (only memory writes/updates seed candidate members — rollup.go).
	if rec1.KindTally["session"] != 2 {
		t.Fatalf("KindTally[session] = %d, want 2 (tally=%v)", rec1.KindTally["session"], rec1.KindTally)
	}
	if rec1.KindTally["write"] != 1 {
		t.Fatalf("KindTally[write] = %d, want 1 (tally=%v)", rec1.KindTally["write"], rec1.KindTally)
	}
	if len(rec1.Members) != 1 || rec1.Members[0].URI != topURI {
		t.Fatalf("Members = %+v, want exactly [%q]", rec1.Members, topURI)
	}
	b1, err := cortex.EncodeRollupRecord(rec1)
	if err != nil {
		t.Fatalf("encode rec1: %v", err)
	}

	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup #2: %v", err)
	}
	rec2, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup #2: %v", err)
	}
	b2, err := cortex.EncodeRollupRecord(rec2)
	if err != nil {
		t.Fatalf("encode rec2: %v", err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("RollupRecord not byte-identical across rebuilds:\n #1 %x\n #2 %x", b1, b2)
	}
	if rec1.ShortForm != rec2.ShortForm {
		t.Fatalf("ShortForm drift:\n #1 %q\n #2 %q", rec1.ShortForm, rec2.ShortForm)
	}
	if rec1.ShortForm == "" {
		t.Fatal("ShortForm empty")
	}
}
