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

// compact swaps out older working history into a consolidated summary when the
// window fills. It announces itself (the spoken promise — transparency rule),
// re-derives a fresh summary via the cheap model against the active-session
// schema, and trims the live transcript. Best-effort: on failure it degrades
// to a safe tail rather than risking a runaway window.
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

	transcript := renderTranscript(a.working)
	client := a.cheap
	if client == nil {
		client = a.main
	}

	res, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(compactionSystemPrompt),
			llm.UserMessage("Transcript to consolidate:\n\n" + transcript),
		},
	})
	if err != nil || res == nil || strings.TrimSpace(res.Message.Content) == "" {
		// Consolidation failed — keep a safe recent tail so the window can't
		// run away, but never silently lose everything.
		a.working = safeTail(a.working)
		return
	}

	// Re-derived fresh from the current transcript (kills summary drift). The
	// long-term half of context is re-faulted from cortex separately each turn.
	//
	// [transparency.authorship] validator: a silent pass confirms every
	// high-entropy token survived verbatim before the window resets — the
	// trust contract (i3). Dropped identifiers are re-appended, never lost.
	summary, _ := validateSummary(transcript, res.Message.Content)
	a.summary = summary
	a.working = nil
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
