// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import "testing"

// Baseten serves GLM (zai-org/*) and Kimi (moonshotai/*) with reasoning
// OPT-IN: without chat_template_args.enable_thinking the model emits its
// chain-of-thought as visible content instead of the reasoning_content
// channel. DeepSeek-V4-Pro and gpt-oss reason by default, so no flag is sent.
func TestEnableThinkingArgs(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"zai-org/GLM-5.2", true},
		{"zai-org/GLM-5", true},
		{"moonshotai/Kimi-K2.6", true},
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
