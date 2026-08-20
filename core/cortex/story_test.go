// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/cmharness"
	"centra/core/cortex/memory"
)

// TestBuildStorySoFarBasic proves task 4.2 do_1: a real transcript builds a
// deterministic extractive StorySoFarRecord (role tally, message count,
// last-user/last-assistant excerpts) that round-trips through LoadStorySoFar.
func TestBuildStorySoFarBasic(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour

	conv := "conv-story"
	for i, s := range []struct {
		role cortex.Role
		body string
	}{
		{cortex.RoleUser, "hello there"},
		{cortex.RoleAssistant, "hi, how can I help"},
		{cortex.RoleUser, "what is the plan"},
		{cortex.RoleAssistant, "the plan is done"},
	} {
		if _, err := c.AppendMessage(cortex.Message{
			ConversationID: conv, Role: s.role, Content: s.body,
		}); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	uri, err := c.BuildStorySoFar(conv)
	if err != nil {
		t.Fatalf("BuildStorySoFar: %v", err)
	}
	if uri != cortex.BuildStorySoFarURI(conv) {
		t.Fatalf("uri = %q, want %q", uri, cortex.BuildStorySoFarURI(conv))
	}

	rec, err := c.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar: %v", err)
	}
	if rec.MessageCount != 4 {
		t.Fatalf("MessageCount = %d, want 4", rec.MessageCount)
	}
	if rec.RoleTally["user"] != 2 || rec.RoleTally["assistant"] != 2 {
		t.Fatalf("RoleTally = %v, want user=2 assistant=2", rec.RoleTally)
	}
	if rec.UpToSeq != 3 {
		t.Fatalf("UpToSeq = %d, want 3 (0-indexed, 4 messages)", rec.UpToSeq)
	}
	if rec.ShortForm == "" {
		t.Fatal("ShortForm empty")
	}
	if !bytes.Contains([]byte(rec.ShortForm), []byte("what is the plan")) {
		t.Fatalf("ShortForm %q missing last_user excerpt", rec.ShortForm)
	}
	if !bytes.Contains([]byte(rec.ShortForm), []byte("the plan is done")) {
		t.Fatalf("ShortForm %q missing last_assistant excerpt", rec.ShortForm)
	}
}

// TestBuildStorySoFarIdempotent proves rebuilding at an unchanged transcript
// head yields a byte-identical record and appends NO new journal entry
// (mirrors cascade.go's compute-compare-skip discipline).
func TestBuildStorySoFarIdempotent(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	conv := "conv-idem"
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar #1: %v", err)
	}
	rec1, err := c.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar #1: %v", err)
	}
	b1, err := cortex.EncodeStorySoFarRecord(rec1)
	if err != nil {
		t.Fatalf("encode rec1: %v", err)
	}

	before := c.Store().JournalCount()
	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar #2: %v", err)
	}
	after := c.Store().JournalCount()
	if after != before {
		t.Fatalf("JournalCount changed across idempotent rebuild: before=%d after=%d", before, after)
	}

	rec2, err := c.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar #2: %v", err)
	}
	b2, err := cortex.EncodeStorySoFarRecord(rec2)
	if err != nil {
		t.Fatalf("encode rec2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("StorySoFarRecord not byte-identical across rebuilds:\n #1 %x\n #2 %x", b1, b2)
	}
}

// TestBuildStorySoFarGrowsWithTranscript proves BuildStorySoFar reflects new
// messages appended after the first build (the "maintained by the ladder"
// discipline) and UpToSeq advances to the new head.
func TestBuildStorySoFarGrowsWithTranscript(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	conv := "conv-grow"
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "first"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar #1: %v", err)
	}
	rec1, err := c.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar #1: %v", err)
	}

	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleAssistant, Content: "second"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar #2: %v", err)
	}
	rec2, err := c.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar #2: %v", err)
	}
	if rec2.UpToSeq <= rec1.UpToSeq {
		t.Fatalf("UpToSeq did not advance: rec1=%d rec2=%d", rec1.UpToSeq, rec2.UpToSeq)
	}
	if rec2.MessageCount != rec1.MessageCount+1 {
		t.Fatalf("MessageCount = %d, want %d", rec2.MessageCount, rec1.MessageCount+1)
	}
}

// TestBuildStorySoFarNoTranscript proves the no-transcript-yet degradation
// (req.7.5 posture reused here): BuildStorySoFar returns ("", nil) and
// LoadStorySoFar returns memory.ErrNotFound, never an error.
func TestBuildStorySoFarNoTranscript(t *testing.T) {
	c, _ := openRollupCortex(t)
	uri, err := c.BuildStorySoFar("conv-none")
	if err != nil {
		t.Fatalf("BuildStorySoFar(no transcript): %v", err)
	}
	if uri != "" {
		t.Fatalf("uri = %q, want empty", uri)
	}
	if _, err := c.LoadStorySoFar("conv-none"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("LoadStorySoFar(no transcript) err = %v, want memory.ErrNotFound", err)
	}
}

// TestBuildStorySoFarDerivedLaneSafety proves req.11.1 posture: BuildStorySoFar
// performs NO anchored-namespace SMT write, and the full OverallRoot rebuilds
// byte-identically with the story journal entry present.
func TestBuildStorySoFarDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	conv := "conv-safety"
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, berr := c.BuildStorySoFar(conv)
		return berr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across BuildStorySoFar: %v", err)
	}

	res, err := cmharness.ReplayPreservesRoot(c, nil)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with story entries present: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}
