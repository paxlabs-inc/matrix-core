// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package converge

import "testing"

func TestPostureLimitsAreSeparatedAndProviderIndependent(t *testing.T) {
	conversation := ForPosture(Conversation)
	exploration := ForPosture(Exploration)
	execution := ForPosture(Execution)
	if conversation.ProviderCalls != 3 || conversation.ToolCalls != 4 || conversation.CumulativeInputTokens != 220_000 ||
		exploration.ProviderCalls != 8 || exploration.ToolCalls != 20 || exploration.CumulativeInputTokens != 600_000 ||
		execution.ProviderCalls != 20 || execution.ToolCalls != 128 || execution.CumulativeInputTokens != 1_200_000 {
		t.Fatalf("unexpected posture tuning: %#v %#v %#v", conversation, exploration, execution)
	}
	for _, limits := range []Limits{conversation, exploration, execution} {
		if limits.PreferredInputTokens != 96_000 || limits.HardInputTokens != 160_000 || limits.ResponseReserveTokens == 0 || limits.SynthesisReserve != 1 {
			t.Fatalf("limits were conflated: %#v", limits)
		}
	}
}
