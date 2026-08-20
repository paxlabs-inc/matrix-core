// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package converge

type Posture string

const (
	Conversation Posture = "conversation"
	Exploration  Posture = "exploration"
	Execution    Posture = "execution"
)

type Limits struct {
	ProviderCalls           uint64 `json:"provider_calls"`
	ToolCalls               uint64 `json:"tool_calls"`
	CumulativeInputTokens   uint64 `json:"cumulative_input_tokens"`
	SynthesisReserve        uint64 `json:"synthesis_reserve"`
	IdenticalFailureRepeats uint64 `json:"identical_failure_repeats"`
	EpistemicRepairAttempts uint64 `json:"epistemic_repair_attempts"`
	CassandraO1Repairs      uint64 `json:"cassandra_o1_repairs"`
	DeliveryAttempts        uint64 `json:"delivery_attempts"`
	PreferredInputTokens    uint64 `json:"preferred_input_tokens"`
	HardInputTokens         uint64 `json:"hard_input_tokens"`
	ResponseReserveTokens   uint64 `json:"response_reserve_tokens"`
}

func ForPosture(posture Posture) Limits {
	base := Limits{
		SynthesisReserve: 1, EpistemicRepairAttempts: 2,
		CassandraO1Repairs: 2, DeliveryAttempts: 3,
		PreferredInputTokens: 96_000, HardInputTokens: 160_000,
		ResponseReserveTokens: 8_192,
	}
	switch posture {
	case Exploration:
		base.ProviderCalls, base.ToolCalls = 8, 20
		base.CumulativeInputTokens, base.IdenticalFailureRepeats = 600_000, 2
	case Execution:
		base.ProviderCalls, base.ToolCalls = 20, 128
		base.CumulativeInputTokens, base.IdenticalFailureRepeats = 1_200_000, 2
	default:
		base.ProviderCalls, base.ToolCalls = 3, 4
		base.CumulativeInputTokens, base.IdenticalFailureRepeats = 220_000, 1
	}
	return base
}

// ForPosture intentionally has no provider-capacity input: turn limits are
// product convergence controls, never fractions of a model context window.
