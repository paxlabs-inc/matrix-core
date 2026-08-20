// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import "fmt"

// budgetTail renders the live context-budget stat that trails every turn's
// window, plus — for a top-level agent that can actually decompose — the
// self-aware decomposition line (self-model task 4.2). Both are turn-varying, so
// they live in the trailing message AFTER the byte-stable charter prefix and
// never disturb the prompt-cache invariant.
func (a *Agent) budgetTail(pct int) string {
	tail := fmt.Sprintf("\n\n[context: %d%% used]\n", pct)
	// Steps-remaining (synthesis-survives-death): surface how much of the step
	// budget is left so the model starts writing its final answer BEFORE the
	// cliff, not after it — a long synthesis attempted on the last step dies
	// truncated. Only meaningful when a real budget is set for this turn (0 in the
	// bare-agent / test path leaves this line off, preserving prior behavior).
	if a.turn != nil && a.turn.stepBudget > 0 {
		remaining := a.turn.stepBudget - a.turn.curStep
		if remaining < 0 {
			remaining = 0
		}
		tail += fmt.Sprintf("[budget: step %d of %d, %d left — if little remains and you already have enough to answer, STOP gathering and write your final answer NOW rather than starting new work you can't finish.]\n", a.turn.curStep+1, a.turn.stepBudget, remaining)
	}
	if sk := a.decompositionSelfKnowledge(pct); sk != "" {
		tail += sk + "\n"
	}
	return tail
}

// decompositionSelfKnowledge surfaces the agent's OWN context limit and current
// fill at the spawn_subagents decision point (self-model task 4.2, req.8.1/8.2),
// so the choice to fan a task out is grounded in the agent's real, self-known
// limits rather than a single hardcoded threshold. It returns "" (surfaces
// nothing) unless this is the TOP-LEVEL agent AND spawning is actually available
// this session — a sub-agent cannot spawn (no recursion / top-level-only,
// req.8.3), so it is never given a decision it cannot act on.
//
// The number is the same context window the budget math (budgetPct) meters
// against and the structural self-model records (StructuralSelf.ContextLimit),
// so the agent's decision-time self-knowledge is consistent with its derived
// self-model. The framing deliberately hands the JUDGEMENT to the model ("judge
// by whether the work fits what you can hold, not a fixed cutoff") rather than
// firing at a constant — the existing budget/stall failsafes remain the backstop.
func (a *Agent) decompositionSelfKnowledge(pct int) string {
	// Top-level only: a sub-agent (persona set) cannot spawn, so it never sees
	// the decomposition router.
	if a.persona != "" {
		return ""
	}
	// Only when spawn_subagents is wired this session — otherwise there is no
	// decision to inform.
	if a.tools == nil || !a.tools.SpawnEnabled() {
		return ""
	}
	limit := a.cfg.ContextWindowTokens
	if limit <= 0 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf(
		"[self-knowledge] Your context window holds about %d tokens and it is %d%% full right now. You know your own limits: when a task needs heavy exploration (reading many files, crawling several pages, comparing options) that would crowd this window, fan it out with spawn_subagents — each sub-agent works in its own fresh window and only its distilled result returns to yours. Judge by whether the remaining work fits what you can still hold, not a fixed cutoff; if it fits comfortably, just do it yourself.",
		limit, pct,
	)
}
