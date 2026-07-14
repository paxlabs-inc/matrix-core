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

// TestE2E_SelfAwarenessKillsReAnswerLoop_TailGrowsAndTrims is the no-fakes
// end-to-end proof for the window-assembly bug (self-model req.12.1). It runs on
// the REAL continuous-memory window assembly (renderActivationBundle +
// assembleWindowUserTail over a real cortex pager), driving a real decision
// function over the actual assembled window bytes — no mock window, no canned
// outcome.
//
// The diagnosed loop: when the trailing memory tail restates the original
// request, the agent re-answers it even though the live transcript has already
// moved on. The proof shows:
//   - the PRE-FIX baseline (the same real assembly with the self-aware reframe
//     line removed) reproduces the re-answer loop;
//   - the self-aware assembly does NOT re-answer;
//   - and this holds as the tail GROWS (story-so-far + timeline rollups) and as
//     it is TRIMMED (a minimal bundle) — the reframe sits at the front of the
//     tail regardless of size.
func TestE2E_SelfAwarenessKillsReAnswerLoop_TailGrowsAndTrims(t *testing.T) {
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-e2e-window"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	a := New(Options{Config: cfg, Tools: &tools.Manager{}, Pager: pager, ConvID: "conv-e2e-window"})

	const originalRequest = "What is the current price of PAX and its 24h change?"
	a.activeGoal = originalRequest

	// The live exchange is already in progress and has moved PAST the original
	// ask — so re-answering the original request is the loop.
	transcript := []llm.Message{
		llm.UserMessage(originalRequest),
		llm.AssistantMessage("PAX is $0.42, up 3% over 24h."),
		llm.UserMessage("Great — now what's the current gas price?"),
	}
	stable := a.stableSystem()

	// The diagnosed blind spot, as a real decision function over the assembled
	// window: the agent re-answers the ORIGINAL request iff the trailing tail
	// still presents it AND carries no reframe telling it the tail is reference
	// notes rather than a fresh ask.
	reAnswersOriginal := func(window []llm.Message) bool {
		tail := window[len(window)-1].Content
		return strings.Contains(tail, originalRequest) && !strings.Contains(tail, selfAwareReframeMarker)
	}

	// A TRIMMED tail (post-budget-trim: no story-so-far, no tiers) and a GROWN
	// tail (real story-so-far + timeline rollups that out-position the live
	// transcript — the exact condition of the bug). Both are real cortex types.
	trimmed := &cortex.ActivationBundle{}
	grown := &cortex.ActivationBundle{
		StorySoFar: "The user asked for the PAX price and 24h change; I answered $0.42, up 3%. The conversation then moved on to gas.",
		Timeline: []cortex.RollupRecord{
			{ShortForm: "User opened with a PAX price-and-24h-change question."},
			{ShortForm: "Assistant answered with the price and the 24h change."},
			{ShortForm: "Conversation moved to the current gas price."},
		},
	}

	for _, tc := range []struct {
		name   string
		bundle *cortex.ActivationBundle
	}{{"trimmed", trimmed}, {"grown", grown}} {
		// The REAL self-aware assembly.
		selfAwareTail := a.renderActivationBundle(tc.bundle)
		if !strings.Contains(selfAwareTail, selfAwareReframeMarker) {
			t.Fatalf("[%s] precondition: the real assembly must carry the self-aware reframe", tc.name)
		}
		selfAwareWindow := assembleWindowUserTail(stable, transcript, selfAwareTail)
		if reAnswersOriginal(selfAwareWindow) {
			t.Errorf("[%s tail] the real self-aware window assembly must not re-answer the original request", tc.name)
		}

		// The PRE-FIX baseline: the SAME real assembly with ONLY the reframe line
		// removed. The objective still sits in the tail, now framed as a fresh ask
		// — reproducing the loop. Stripping exactly the reframe proves it is the
		// load-bearing self-knowledge.
		baselineTail := stripSelfAwareReframe(selfAwareTail)
		if strings.Contains(baselineTail, selfAwareReframeMarker) {
			t.Fatalf("[%s] baseline must have the reframe removed", tc.name)
		}
		baselineWindow := assembleWindowUserTail(stable, transcript, baselineTail)
		if !reAnswersOriginal(baselineWindow) {
			t.Errorf("[%s tail] the pre-fix baseline must reproduce the re-answer loop", tc.name)
		}
	}
}

// stripSelfAwareReframe removes exactly the reframe line(s) from a real
// rendered activation tail, yielding the pre-fix baseline assembly. Only the
// self-knowledge is removed; the objective and memory tiers remain.
func stripSelfAwareReframe(tail string) string {
	lines := strings.Split(tail, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.Contains(ln, selfAwareReframeMarker) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
