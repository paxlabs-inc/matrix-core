// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import "testing"

// Grok (grok-*) is served by xAI. Only xiaomimimo/mimo-v2.5-pro-ultraspeed honors the native
// `reasoning_effort` field, pinned EXPLICITLY both ways from the per-client
// flag (ON → "medium", OFF → "none"); other Grok models (grok-build-*) ignore it
// so it is omitted. Kimi (moonshotai/*) stays Baseten-served with reasoning
// OPT-IN via chat_template_args.enable_thinking. DeepSeek-V4-Pro and gpt-oss
// reason by default, so nothing is sent for them.
func TestSupportsReasoningEffort(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"xiaomimimo/mimo-v2.5-pro-ultraspeed", true},
		{"xiaomimimo/mimo-v2.5-pro-fast", true},
		{"xiaomimimo/mimo-v2.5-pro-ultraspeed", true},
		{"grok-build-0.1", false},
		{"grok-4.20-0309-non-reasoning", false},
		{"moonshotai/Kimi-K2.6", false},
		{"deepseek-ai/DeepSeek-V4-Pro", false},
		{"accounts/fireworks/models/gpt-oss-120b", false},
	}
	for _, c := range cases {
		if got := supportsReasoningEffort(c.model); got != c.want {
			t.Errorf("%s: supportsReasoningEffort = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestReasoningEffort(t *testing.T) {
	if got := reasoningEffort(true); got != "medium" {
		t.Errorf("reasoningEffort(true) = %q, want %q", got, "medium")
	}
	if got := reasoningEffort(false); got != "none" {
		t.Errorf("reasoningEffort(false) = %q, want %q", got, "none")
	}
}

func TestEnableThinkingArgs(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"moonshotai/Kimi-K2.6", true},
		// Grok uses xAI's native reasoning_effort field — no chat_template_args.
		{"xiaomimimo/mimo-v2.5-pro-ultraspeed", false},
		{"grok-build-0.1", false},
		{"deepseek-ai/DeepSeek-V4-Pro", false},
		{"openai/gpt-oss-120b", false},
		{"accounts/fireworks/models/gpt-oss-120b", false},
		{"accounts/fireworks/models/kimi-k2p6", false},
		{"Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8", false},
	}
	for _, c := range cases {
		args := enableThinkingArgs(c.model)
		if c.want {
			if args == nil {
				t.Errorf("%s: want enable_thinking, got nil", c.model)
				continue
			}
			if v, ok := args["enable_thinking"].(bool); !ok || !v {
				t.Errorf("%s: enable_thinking = %v, want true", c.model, args["enable_thinking"])
			}
		} else if args != nil {
			t.Errorf("%s: want nil (reasons by default / wrong provider param), got %v", c.model, args)
		}
	}
}
