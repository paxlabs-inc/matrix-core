// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

// rebaseTranscript builds a real multi-turn working transcript (~100K estimated
// tokens): several user turns, each answered with tool-call evidence, so the
// trim path has both an "older" section to carve and recent turns to keep.
func rebaseTranscript() []llm.Message {
	filler := strings.Repeat("evidence line from a real tool result. ", 200) // ~2K tokens
	var msgs []llm.Message
	for turn := 0; turn < 8; turn++ {
		msgs = append(msgs, llm.UserMessage("investigate item "+strings.Repeat("x", turn+1)))
		for i := 0; i < 6; i++ {
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "checking " + filler})
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, Name: "fs_read", Content: filler})
		}
	}
	return msgs
}

// TestWindowRebaseTrimAtBothScales proves the re-based budget derivation
// against the REAL trim trigger + trim code paths (req 1.2/1.3/1.4): the same
// transcript that leaves ample headroom at the 1M default (no trim — within-
// turn tool evidence retained) is genuine pressure at a 32K override (the trim
// fires and drops older turns while keeping the recent user turns verbatim).
func TestWindowRebaseTrimAtBothScales(t *testing.T) {
	system := "charter"

	t.Run("1M default: ample window, evidence retained", func(t *testing.T) {
		cfg := config.Default()
		if cfg.ContextWindowTokens != 1000000 {
			t.Fatalf("default ContextWindowTokens = %d, want 1000000", cfg.ContextWindowTokens)
		}
		a := New(Options{Config: cfg})
		a.working = rebaseTranscript()
		before := len(a.working)
		if a.overSoftBudget(system) || a.overHardBudget(system) {
			t.Fatalf("~%d tokens must not trip 1M budgets (soft=%d hard=%d)",
				a.usedTokens(system), cfg.SoftBudgetTokens(), cfg.HardBudgetTokens())
		}
		// The loop's exact guard: no trim while ample window remains.
		if a.overHardBudget(system) {
			a.cmTrimWorking()
		}
		if len(a.working) != before {
			t.Fatalf("within-turn tool evidence evicted with ample window: %d -> %d messages", before, len(a.working))
		}
	})

	t.Run("32K override: genuine pressure, trim fires", func(t *testing.T) {
		cfg := config.Default()
		cfg.ContextWindowTokens = 32768 // the NEO_CONTEXT_WINDOW_TOKENS local-model path
		a := New(Options{Config: cfg})
		a.working = rebaseTranscript()
		before := len(a.working)
		if !a.overHardBudget(system) {
			t.Fatalf("~%d tokens must trip the 32K hard budget (%d)", a.usedTokens(system), cfg.HardBudgetTokens())
		}
		if a.overHardBudget(system) {
			a.cmTrimWorking()
		}
		if len(a.working) >= before {
			t.Fatalf("trim did not shrink the window: %d -> %d messages", before, len(a.working))
		}
		// The recent user turns survive verbatim (splitForCompaction keeps
		// keepRecentUserTurns): the newest user message must still be present.
		last := "investigate item " + strings.Repeat("x", 8)
		found := false
		for _, m := range a.working {
			if m.Role == llm.RoleUser && m.Content == last {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("most recent user turn was evicted by the trim")
		}
	})
}
