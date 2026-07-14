// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/cortex"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// These tests cover cmTrimWorking — the single memory path's ONLY window trim
// (MORPHEUS req.1.4). The legacy a.compact sync-consolidation property
// ("evicted turns reach cortex before eviction") is structurally subsumed:
// every message is recorded to the durable cortex transcript AT APPEND TIME
// (cmRecordUser / cmRecordAssistant / cmRecordToolResult), so anything the
// trim later drops from the in-memory window is already durable. The tests
// prove that on the real pager + cortex store, no fakes.

// newTrimTestAgent builds an Agent over a REAL pager on a temp cortex store.
// Main is nil: the trim must never touch a model (non-summarizing).
func newTrimTestAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-trim-sync"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	return New(Options{Config: cfg, Tools: &tools.Manager{}, Pager: pager, ConvID: "conv-trim-sync"})
}

// TestCmTrim_DroppedTurnsAlreadyDurableInCortex proves the trust contract on
// the single path: high-entropy tokens in turns the trim drops from the
// in-memory window are ALREADY in the durable cortex transcript, verbatim —
// recorded at append time, not flushed at eviction time.
func TestCmTrim_DroppedTurnsAlreadyDurableInCortex(t *testing.T) {
	a := newTrimTestAgent(t)

	tokens := []string{
		"0x742d35Cc6634C0532925a3b844Bc9e7a5C42d8fc",
		"0xabc123def4567890123456789012345678901234567890123456789012345678",
		"/root/project/src/main.go",
		"tx_4a5e1e3ba096a3b42cf2e7e8b3a5d4c1f9e8d7c6b5a4e3f2d1c0b9a8e7f6d5c4",
	}
	for i, tok := range tokens {
		user := "turn " + string(rune('a'+i)) + " ref: " + tok
		a.working = append(a.working, llm.UserMessage(user))
		a.cmRecordUser(user)
		reply := "response " + string(rune('a'+i))
		a.working = append(a.working, llm.AssistantMessage(reply))
		a.cmRecordAssistant(llm.AssistantMessage(reply))
	}
	// Enough extra turns that the token-bearing turns fall in the OLDER
	// section the trim drops (keepRecentUserTurns bounds the recent tail).
	for i := 0; i < 6; i++ {
		user := "extra " + string(rune('a'+i))
		a.working = append(a.working, llm.UserMessage(user))
		a.cmRecordUser(user)
		reply := "extra resp " + string(rune('a'+i))
		a.working = append(a.working, llm.AssistantMessage(reply))
		a.cmRecordAssistant(llm.AssistantMessage(reply))
	}

	before := len(a.working)
	a.cmTrimWorking()
	if len(a.working) >= before {
		t.Fatalf("cmTrimWorking must shrink the window: before=%d after=%d", before, len(a.working))
	}
	// The token-bearing turns are gone from the in-memory window…
	live := ""
	for _, m := range a.working {
		live += m.Content + "\n"
	}
	for _, tok := range tokens {
		if strings.Contains(live, tok) {
			t.Fatalf("test setup: token %q survived the trim in-window; add more recent turns", tok)
		}
	}
	// …and present, verbatim, in the durable cortex transcript.
	bundle, err := a.pager.Activate("conv-trim-sync", "", cortex.Budget{})
	if err != nil {
		t.Fatalf("pager.Activate: %v", err)
	}
	durable := ""
	for _, m := range bundle.Transcript {
		durable += m.Content + "\n"
	}
	for _, tok := range tokens {
		if !strings.Contains(durable, tok) {
			t.Errorf("verbatim token NOT durable in the cortex transcript after trim: %q", tok)
		}
	}
}

// TestCmTrim_NonSummarizingAndKeepsRecent proves the trim is a pure in-memory
// bound: nil model client (a summarization attempt would panic), no summary
// produced, the most recent user turns survive verbatim.
func TestCmTrim_NonSummarizingAndKeepsRecent(t *testing.T) {
	a := newTrimTestAgent(t)
	for i := 0; i < 10; i++ {
		a.working = append(a.working,
			llm.UserMessage("user turn "+string(rune('a'+i))),
			llm.AssistantMessage("reply "+string(rune('a'+i))),
		)
	}
	lastUser := "user turn " + string(rune('a'+9))

	a.cmTrimWorking() // Main is nil: must not touch a model

	if len(a.working) == 0 {
		t.Fatal("cmTrimWorking must keep the recent turns, not clear the window")
	}
	kept := ""
	for _, m := range a.working {
		kept += m.Content + "\n"
	}
	if !strings.Contains(kept, lastUser) {
		t.Errorf("the most recent user turn must survive the trim verbatim; window:\n%s", kept)
	}
	if a.working[0].Role != llm.RoleUser {
		t.Errorf("the trimmed window must start at a user turn (provider-safe shape), got role %q", a.working[0].Role)
	}
}

// TestCmTrim_SingleLongTurnFallsBackToSafeTail proves the no-older-section
// path: when the window is one long turn (nothing to carve), the trim strips
// dead-weight images and keeps a provider-safe tail instead of summarizing
// recent verbatim context away.
func TestCmTrim_SingleLongTurnFallsBackToSafeTail(t *testing.T) {
	a := newTrimTestAgent(t)
	a.working = append(a.working, llm.UserMessage("the one long turn"))
	for i := 0; i < 8; i++ {
		a.working = append(a.working, llm.AssistantMessage("working on it "+string(rune('a'+i))))
	}

	a.cmTrimWorking()

	if len(a.working) == 0 {
		t.Fatal("safeTail must keep the live turn")
	}
	if a.working[0].Role != llm.RoleUser || a.working[0].Content != "the one long turn" {
		t.Errorf("safeTail must keep the transcript from the last user message onward; got first=%q role=%q", a.working[0].Content, a.working[0].Role)
	}
}
