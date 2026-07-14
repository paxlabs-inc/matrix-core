// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"

	"matrix/neo/internal/llm"
)

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
