// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import "testing"

// GLM (zai-org/*) is served by Z.ai, whose native `thinking` block defaults
// ON — so the client pins it explicitly BOTH ways from the per-client flag.
// Kimi (moonshotai/*) stays Baseten-served with reasoning OPT-IN via
// chat_template_args.enable_thinking. DeepSeek-V4-Pro and gpt-oss reason by
// default, so nothing is sent for them.
func TestZaiThinking(t *testing.T) {
	cases := []struct {
		model   string
		enabled bool
		want    string // "" = nil expected
	}{
		{"zai-org/GLM-5.2", true, "enabled"},
		{"zai-org/GLM-5.2", false, "disabled"},
		{"zai-org/GLM-5", true, "enabled"},
		{"moonshotai/Kimi-K2.6", true, ""},
		{"deepseek-ai/DeepSeek-V4-Pro", true, ""},
		{"accounts/fireworks/models/gpt-oss-120b", false, ""},
	}
	for _, c := range cases {
		got := zaiThinking(c.model, c.enabled)
		if c.want == "" {
			if got != nil {
				t.Errorf("%s enabled=%v: want nil, got %+v", c.model, c.enabled, got)
			}
			continue
		}
		if got == nil || got.Type != c.want {
			t.Errorf("%s enabled=%v: want thinking type %q, got %+v", c.model, c.enabled, c.want, got)
		}
	}
}

func TestEnableThinkingArgs(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"moonshotai/Kimi-K2.6", true},
		// GLM moved off Baseten to Z.ai's native thinking block — no
		// chat_template_args for it anymore.
		{"zai-org/GLM-5.2", false},
		{"zai-org/GLM-5", false},
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
