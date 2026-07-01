// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// continuous.go implements the Neo side of the continuous-memory collapse
// (spec/continuous-memory, waves 6): Neo is reduced to
//
//	Append(conv, message) -> Activate(conv, query, budget) -> render -> transport
//
// cortex owns the durable transcript (via AppendMessage), the temporal ladder,
// the activation composer, and the durable story-so-far. This file holds ONLY
// the agent-side glue: recording each message to cortex (task 6.1), rendering
// the cortex.Activate bundle into a USER-role trailing message that replaces the
// dynamicTail memory sections (task 6.2), and a non-summarizing window trim that
// replaces a.compact (task 6.3's a.summary/a.compact retirement). All selection/
// ranking/recency lives in cortex.Activate now — there is no independent
// agent-side brain logic on this path.
//
// Everything here is GATED by a.continuousMemory(): with the feature flag off
// the legacy pager path is byte-identical (nothing in this file runs).
package agent

import (
	"fmt"
	"strings"

	"matrix/cortex"
	cmem "matrix/cortex/memory"
	"matrix/neo/internal/llm"
)

// continuousMemory reports whether the continuous-memory collapse is active for
// this agent: the feature flag is on and a pager (the cortex-brain shim) is
// wired. The session store is scoped by cmConvID (the conversation, or "cli").
func (a *Agent) continuousMemory() bool {
	return a.cfg.ContinuousMemory && a.pager != nil
}

// cmConvID scopes the cortex session/transcript store: the agent's conversation
// id, or "cli" on the bare CLI path (mirrors turnIntentID's fallback).
func (a *Agent) cmConvID() string {
	if a.convID != "" {
		return a.convID
	}
	return "cli"
}

