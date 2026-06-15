// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"strings"
	"testing"
)

func TestAggregateResults(t *testing.T) {
	out := aggregateResults([]subResult{
		{index: 1, name: "Go Analyst", persona: "senior Go reviewer", text: "Found a race in run().", ok: true},
		{index: 2, name: "Security", persona: "auditor", text: "timed out", ok: false},
	})
	if !strings.Contains(out, "(1 succeeded)") {
		t.Errorf("digest should report 1 of 2 succeeded:\n%s", out)
	}
	if !strings.Contains(out, "## 01 · Go Analyst — senior Go reviewer") {
		t.Errorf("digest missing labelled section for agent 1:\n%s", out)
	}
	if !strings.Contains(out, "[did not fully complete]") {
		t.Errorf("digest should mark the failed sub-agent honestly:\n%s", out)
	}
	if !strings.Contains(out, "Found a race in run().") {
		t.Errorf("digest should carry the sub-agent's verbatim findings:\n%s", out)
	}
}

// TestSwarmActiveGuard asserts the recursion backstop: a context already inside
// a swarm reports active, so a sub-agent that reaches runSwarm is refused.
func TestSwarmActiveGuard(t *testing.T) {
	if swarmActive(context.Background()) {
		t.Fatal("a plain context must not be marked swarm-active")
	}
	if !swarmActive(withSwarmActive(context.Background())) {
		t.Fatal("withSwarmActive must mark the context active")
	}
}
