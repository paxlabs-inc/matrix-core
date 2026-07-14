// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// continuous.go implements the Neo side of the continuous-memory collapse —
// the ONLY memory path (MORPHEUS req.1): Neo is reduced to
//
//	Append(conv, message) -> Activate(conv, query, budget) -> render -> transport
//
// cortex owns the durable transcript (via AppendMessage), the temporal ladder,
// the activation composer, and the durable story-so-far. This file holds ONLY
// the agent-side glue: recording each message to cortex, rendering the
// cortex.Activate bundle into a USER-role trailing message, and a
// non-summarizing window trim. All selection/ranking/recency lives in
// cortex.Activate — there is no independent agent-side brain logic.
//
// Everything degrades gracefully without a pager (nil = no cortex wired, e.g.
// a bare test agent): records are dropped, the activation bundle is nil, and
// the window carries just the charter + reframe tail.
package agent

import (
	"context"
	"fmt"
	"strings"

	"matrix/cortex"
	cmem "matrix/cortex/memory"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// cmConvID scopes the cortex session/transcript store: the agent's conversation
// id, or "cli" on the bare CLI path (mirrors turnIntentID's fallback).
func (a *Agent) cmConvID() string {
	if a.convID != "" {
		return a.convID
	}
	return "cli"
}

// cmRecordUser appends a user message to the cortex transcript.
func (a *Agent) cmRecordUser(content string) {
	if a.pager == nil {
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

// cmRecordAssistant appends an assistant turn to the cortex transcript: its
// content (if any) as an assistant message, and each tool call as a tool_call
// message (name + canonical-JSON args, losslessly).
func (a *Agent) cmRecordAssistant(msg llm.Message) {
	if a.pager == nil {
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

// cmRecordToolResult appends a tool result to the cortex transcript. cortex
// spills an oversized tool_result to a resolvable ref on its own (the overflow
// discipline lives in session.go), so the full content is handed over.
func (a *Agent) cmRecordToolResult(name, content string) {
	if a.pager == nil {
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
	if a.pager == nil {
		return nil
	}
	// Window-proportional budget (config.ActivationBudget): a 1M window
	// carries a ~20K-token bundle instead of cortex's legacy 3K default, so
	// the durable-memory field is as rich as the window affords.
	b, err := a.pager.Activate(a.cmConvID(), query, cortex.Budget{Tokens: a.cfg.ActivationBudget()})
	if err != nil {
		return nil
	}
	return b
}

// cmRelevancePush is the Q2 first-message relevance push. cortex.Activate's
// pushed tiers (Pinned/Timeline/Recent) are recency-based and query-INDEPENDENT
// (activate.go: the query arg is unused), so on the OPENING message the agent
// carries recency but nothing matched to what the user actually asked unless it
// reactively calls memory_recall. This runs the pager's relevance retrieval
// (semantic HNSW + the always-on salience lane + edge cascade) ONCE, on turn 1
// only, and returns both the retrieved snippets (so the caller can fold them
// into the turn's surfaced set for the usage-salience learning loop) and a
// rendered block to append to the activation tail. After turn 1 the pull-not-
// push philosophy resumes (the model pages in the rest via memory_recall).
//
// Returns (nil, "") when disabled, off the first turn, without a pager, or when
// retrieval yields nothing — a pure read, no durable state is touched.
func (a *Agent) cmRelevancePush(ctx context.Context, query string) ([]memory.Snippet, string) {
	if !a.cfg.FirstTurnRelevancePush || a.turnSeq != 1 || a.pager == nil {
		return nil, ""
	}
	snips, err := a.pager.Retrieve(ctx, query)
	if err != nil || len(snips) == 0 {
		return nil, ""
	}
	var b strings.Builder
	b.WriteString("\nRelevant to your message (auto-recalled from memory; call memory_recall for more — may be stale, the live conversation wins):\n")
	for _, s := range snips {
		line := strings.TrimSpace(s.Text)
		if line == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(line)
		if s.Note != "" {
			b.WriteString(" [")
			b.WriteString(s.Note)
			b.WriteString("]")
		}
		b.WriteString("\n")
	}
	return snips, b.String()
}

// MemoryEvent is the legible, protocol-free view of the memory Neo carries into
// a turn (continuous-memory task 7.1, req.10): the durable story-so-far and a
// coarse timeline of past activity. It deliberately carries NO journal / MMR /
// rollup / snapshot jargon and no raw URIs — only the RESULT a user should see.
type MemoryEvent struct {
	StorySoFar string   // durable rolling summary of this conversation
	Timeline   []string // coarse timeline short-forms, oldest first
}

// MemoryObserver receives the per-turn MemoryEvent. nil disables surfacing; the
// agent loop stays oblivious to the presentation layer (mirrors AuditObserver).
type MemoryObserver func(MemoryEvent)

// emitMemory hands the harness a legible summary of this turn's activation
// bundle so the client can show the memory Neo carries (task 7.1). Pure
// side-channel: it reads the already-computed read-only bundle, mutates nothing,
// and is a no-op when no observer is wired or there is nothing to show.
func (a *Agent) emitMemory(b *cortex.ActivationBundle) {
	if a.memObserver == nil || b == nil {
		return
	}
	timeline := make([]string, 0, len(b.Timeline))
	for _, r := range b.Timeline {
		if sf := strings.TrimSpace(r.ShortForm); sf != "" {
			timeline = append(timeline, sf)
		}
	}
	ev := MemoryEvent{StorySoFar: strings.TrimSpace(b.StorySoFar), Timeline: timeline}
	if ev.StorySoFar == "" && len(ev.Timeline) == 0 {
		return
	}
	a.memObserver(ev)
}

// renderActivationBundle renders the cortex.Activate bundle into the trailing
// working-memory block — the single window law's memory tail (MORPHEUS req.1.3).
// It frames the block as in-progress reference (never a new instruction — the
// leading line tells the model to continue, not restart), then surfaces the
// standing objective as background, the Pinned tier (identity, hard constraints,
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

	sb.WriteString("(Reference notes, not a new message — this conversation is already in progress. Continue from the live exchange above; do NOT restart it or re-answer an earlier request.)\n")

	if goal := strings.TrimSpace(a.activeGoal); goal != "" {
		fmt.Fprintf(&sb, "Standing objective for this conversation (background, already underway — not a fresh request): %s\n", goal)
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
