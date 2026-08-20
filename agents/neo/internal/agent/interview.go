// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package agent

import (
	"fmt"
	"strings"
)

// The ORACLE guided personalization interview (task 5.3, req 12).
//
// The interview is a NORMAL Neo conversation — same live surface, same tools —
// with three deliberate differences, all properties of the constructor:
//
//  1. the system prompt gains the guided-interview charter below (five
//     skippable question groups, ONE at a time, plain-language summary +
//     explicit confirmation before anything is saved, sensitive traits
//     forbidden);
//  2. the agent additionally advertises save_personalization_profile — the
//     ONLY surface that tool is advertised on (req 13.3), and its dispatch
//     enforces the confirmation gate (req 12.3);
//  3. the session that mints an interview agent passes NO consolidator, so
//     writeback extraction never runs over interview turns — individual
//     answers can never become fragmented inferred memories (req 12.3).

// NewInterview builds an Agent for a personalization-interview conversation.
// existing is the rendered existing-answers block for a repeat/edit interview
// ("" on a first run, req 12.1).
func NewInterview(o Options, existing string) *Agent {
	o.Interview = true
	o.InterviewExisting = existing
	return New(o)
}

// WritebackEnabled reports whether this agent runs background write-back
// extraction over its turns. False for interview agents (req 12.3: interview
// conversations are excluded from writeback so answers never become
// fragmented inferred memories).
func (a *Agent) WritebackEnabled() bool { return a.consolidator != nil }

// interviewCharter renders the guided-interview section of the system prompt
// (req 12.1/12.2/12.3). Per-agent-constant, so it lives in the stable prefix.
func (a *Agent) interviewCharter(b *strings.Builder, name string) {
	b.WriteString("Personalization interview — this conversation has ONE job:\n")
	fmt.Fprintf(b, "- The user chose to set up (or revisit) their personalization profile, and you (%s) are running the short guided interview. Nothing else happens in this conversation.\n", name)
	b.WriteString("- Ask about the FIVE question groups below, ONE group at a time — one message per group, wait for the answer before moving on. Keep each question short, warm, and concrete. The groups, in order:\n")
	b.WriteString("  1. Interests and topics they care about (work and personal).\n")
	b.WriteString("  2. Day-to-day things they'd like help with (recurring tasks, reminders, planning).\n")
	b.WriteString("  3. Media tastes — what they LIKE and what they DISLIKE, across music, films, shows, books, games, and creators (whichever they care about).\n")
	b.WriteString("  4. How adventurous recommendations should be: familiar, balanced, or surprising.\n")
	b.WriteString("  5. Their morning brief: how long (short / standard / deep) and which sections (news, music, movies & TV, books, games, daily assistance).\n")
	b.WriteString("- EVERY question is skippable. If they skip or decline a group, move on without pressing; a skipped group is simply left out of the profile — never fill it with guesses.\n")
	b.WriteString("- Record ONLY what they actually say. Never infer or record sensitive traits (health, religion, politics, sexuality, finances) even if implied — the profile has no fields for them, and probing for them is forbidden.\n")
	b.WriteString("- After the last group, present a plain-language summary of EVERYTHING you understood, and ask them to confirm it. Only after their explicit confirmation call save_personalization_profile with user_confirmed=true — nothing is saved without that confirmation, and if they correct something, fold the correction in and confirm again before saving.\n")
	b.WriteString("- After a successful save, tell them it's saved and that they can view, edit, export, or delete it in Settings — then the interview is done.\n")
	if a.interviewExisting != "" {
		b.WriteString("- They already have a saved profile — this is a REVISIT. Open by showing them what's currently saved (below), ask what they'd like to change, and only walk the groups they want to revisit; confirmed changes REPLACE the record via the same summary-then-confirm flow.\n")
		b.WriteString("  Currently saved:\n")
		for _, ln := range strings.Split(a.interviewExisting, "\n") {
			b.WriteString("  " + ln + "\n")
		}
	}
	b.WriteString("\n")
}
