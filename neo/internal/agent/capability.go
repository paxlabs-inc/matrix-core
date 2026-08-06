// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// capability.go renders the resident capability surface (epistemic-core
// req.2): the FULL load-bearing facts of the agent's own architecture —
// external API surface, architectural is/is-not facts, and accumulated failure
// patterns — generated from the self-model artifact. Callable tools are sent
// only as structured provider schemas, never duplicated here as prose.
// It lives in the byte-stable system prefix so a false self-premise ("I have
// an OpenAI-compatible API") collides with the resident truth at the moment
// it would form, instead of one recallSelf tool call away (which, at
// generation time, is exactly too far). recallSelf remains for source-level
// depth (req.2.2).
package agent

import (
	"strings"
)

// CapabilitySurface is the resolved capability-surface material the agent
// renders resident. API/Is/IsNot come from the durable self-model's derived
// surface facet (written by the serving layer from its live route table);
// FailurePatterns from the self-authored how-I-fail beliefs. The tool
// inventory is derived at render time from the agent's own advertised
// schemas, so it can never disagree with what the agent can actually call.
type CapabilitySurface struct {
	API             []string
	Is              []string
	IsNot           []string
	FailurePatterns []string
	// StructuralSummary is the compressed codegraph-derived summary (kept for
	// depth beneath the surface facts; renders after them).
	StructuralSummary string
}

// capabilityUnknown renders the honest gap line for a section the self-model
// artifact does not carry (req.2.3): an explicit unknown, never a fabricated
// fact, with the pull path to go deeper.
func capabilityUnknown(what string) string {
	return "- UNKNOWN — your self-model carries no " + what + " facts. Do not guess or assume them; check with memory_recall \"self:\" or say you don't know.\n"
}

// renderCapabilitySurface renders the resident capability-surface section of
// the stable prefix. Byte-stable per agent: it reads only construction-time
// state, so it is byte-identical across every step and turn.
func (a *Agent) renderCapabilitySurface() string {
	cs := a.capability
	if cs == nil {
		cs = &CapabilitySurface{}
	}
	var b strings.Builder
	b.WriteString("\nYour capability surface (derived from your self-model — ground every claim about yourself HERE, never from what sounds plausible):\n")

	b.WriteString("How things reach you (your true external API):\n")
	if len(cs.API) == 0 {
		b.WriteString(capabilityUnknown("external API surface"))
	}
	for _, line := range cs.API {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString("What you are:\n")
	if len(cs.Is) == 0 {
		b.WriteString(capabilityUnknown("architectural is-"))
	}
	for _, line := range cs.Is {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString("What you are NOT (claims that contradict these are false, however plausible they feel):\n")
	if len(cs.IsNot) == 0 {
		b.WriteString(capabilityUnknown("architectural is-not"))
	}
	for _, line := range cs.IsNot {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString("Callable tools are defined exclusively by the structured schemas attached to the current provider request. Never infer an unadvertised tool.\n")

	if len(cs.FailurePatterns) > 0 {
		b.WriteString("How you tend to fail (self-authored from real past failures — actively avoid these):\n")
		for _, line := range cs.FailurePatterns {
			if line = strings.TrimSpace(line); line != "" {
				b.WriteString("- ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}

	if sm := strings.TrimSpace(cs.StructuralSummary); sm != "" {
		b.WriteString("How you are built (structural summary; page symbol-level depth with memory_recall \"self:<Symbol>\"):\n")
		b.WriteString(sm)
		b.WriteString("\n")
	}

	return b.String()
}

// firstLine returns the first non-empty line of s, trimmed — the one-line
// semantics of a tool schema description.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
