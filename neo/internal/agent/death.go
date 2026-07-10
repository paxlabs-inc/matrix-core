// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"strings"

	"matrix/neo/internal/llm"
)

// Death reason classes — the loop-affecting way a supervised attempt ended, so
// the durable death-journal entry (and any self-authored failure pattern
// derived from it) can group deaths by mode rather than by prose.
const (
	DeathReasonStall        = "no_progress_stall" // repeated the same step without progress
	DeathReasonStepBudget   = "step_budget"       // exhausted the per-turn step budget
	DeathReasonUnproductive = "unproductive_cap"  // hit the unified unproductive-attempt bound
)

// LoopDeath is the STRUCTURED capture of the loop state at the moment a turn
// died in a loop-affecting way (self-model task 2.2, req.3.1) — the rich
// experiential-self record that extends last session's text-only summary. It is
// read by the supervisor (via Agent.LastDeath) to write a durable death-journal
// entry the successor and the self-authoring pass can learn from. Every field
// is populated from the ACTUAL agent loop state, never a placeholder.
type LoopDeath struct {
	// Objective is the conversation's active goal at death (clamped).
	Objective string `json:"objective"`
	// Faculty is which faculty was running — conversation (the top-level Neo),
	// or sub-agent for a spawned scoped instance.
	Faculty string `json:"faculty"`
	// Reason is the loop-affecting death class (one of DeathReason*).
	Reason string `json:"reason"`
	// LastTool is the tool name of the final assistant batch before death.
	LastTool string `json:"last_tool"`
	// RepeatCount is the no-progress repeat count at death.
	RepeatCount int `json:"repeat_count"`
	// RecentSignatures is the small window of recent batch signatures that shows
	// the shape of the spiral (already-seen operations, rotating cycles).
	RecentSignatures []string `json:"recent_signatures,omitempty"`
	// DistinctTools is how many distinct tool names the turn dispatched (breadth).
	DistinctTools int `json:"distinct_tools"`
	// ContextFillPct is the context-window fill fraction at death (0–100).
	ContextFillPct int `json:"context_fill_pct"`
	// StepsUsed is how many loop iterations the turn ran before dying.
	StepsUsed int `json:"steps_used"`
	// Digest is the concise where-it-got-stuck line (the last tool summary).
	Digest string `json:"digest,omitempty"`
}

// loopSnapshot is the live per-turn loop state, refreshed each Chat iteration so
// a death exit can capture the real state at death without threading locals into
// every return path.
type loopSnapshot struct {
	repeats       int
	recentSigs    []string
	lastTool      string
	distinctTools int
	contextPct    int
	step          int
}

// faculty reports which faculty this agent is — conversation for the top-level
// Neo, sub-agent for a spawned scoped instance (persona set). The coding and
// execution faculties are separate daemons/pipelines; a Neo Agent is always the
// conversation faculty unless it is a headless sub-agent.
func (a *Agent) faculty() string {
	if strings.TrimSpace(a.persona) != "" {
		return "sub-agent"
	}
	return "conversation"
}

// snapshotLoop refreshes the live loop-state snapshot for THIS iteration. Called
// once per step after the no-progress read, so a subsequent death exit captures
// the real state. distinctTools is the breadth seen through prior steps plus any
// new tool this batch introduces.
func (a *Agent) snapshotLoop(step, pct, repeats int, recentSigs []string, batch []llm.ToolCall, distinct map[string]struct{}) {
	extra := 0
	for _, c := range batch {
		if _, seen := distinct[c.Function.Name]; !seen {
			extra++
		}
	}
	a.curLoop = loopSnapshot{
		repeats:       repeats,
		recentSigs:    append([]string(nil), recentSigs...),
		lastTool:      lastToolName(batch),
		distinctTools: len(distinct) + extra,
		contextPct:    pct,
		step:          step,
	}
}

// recordDeath finalizes the structured death record from the live loop snapshot
// at a death exit (stall, step budget, or the unproductive-cap escalation). It
// is best-effort observability: it never blocks or alters the returned error,
// preserving the existing summary + successor-prime digest (req.3.2).
func (a *Agent) recordDeath(reason, digest string) {
	a.lastDeath = &LoopDeath{
		Objective:        truncate(strings.TrimSpace(a.activeGoal), 200),
		Faculty:          a.faculty(),
		Reason:           reason,
		LastTool:         a.curLoop.lastTool,
		RepeatCount:      a.curLoop.repeats,
		RecentSignatures: a.curLoop.recentSigs,
		DistinctTools:    a.curLoop.distinctTools,
		ContextFillPct:   a.curLoop.contextPct,
		StepsUsed:        a.curLoop.step + 1,
		Digest:           oneLine(strings.TrimSpace(digest)),
	}
}

// LastDeath returns the structured loop-state capture from the most recent
// loop-affecting death in the current turn (self-model task 2.2). ok is false
// when the turn did not die in a loop-affecting way (a clean finish, a plain
// model/transport error). The supervisor reads it to enrich the durable
// death-journal entry with the real loop state at death.
func (a *Agent) LastDeath() (LoopDeath, bool) {
	if a.lastDeath == nil {
		return LoopDeath{}, false
	}
	return *a.lastDeath, true
}

// StateLine renders the structured loop state as a compact, parseable suffix
// appended to the durable death-journal summary, so the record CARRIES the loop
// state at death (req.3.1) without changing the existing summary prefix.
func (d LoopDeath) StateLine() string {
	sigs := ""
	if len(d.RecentSignatures) > 0 {
		sigs = fmt.Sprintf(" recent_signatures=%d", len(d.RecentSignatures))
	}
	return fmt.Sprintf(
		" [loop-state: reason=%s faculty=%s last_tool=%s repeats=%d distinct_tools=%d context_fill=%d%% steps=%d%s]",
		orNone(d.Reason), orNone(d.Faculty), orNone(d.LastTool),
		d.RepeatCount, d.DistinctTools, d.ContextFillPct, d.StepsUsed, sigs,
	)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

// lastToolName returns the tool name of the last call in an assistant batch, or
// "" when the batch carried no tool calls (a plain-message death).
func lastToolName(batch []llm.ToolCall) string {
	if len(batch) == 0 {
		return ""
	}
	return batch[len(batch)-1].Function.Name
}
