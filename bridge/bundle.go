// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package bridge

import (
	"fmt"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// FormatBundle renders a *cortex.Bundle into the canonical text shape
// that fills `{cortex.bundle}` in MCL prompt interpolation.
//
// The format is stable, deterministic, and easy for an LLM to parse:
//
//	## Pinned
//	- matrix://cortex/Identity/<ulid>#<v> — <summary>
//	- ...
//
//	## Frame-relevant
//	- ...
//
//	## Outcomes
//	- ...
//
//	## Reachable (lazy)
//	- matrix://cortex/...#<v>
//
//	---
//	form=medium  total_tokens=N  trimmed=N  latency_ms=N
//
// Empty tiers are omitted. The Reachable section only renders when
// bundle.ReachableURIs is non-empty. Sections are emitted in fixed
// order regardless of caller-requested IncludeTiers so the output
// shape is predictable.
//
// Pure function: same Bundle bytes → same string bytes.
func FormatBundle(b *cortex.Bundle) string {
	if b == nil {
		return "(no cortex bundle)"
	}

	var sb strings.Builder

	writeTier(&sb, "Pinned", b.Pinned, b.Rendered)
	writeTier(&sb, "Frame-relevant", b.FrameRelevant, b.Rendered)
	writeTier(&sb, "Outcomes", b.Outcomes, b.Rendered)

	if len(b.ReachableURIs) > 0 {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("## Reachable (lazy)\n")
		for _, u := range b.ReachableURIs {
			sb.WriteString("- ")
			sb.WriteString(string(u))
			sb.WriteByte('\n')
		}
	}

	if sb.Len() == 0 {
		sb.WriteString("(empty cortex bundle)\n")
	}

	// Trailer with compile_metadata-like stats; spec research/03 §2.4
	// "compile_metadata" maps to this footer at the prompt boundary.
	sb.WriteString("\n---\n")
	fmt.Fprintf(&sb, "form=%s  total_tokens=%d  trimmed=%d  latency_ms=%d\n",
		formOrDefault(b.Form), b.TotalTokens, b.Trimmed, b.LatencyMS)

	return sb.String()
}

func writeTier(sb *strings.Builder, label string, mems []*memory.Memory, rendered map[memory.ID]string) {
	if len(mems) == 0 {
		return
	}
	if sb.Len() > 0 {
		sb.WriteByte('\n')
	}
	sb.WriteString("## ")
	sb.WriteString(label)
	sb.WriteByte('\n')
	for _, m := range mems {
		uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
		summary := rendered[m.Head.ID]
		if summary == "" {
			summary = m.Version.Forms.Medium
		}
		if summary == "" {
			summary = m.Version.Forms.Short
		}
		sb.WriteString("- ")
		sb.WriteString(string(uri))
		if summary != "" {
			sb.WriteString(" — ")
			// Collapse newlines so each memory is one bullet line.
			sb.WriteString(strings.ReplaceAll(summary, "\n", " "))
		}
		sb.WriteByte('\n')
	}
}

func formOrDefault(f any) string {
	s := fmt.Sprintf("%v", f)
	if s == "" {
		return "medium"
	}
	return s
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