// cmRecordUser appends a user message to the cortex transcript (task 6.1).
func (a *Agent) cmRecordUser(content string) {
	if !a.continuousMemory() {
		return
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	a.cmAppend(cortex.Message{
		ConversationID: a.cmConvID(),
		Role:           cortex.RoleUser,
		Content:        content,
	})
}

// cmRecordAssistant appends an assistant turn to the cortex transcript (task
// 6.1): its content (if any) as an assistant message, and each tool call as a
// tool_call message (name + canonical-JSON args, losslessly — req.2.3).
func (a *Agent) cmRecordAssistant(msg llm.Message) {
	if !a.continuousMemory() {
		return
	}
	// Guidance-channel turns are internal steering, never durable transcript.
	if msg.IsGuidance() {
		return
	}
	conv := a.cmConvID()
	if c := strings.TrimSpace(msg.Content); c != "" {
		a.cmAppend(cortex.Message{
			ConversationID: conv,
			Role:           cortex.RoleAssistant,
			Content:        c,
		})
	}
	for _, tc := range msg.ToolCalls {
		var args []byte
		if s := tc.Function.Arguments; s != "" {
			args = []byte(s)
		}
		a.cmAppend(cortex.Message{
			ConversationID: conv,
			Role:           cortex.RoleToolCall,
			ToolName:       tc.Function.Name,
			ToolArgs:       args,
		})
	}
}

// cmRecordToolResult appends a tool result to the cortex transcript (task 6.1).
// cortex spills an oversized tool_result to a resolvable ref on its own (the
// overflow discipline lives in session.go), so the full content is handed over.
func (a *Agent) cmRecordToolResult(name, content string) {
	if !a.continuousMemory() {
		return
	}
	a.cmAppend(cortex.Message{
		ConversationID: a.cmConvID(),
		Role:           cortex.RoleToolResult,
		ToolName:       name,
		Content:        content,
	})
}

// cmAppend writes one message to the cortex session store. Best-effort: a
// durable-append failure does not crash the turn (the in-memory transcript
// remains the live source for THIS turn); durability is a background concern.
func (a *Agent) cmAppend(m cortex.Message) {
	_, _ = a.pager.AppendMessage(m)
}

// cmActivate computes this turn's activation bundle ONCE (Pinned computed once
// per turn — the NE-7 fix is realised by cortex's per-turn pinned cache plus
// this single call site). Best-effort: on error it returns nil and the turn
// degrades to the bare charter window (req.7.5 graceful degradation).
func (a *Agent) cmActivate(query string) *cortex.ActivationBundle {
	b, err := a.pager.Activate(a.cmConvID(), query, cortex.Budget{})
	if err != nil {
		return nil
	}
	return b
}

// renderActivationBundle renders the cortex.Activate bundle into the trailing
// working-memory block that REPLACES the dynamicTail memory sections (req.9.4).
// It surfaces the current goal, the Pinned tier (identity, hard constraints,
// active cortex goals), the durable story-so-far, the T0 timeline and T1 recent
// tiers, and a note that exact specifics page in on demand via memory_recall
// (T3 handles — the pull-over-push philosophy, req.9.5). The transcript slice is
// NOT re-rendered here: the live transcript is the window's middle already.
//
// The block is delivered by the caller as a USER-role trailing message
// (assembleWindowUserTail) so strict Qwen-derived chat templates accept it (the
// prompt-window portability fix), keeping the byte-stable system prefix at
// index 0.
func (a *Agent) renderActivationBundle(b *cortex.ActivationBundle) string {
	var sb strings.Builder

	goal := strings.TrimSpace(a.activeGoal)
	if goal != "" {
		fmt.Fprintf(&sb, "Current goal: %s\n", goal)
	}

	if b == nil {
		return sb.String()
	}

	sb.WriteString("\nYour durable memory (cortex manages this for you; the live exchange above is most current and wins on any conflict):\n")

	if len(b.Pinned) > 0 {
		sb.WriteString("Pinned:\n")
		for _, m := range b.Pinned {
			if line := renderPinnedMemory(m); line != "" {
				sb.WriteString("- ")
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
	}

	if s := strings.TrimSpace(b.StorySoFar); s != "" {
		sb.WriteString("Story so far:\n")
		sb.WriteString(s)
		sb.WriteString("\n")
	}

	if len(b.Timeline) > 0 {
		sb.WriteString("Timeline (coarse, older first):\n")
		for _, r := range b.Timeline {
			if sf := strings.TrimSpace(r.ShortForm); sf != "" {
				sb.WriteString("- ")
				sb.WriteString(sf)
				sb.WriteString("\n")
			}
		}
	}

	if len(b.Recent) > 0 {
		sb.WriteString("Recent activity (page in with memory_recall):\n")
		for _, ep := range b.Recent {
			if u := strings.TrimSpace(string(ep.Ref.URI)); u != "" {
				sb.WriteString("- ")
				sb.WriteString(u)
				sb.WriteString("\n")
			}
		}
	}

	if n := len(b.ReachableURIs); n > 0 {
		fmt.Fprintf(&sb, "More available on demand — call memory_recall to page in exact specifics (%d further items reachable).\n", n)
	}

	return sb.String()
}

// renderPinnedMemory renders one Pinned-tier cortex memory to a single line,
// preferring the medium then short rendered form. Empty when neither form has
// content.
func renderPinnedMemory(m *cmem.Memory) string {
	if m == nil {
		return ""
	}
	if s := strings.TrimSpace(m.Version.Forms.Medium); s != "" {
		return s
	}
	return strings.TrimSpace(m.Version.Forms.Short)
}

// assembleWindowUserTail is the continuous-memory window assembly: the
// byte-stable system prefix at index 0, the append-only live transcript, then
// the rendered Activate bundle as ONE trailing USER-role message (req.9.4 — the
// Qwen-template portability fix). The transcript slice is never mutated (the
// inner append allocates a fresh backing array).
func assembleWindowUserTail(stableSystem string, transcript []llm.Message, tail string) []llm.Message {
	return append(append([]llm.Message{llm.SystemMessage(stableSystem)}, transcript...), llm.UserMessage(tail))
}

// cmTrimWorking is the continuous-memory replacement for a.compact (retired,
// req.9.3): it bounds the live in-memory window WITHOUT an LLM summarization
// pass, because the older turns are already durable in cortex (task 6.1) and
// the coarse history is carried by the durable story-so-far (task 4.2), which
// Activate re-surfaces every turn. It keeps the most recent user turns verbatim
// and drops older ones; when the window is a single long turn (no older section
// to carve), it strips dead-weight inline images and keeps a provider-safe tail.
func (a *Agent) cmTrimWorking() {
	if len(a.working) <= 2 {
		return
	}
	older, recent := splitForCompaction(a.working, keepRecentUserTurns)
	if older == nil {
		a.stripOldImages()
		a.working = safeTail(a.working)
		return
	}
	a.working = recent
}
