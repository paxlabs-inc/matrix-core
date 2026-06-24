// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"

	"matrix/neo/internal/llm"
)

// compactionSystemPrompt is the active-session schema the compaction summary
// MUST fill — nothing load-bearing dropped, high-entropy tokens copied
// verbatim (the trust contract).
const compactionSystemPrompt = `You compress an agent's working memory so it can keep going without losing anything load-bearing. Read the transcript and fill the template below. Rules:
- Copy high-entropy tokens — addresses, transaction hashes, IDs, file paths, exact numbers, command strings — VERBATIM. Never paraphrase or round them.
- Be terse and factual. Omit chit-chat. Keep only what's needed to continue the task correctly.
- If a section has nothing, write "none".

GOAL: <the task being pursued>
DECISIONS: <choices made, each with a one-line why>
ARTIFACTS: <files / addresses / tx hashes / IDs produced or referenced, verbatim>
OPEN: <unresolved questions or blockers>
LAST_RESULTS: <still-relevant tool outputs worth carrying forward>
NEXT: <the planned next step(s)>`

// compact swaps OLDER working history into a consolidated summary when the
// window fills, while keeping the most recent turns VERBATIM (Phase 4.2). It
// announces itself (the spoken promise — transparency rule), summarizes only
// the older section (folding any prior summary forward so nothing accumulated
// is lost), strips dead-weight inline image payloads from that section first,
// validates that every high-entropy token survived, and keeps the recent tail
// intact. Best-effort: on failure it degrades to the recent tail rather than
// risking a runaway window.
//
// reason is "hard" (forced, over the hard threshold) or "soft" (cooperative,
// at a clean boundary) — used only to tune the spoken notice.
func (a *Agent) compact(ctx context.Context, reason string) {
	if len(a.working) <= 2 {
		return // nothing meaningful to consolidate yet
	}

	if reason == "hard" {
		a.out.Notice("I'm right at my working-memory limit — one moment while I consolidate where we are so I don't drop the thread.")
	} else {
		a.out.Notice("We've covered a lot — let me quickly consolidate where we are so I stay sharp.")
	}

	// Keep the most recent turns verbatim; summarize only what is older.
	older, recent := splitForCompaction(a.working, keepRecentUserTurns)
	if older == nil {
		// Too short to carve a clean older section but still over budget — the
		// window is dominated by large payloads. Strip inline images and keep a
		// safe tail rather than summarizing recent verbatim context away.
		a.stripOldImages()
		a.working = safeTail(a.working)
		return
	}

	// Inline image payloads in the older section are dead weight once they are
	// being summarized to text — strip them before rendering so the summarizer
	// and the validator operate on text, not base64.
	stripImagesIn(older)

	transcript := renderTranscript(older)

	// P1-3: Synchronous pre-compaction consolidation. Before the older turns
	// are evicted from a.working, run a SYNCHRONOUS consolidation pass so
	// durable facts/events/patterns reach cortex FIRST. This closes the gap
	// where the async consolidator could lag behind compaction and lose the
	// last turns. The async path (steady-state) is unchanged. Nil consolidator
	// is a no-op (same posture as consolidateWorking).
	if a.consolidator != nil {
		a.consolidator.ConsolidateSync(ctx, transcript)
	}

	prior := strings.TrimSpace(a.summary)
	source := transcript
	userMsg := "Transcript to consolidate:\n\n" + transcript
	if prior != "" {
		// Fold the existing summary forward so previously-evicted context is
		// carried, not dropped; validate against both halves.
		source = prior + "\n" + transcript
		userMsg = "Summary so far (extend it; keep everything load-bearing):\n" + prior + "\n\n" + userMsg
	}

	client := a.cheap
	if client == nil {
		client = a.main
	}
	res, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(compactionSystemPrompt),
			llm.UserMessage(userMsg),
		},
	})
	if err != nil || res == nil || strings.TrimSpace(res.Message.Content) == "" {
		// Consolidation failed — keep the prior summary and the recent verbatim
		// tail so nothing recent is lost and the window still shrinks (the older
		// raw turns drop, but they were the least recent and image-stripped).
		a.working = recent
		return
	}

	// [transparency.authorship] validator: a silent pass confirms every
	// high-entropy token survived verbatim before the older turns are evicted —
	// the trust contract (i3). Dropped identifiers are re-appended, never lost.
	summary, _ := validateSummary(source, res.Message.Content)
	a.summary = summary
	a.working = recent
}

// renderTranscript flattens the working messages into a plain-text transcript
// for the summarizer.
func renderTranscript(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString("USER: ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
		case llm.RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				b.WriteString("ASSISTANT: ")
				b.WriteString(c)
				b.WriteString("\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("ASSISTANT→tool ")
				b.WriteString(tc.Function.Name)
				b.WriteString(" ")
				b.WriteString(tc.Function.Arguments)
				b.WriteString("\n")
			}
		case llm.RoleTool:
			b.WriteString("TOOL ")
			b.WriteString(m.Name)
			b.WriteString(": ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// safeTail keeps the transcript from the last user message onward, so no
// tool-result message is left without its preceding assistant tool-call
// message (which most providers reject).
func safeTail(msgs []llm.Message) []llm.Message {
	last := -1
	for i, m := range msgs {
		if m.Role == llm.RoleUser {
			last = i
		}
	}
	if last <= 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs)-last)
	copy(out, msgs[last:])
	return out
}
