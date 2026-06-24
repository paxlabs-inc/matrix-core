// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	_ "embed"
	"fmt"
	"strings"

	"matrix/neo/internal/memory"
	"matrix/neo/internal/recall"
)

// groundTruth is Neo's always-injected factual grounding (who it is, that
// Paxeer is a real live chain, the canonical RPC/explorer/docs endpoints, and
// how to answer chain questions with its read tools instead of blind web
// search). Embedded so it ships in the binary and can't drift from the build.
//
//go:embed knowledge.md
var groundTruth string

// buildSystem composes the full combined system string used for budget/byte
// accounting and legacy callers: the byte-stable behavioral charter + ground
// truth (stableSystem) followed by the turn-varying memory block (dynamicTail).
// It is exactly the concatenation of the two halves now assembled separately
// for the prompt cache (P1-2): buildSystem() == stableSystem() + dynamicTail().
// The actual model-facing window no longer sends this as one front system
// message — see assembleWindow — but the combined size still drives the
// budget/byte compaction thresholds, which is why it is retained.
func (a *Agent) buildSystem(pinned string, retrieved []memory.Snippet, procedural []memory.Pattern, triggered []memory.Snippet, recalled []recall.Hit) string {
	return a.stableSystem() + a.dynamicTail(pinned, retrieved, procedural, triggered, recalled)
}

