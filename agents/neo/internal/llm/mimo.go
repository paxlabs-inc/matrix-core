// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"strings"

	mcllm "centra/core/mcl/llm"
	runtimeprovider "centra/agents/neo/internal/runtime/provider"
)

// mimo.go is Neo's native adapter for Xiaomi MiMo (MiMo-V2.5-Pro, the current
// main/cheap/consolidation model). It mirrors, on the client side, the two
// server flags Xiaomi's vLLM recipe recommends for MiMo:
//
//	--tool-call-parser mimo     → parseMimoToolCalls   (the qwen3_xml tag grammar)
//	--reasoning-parser  mimo    → mimoReasoningContent (DeepSeek-compatible thinking)
//
// The provider (api.xiaomimimo.com, and any self-hosted vLLM) does not always
// parse tool calls server-side, so MiMo emits them inline in `content` using
// the qwen3_xml tag grammar. And MiMo uses DeepSeek-style thinking, which
// REQUIRES prior assistant reasoning_content to be replayed through a
// multi-turn tool conversation — dropping it (or omitting the empty string on a
// tool-call turn) degrades or 400s the next turn. This file owns both halves so
// MiMo compatibility lives in one legible place instead of scattered fallbacks.

// isMimoFamily reports whether the model is any MiMo v2.5 variant — the text
// planner (mimo-v2.5-pro, matched by mcllm.IsXiaomiModel) OR the omni vision id
// (mimo-v2.5, used only for DOJO desktop_look grounding). Both take Xiaomi's
// thinking control and the max_completion_tokens field, so the omni grounder's
// visible JSON is never starved by a reasoning burst (wave-1 empty-content
// gotcha). It deliberately does NOT feed XiaomiModelID (which would rewrite the
// omni id to the planner id) or the multi-turn reasoning_content replay flag
// (omni is a stateless single-turn call).
func isMimoFamily(model string) bool {
	if mcllm.IsXiaomiModel(model) {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "xiaomimimo/")
	return strings.HasPrefix(m, "mimo-v2.5")
}

func parseMimoToolCalls(content string) (string, []ToolCall) {
	cleaned, normalized, _, err := runtimeprovider.ParseMiMoToolCalls(content)
	if err != nil {
		if marker := strings.Index(content, "<tool_call>"); marker >= 0 {
			return strings.TrimSpace(content[:marker]), nil
		}
		return content, nil
	}
	if len(normalized) == 0 {
		return cleaned, nil
	}
	calls := make([]ToolCall, 0, len(normalized))
	for _, call := range normalized {
		calls = append(calls, ToolCall{
			ID:   call.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      call.Name,
				Arguments: string(call.Arguments),
			},
		})
	}
	return cleaned, calls
}

// mimoReasoningContent is the request-side half of the MiMo reasoning adapter
// (the `--reasoning-parser mimo` counterpart). MiMo uses DeepSeek-compatible
// thinking: prior assistant reasoning_content must be replayed through a
// multi-turn tool conversation, and on an assistant turn that is part of a
// tool-call chain the field must be present VERBATIM — including the empty
// string "" — or a strict deserializer 400s ("reasoning_content" required) and
// the model loses the thread it was mid-thought on.
//
// So for a MiMo request every assistant message carries reasoning_content
// (a non-nil pointer, possibly to ""); user / tool / system messages never do.
// The pointer lets an explicit "" ride the wire that `omitempty` would drop.
// A resumed conversation whose durable seed lost the reasoning (it is never
// persisted) degrades gracefully: the assistant turn ships reasoning_content:""
// which MiMo accepts, rather than an absent field it may reject.
func mimoReasoningContent(role, reasoning string) *string {
	if role != RoleAssistant {
		return nil
	}
	return &reasoning
}
