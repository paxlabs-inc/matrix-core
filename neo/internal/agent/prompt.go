// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	_ "embed"
	"fmt"
	"strings"

	"matrix/neo/internal/memory"
)

// groundTruth is Neo's always-injected factual grounding (who it is, that
// Paxeer is a real live chain, the canonical RPC/explorer/docs endpoints, and
// how to answer chain questions with its read tools instead of blind web
// search). Embedded so it ships in the binary and can't drift from the build.
//
//go:embed knowledge.md
var groundTruth string

// buildSystem composes the single system block injected each turn:
// behavior + pinned identity/rules/goal + consolidated summary + page-faulted
// memory + proven patterns. Re-derived every turn so nothing here drifts (the
// budget stat is appended by the caller).
func (a *Agent) buildSystem(pinned string, retrieved []memory.Snippet, procedural []memory.Pattern) string {
	var b strings.Builder
	b.WriteString(a.systemPrompt())

	if strings.TrimSpace(pinned) != "" {
		b.WriteString("\n")
		b.WriteString(pinned)
	}

	if strings.TrimSpace(a.summary) != "" {
		b.WriteString("\nStory so far (consolidated working memory; the live conversation overrides it on any conflict):\n")
		b.WriteString(strings.TrimSpace(a.summary))
		b.WriteString("\n")
	}

	if len(retrieved) > 0 {
		b.WriteString("\nRelevant memory (durable; may be stale — the live conversation wins):\n")
		for _, s := range retrieved {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(s.Text))
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

// systemPrompt is Neo's static behavioral charter — the "normal agent" shape.
func (a *Agent) systemPrompt() string {
	name := a.cfg.AgentName
	if name == "" {
		name = "Neo"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, Matrix's default agent: a capable, trustworthy, conversational assistant.\n\n", name)

	b.WriteString("How you work:\n")
	b.WriteString("- You are a normal tool-using agent. To actually DO things, call the tools you are given and use their REAL results. Never fabricate file contents, command output, search results, addresses, or transaction hashes — if you don't have it, get it with a tool or say so.\n")
	b.WriteString("- Act autonomously on reversible work: pick sensible defaults and proceed, noting the choice. Ask at most one short clarifying question, and only when the intent is genuinely ambiguous in a way that changes the outcome, when an action is destructive (e.g. deleting the user's work), or when the request expands in scope.\n")
	b.WriteString("- Work in a loop: call a tool, read its result, and keep going until the task is done — then give a clear, useful final answer. Once you can answer, stop calling tools and answer.\n")
	b.WriteString("- When something fails, read the error and adapt your approach. Don't repeat the same failing call. If you're truly blocked, say what you tried and what you need.\n\n")

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

	b.WriteString("\nVoice:\n")
	b.WriteString("- Speak plainly and concretely. Explain what you're doing in human terms; keep internal machinery and jargon out of what the user sees.\n")

	if g := strings.TrimSpace(groundTruth); g != "" {
		b.WriteString("\n")
		b.WriteString(g)
		b.WriteString("\n")
	}
	return b.String()
}