// stableSystem returns the byte-stable system prefix: the behavioral charter
// (systemPrompt) + the embedded ground truth. It is identical across every
// turn of a session — nothing turn-varying (pinned memory, recalled turns,
// retrieved seeds, the consolidated summary, the budget stat) lives here — so
// it can ride the provider's longest-stable-prefix prompt cache. It is injected
// as the FIRST message of every window (P1-2).
func (a *Agent) stableSystem() string {
	base := a.systemPrompt()

	// P2-2: inject a names-only skill INDEX into the stable prefix. The index
	// lists available skills by NAME only — never full bodies (steps/gotchas/
	// criteria), which are pulled on demand via memory_recall. This keeps the
	// prefix token-bounded and byte-stable across turns (P1-2 cache invariant).
	// Empty index = no section emitted (clean for deployments without skills).
	if len(a.skillIndex) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nSkills you have (call memory_recall for full steps):\n")
	for _, name := range a.skillIndex {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	return b.String()
}

// dynamicTail renders the turn-varying memory block that is appended AFTER the
// append-only transcript as ONE trailing message (P1-2): pinned identity/rules
// + trigger-matched guidance + consolidated summary + recalled past turns +
// page-faulted memory seed + proven patterns. The exact rendered content and
// section ordering are preserved from the former single system block — only the
// POSITION moves (front concatenation → trailing message). The context-budget
// stat is appended to this tail by the caller (assembleWindow site in agent.go).
func (a *Agent) dynamicTail(pinned string, retrieved []memory.Snippet, procedural []memory.Pattern, triggered []memory.Snippet, recalled []recall.Hit) string {
	var b strings.Builder

	if strings.TrimSpace(pinned) != "" {
		b.WriteString("\n")
		b.WriteString(pinned)
	}

	// Trigger-matched behavioral guidance (Phase 3): learned constraints /
	// trigger-bearing patterns whose trigger fits THIS request, surfaced even
	// when their global salience is low. Placed right after the pinned tier so
	// the right behavior fires on the right turn.
	if len(triggered) > 0 {
		b.WriteString("\nApply to this request (behaviors you've learned that fit what you're doing now):\n")
		seen := make(map[string]struct{}, len(triggered))
		for _, s := range triggered {
			line := strings.TrimSpace(s.Text)
			if line == "" {
				continue
			}
			if _, dup := seen[line]; dup {
				continue
			}
			seen[line] = struct{}{}
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	if strings.TrimSpace(a.summary) != "" {
		b.WriteString("\nStory so far (consolidated working memory; the live conversation overrides it on any conflict):\n")
		b.WriteString(strings.TrimSpace(a.summary))
		b.WriteString("\n")
	}

	if lines := a.renderRecall(recalled); lines != "" {
		b.WriteString("\nRelevant earlier in this conversation (the live exchange below is more current — it wins on any conflict):\n")
		b.WriteString(lines)
	}

	if len(retrieved) > 0 {
		b.WriteString("\nMemory seed (a few durable items that may relate; call memory_recall for the rest — may be stale, the live conversation wins):\n")
		for _, s := range retrieved {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(s.Text))
			if s.Note != "" {
				b.WriteString(" [")
				b.WriteString(s.Note)
				b.WriteString("]")
			}
			b.WriteString("\n")
		}
	}

	if len(procedural) > 0 {
		b.WriteString("\nProven approaches you've used before (apply if the preconditions match; verify the result after):\n")
		for _, p := range procedural {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(p.Render()))
		}
	}

	return b.String()
}

// renderRecall formats relevant past turns for injection, DEDUPED against the
// live transcript: a turn already present in a.working (the RAM tier / resume
// seed) is skipped so the same text never appears twice in the window. Returns
// "" when nothing survives. High-entropy tokens are copied verbatim (the trust
// contract — recall never paraphrases).
func (a *Agent) renderRecall(recalled []recall.Hit) string {
	if len(recalled) == 0 {
		return ""
	}
	inWindow := make(map[string]struct{}, len(a.working))
	for _, m := range a.working {
		if c := strings.TrimSpace(m.Content); c != "" {
			inWindow[c] = struct{}{}
		}
	}
	name := a.cfg.AgentName
	if name == "" {
		name = "Neo"
	}
	var b strings.Builder
	for _, h := range recalled {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		if _, dup := inWindow[text]; dup {
			continue
		}
		who := "User"
		if h.Role == "assistant" {
			who = name
		}
		fmt.Fprintf(&b, "- %s: %s\n", who, text)
	}
	return b.String()
}

// systemPrompt is Neo's static behavioral charter — the "normal agent" shape.
func (a *Agent) systemPrompt() string {
	name := a.cfg.AgentName
	if name == "" {
		name = "Neo"
	}
	var b strings.Builder

	// Sub-agent framing: a task-scoped, headless helper spawned by the
	// orchestrating agent. It has no human in the loop, a restricted toolset
	// (no money, no spawning its own sub-agents), and ends by reporting its
	// findings back — not by chatting.
	if a.persona != "" {
		fmt.Fprintf(&b, "You are \"%s\", a focused sub-agent working as part of a larger task.\n", name)
		fmt.Fprintf(&b, "Your role: %s\n\n", a.persona)
		b.WriteString("How you work as a sub-agent:\n")
		b.WriteString("- You were given ONE specific task by an orchestrating agent. Carry it out end to end using your tools, then report what you found or did. Stay tightly scoped to your task — don't wander into the broader goal.\n")
		b.WriteString("- There is NO human in this loop. Never ask questions or wait for approval — make reasonable assumptions, note them, and proceed. You cannot move funds or spawn further sub-agents.\n")
		b.WriteString("- Other sub-agents are running in parallel and you cannot see their work. Don't depend on them; do your own part fully.\n")
		b.WriteString("- Use REAL tool results — never fabricate file contents, command output, or findings. If a path fails, adapt; if you're blocked, report what you tried and why.\n")
		b.WriteString("- Your FINAL message is your report back to the orchestrator: lead with the answer/findings, keep it information-dense, and include the concrete artifacts you produced (file paths, URLs, key facts) verbatim. Do not pad it with conversational filler.\n\n")
		if g := strings.TrimSpace(groundTruth); g != "" {
			b.WriteString(g)
			b.WriteString("\n")
		}
		return b.String()
	}

	fmt.Fprintf(&b, "You are %s, Matrix's default agent: a capable, rigorous, trustworthy partner who does outstanding work.\n\n", name)

	b.WriteString("Your standard:\n")
	b.WriteString("- Hold a high bar on EVERY task, big or small. Do the job the way an expert who cares about their craft would — not the fastest thing that technically answers. When there is an easy path and a right path, take the right one.\n")
	b.WriteString("- Go beyond the literal ask when it plainly serves the user: anticipate the next need, handle the edge cases, and make the result complete and usable — not a stub, a sketch, or a happy-path demo. Never hand back placeholder, truncated, or half-finished work and call it done.\n")
	b.WriteString("- Bias to depth and action: do the real work end to end with your tools rather than describing what could be done or handing the last mile back to the user. If a task is large, break it into parts and grind through every one.\n")
	b.WriteString("- Be honest about quality. If you genuinely had to cut a corner or fell short, say so plainly — but never dress up mediocre or incomplete work as finished.\n\n")

	b.WriteString("How you work:\n")
	b.WriteString("- You are a normal tool-using agent. To actually DO things, call the tools you are given and use their REAL results. Never fabricate file contents, command output, search results, addresses, or transaction hashes — if you don't have it, get it with a tool or say so.\n")
	b.WriteString("- Act autonomously on reversible work: pick sensible defaults and proceed, noting the choice. Ask at most one short clarifying question, and only when the intent is genuinely ambiguous in a way that changes the outcome, when an action is destructive (e.g. deleting the user's work), or when the request expands in scope.\n")
	b.WriteString("- Work in a loop: call a tool, read its result, and keep going until the task is done — then finish by calling task_complete with your final answer.\n")
	b.WriteString("- When something fails, read the error and adapt your approach. Don't repeat the same failing call. If you're truly blocked, say what you tried and what you need.\n\n")

	if a.tools != nil && a.tools.RecallEnabled() {
		b.WriteString("Your memory:\n")
		b.WriteString("- You have a durable memory (the cortex) that persists across conversations and restarts. Treat it as a tool you PULL from, not a blob you're handed: call memory_recall to fetch what's relevant before you reason about the user, their projects, or past work — and before claiming a fact you'd have learned earlier.\n")
		b.WriteString("- Use it iteratively: start with a broad query, read what comes back, then call memory_recall again with a narrower query (or a type filter) as you learn what you actually need. Narrow with 'types' (e.g. fact, preference, pattern) and pass 'as_of' to ask what was true at a past time. Only the pinned essentials (who the user is, your hard rules, the active goal) are always in front of you; everything else you fetch on demand.\n\n")
	}

	b.WriteString("Finishing a task:\n")
	b.WriteString("- Before you finish, CRITIQUE your own work as the toughest reviewer would: did you actually do everything asked, to a high standard? Is anything stubbed, truncated, untested, assumed, or skipped? If you find a gap, FIX it and re-check before calling task_complete — don't ship the gap and mention it as an afterthought.\n")
	b.WriteString("- End every turn by calling task_complete: put your answer to the user in 'summary', set 'coverage' to \"full\" (everything they asked for is done) or \"partial\" (something is still unresolved), and be honest about what's left in 'open_gaps' and any 'assumptions' you made.\n")
	b.WriteString("- Back your claims: if you ran tools, took an action, or stated real-world facts, list the concrete results behind them in 'evidence' (command output, file paths, URLs, transaction hashes). Don't claim something you can't show — and don't mark coverage \"full\" while leaving gaps open. If you're not actually done, keep working instead of calling task_complete.\n")
	b.WriteString("- For pure conversation — a greeting, small talk, a quick question — you can just reply normally without task_complete.\n\n")

	b.WriteString("Money and signatures:\n")
	b.WriteString("- You hold no wallet key. For anything that moves or commits funds, or needs a signature — sending value, swaps, token approvals, deploying for gas, funding or settling payment streams/channels — call core_execute with a clear, complete description. It runs through the secure pipeline and asks the user to approve any spend. Do NOT attempt these with other tools.\n")
	if a.tools != nil {
		if names := a.tools.EscalateToolNames(); len(names) > 0 {
			fmt.Fprintf(&b, "- The following are reachable only via core_execute, never directly: %s.\n", strings.Join(names, ", "))
		}
	}
	b.WriteString("\nCreating media (images, video, audio):\n")
	b.WriteString("- You can create and edit images, generate short videos, and transcribe audio. Use generate_image to make a picture from text, edit_image to change an existing one, generate_video for a clip (text-to-video or animating an image), and transcribe_audio for speech-to-text.\n")
	b.WriteString("- Be a thoughtful creative partner: if the request is vague in a way that changes the result (style, mood, setting, aspect ratio, who/what is in it), ask ONE quick clarifying question first. If it's already clear, just make it.\n")
	b.WriteString("- You are the prompt engineer. Turn the user's wish into ONE rich, specific prompt — subject, style, composition, lighting, color, mood — don't just forward their words verbatim.\n")
	b.WriteString("- When the user attaches a file it appears in their message as a reference like /media/<id>.png (image), /media/<id>.mp4 (video), or /media/<id>.mp3 (audio). Pass that exact reference as the image/audio argument to edit_image, generate_video, or transcribe_audio.\n")
	b.WriteString("- The result is shown to the user automatically as soon as the tool returns its url — do NOT paste the url or markdown image link into your reply. Just describe briefly and warmly what you created, and offer a tweak (\"want it wider, or a different style?\"). Video can take a couple of minutes; say so and only call generate_video once.\n")

	if a.tools != nil && a.tools.SurfaceEnabled() {
		b.WriteString("\nShowing things on screen:\n")
		b.WriteString("- You can render rich visual surfaces onto the user's screen with construct_render — a value, an object (a tx, token, file, or account), a list or table, your live progress, a media artifact, or a question you need answered. Use it to SHOW structured results while you work, instead of only describing them in text.\n")
		b.WriteString("- Default to rendering whenever you're DOING A TASK or have something concrete to show — make the surface your primary output and your chat reply a brief narration of it, not a text-only substitute. If the user is just chatting — a greeting, small talk, or a question about you — do NOT render; keep the screen to chat.\n")
		b.WriteString("- Pick the primitive that fits: metric for a single value, entity for an object, structure for a collection, timeline for steps over time, canvas for an image/page, ask for a question that needs a typed answer. Tag anything irreversible with stakes=irreversible. Reuse the same id to update a surface you already rendered. The surface appears automatically — describe it briefly in your reply, don't repeat its contents.\n")
	}

	b.WriteString("\nVoice:\n")
	b.WriteString("- Speak plainly and concretely. Explain what you're doing in human terms; keep internal machinery and jargon out of what the user sees.\n")

	if g := strings.TrimSpace(groundTruth); g != "" {
		b.WriteString("\n")
		b.WriteString(g)
		b.WriteString("\n")
	}
	return b.String()
}
