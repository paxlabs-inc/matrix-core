// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/tools"
)

// TestSubagentInheritsSelfModelRoleAndBounds proves the sub-agent alignment
// contract (self-model task 4.3, req.9.1): a spawned sub-agent's charter carries
// the inherited shared self-model (structural summary + failure patterns), an
// explicit ephemeral-role awareness, its bounds (restricted surface, budget),
// and its scoped role — so it acts as the SAME mind on a scoped slice.
func TestSubagentInheritsSelfModelRoleAndBounds(t *testing.T) {
	cfg := config.Default()
	cfg.StepBudget = 40
	const selfModel = "Architecture (how you are built): Neo assembles system, transcript, then a trailing memory tail; core_execute is the value-transfer wall.\nFailure patterns to avoid:\n- [failure-mode:no_progress_stall] I tend to die by repeating the same step."
	sub := New(Options{
		Config:        cfg,
		Tools:         &tools.Manager{},
		Persona:       "a senior Go reviewer focused on concurrency",
		SelfModel:     selfModel,
		RestrictTools: true,
	})

	sp := sub.systemPrompt()

	// Inherited self-model (structural summary + failure patterns).
	if !strings.Contains(sp, "core_execute is the value-transfer wall") {
		t.Error("sub-agent must inherit the shared structural self-summary")
	}
	if !strings.Contains(sp, "no_progress_stall") {
		t.Error("sub-agent must inherit the shared how-I-fail patterns")
	}
	// Explicit ephemeral-role awareness.
	if !strings.Contains(strings.ToLower(sp), "ephemeral") || !strings.Contains(strings.ToLower(sp), "scoped instance") {
		t.Errorf("sub-agent must carry explicit ephemeral-role awareness:\n%s", sp)
	}
	// Bounds: positive read-only surface, no mutation / action / recursion, and a budget.
	lower := strings.ToLower(sp)
	if !strings.Contains(sp, "Your bounds:") || !strings.Contains(lower, "read-only research worker") ||
		!strings.Contains(lower, "shell") || !strings.Contains(lower, "memory writes") ||
		!strings.Contains(lower, "external actions") || !strings.Contains(lower, "spawning sub-agents") {
		t.Errorf("sub-agent must state its restricted-surface bounds:\n%s", sp)
	}
	if !strings.Contains(sp, "40 tool-call steps") {
		t.Errorf("sub-agent must state its real step budget:\n%s", sp)
	}
	// Its scoped role.
	if !strings.Contains(sp, "a senior Go reviewer focused on concurrency") {
		t.Error("sub-agent must carry its scoped role")
	}

	// The sub-agent surface is value-transfer-free (req.9.3): the alignment
	// contract never weakens the wall.
	if err := tools.AssertNoValueTransferTools(sub.AdvertisedTools()); err != nil {
		t.Errorf("sub-agent surface must pass AssertNoValueTransferTools: %v", err)
	}
}

// TestTopLevelAgentHasNoSubagentContract proves the alignment block is
// sub-agent-only: the top-level conversational agent (no persona) never carries
// the ephemeral-role / inherited-self-model framing (it holds its self-model
// through its own memory surface instead).
func TestTopLevelAgentHasNoSubagentContract(t *testing.T) {
	top := New(Options{Config: config.Default(), Tools: &tools.Manager{}})
	sp := top.systemPrompt()
	if strings.Contains(strings.ToLower(sp), "ephemeral, scoped instance") {
		t.Error("the top-level agent must not carry the sub-agent ephemeral-role framing")
	}
	if strings.Contains(sp, "Your bounds:") {
		t.Error("the top-level agent must not carry the sub-agent bounds block")
	}
}
