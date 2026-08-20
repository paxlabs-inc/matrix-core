// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/tools"
)

// spawnWiredTools returns a real tool Manager with the swarm runner wired, so
// SpawnEnabled() is true — the top-level agent can actually decompose. The
// runner is a no-op (never invoked in these unit tests); wiring it is what turns
// the decomposition router on, exactly as the engine does in production.
func spawnWiredTools(t *testing.T) *tools.Manager {
	t.Helper()
	m := &tools.Manager{}
	m.SetSwarm(func(context.Context, []tools.SubagentSpec) (string, error) { return "", nil }, 8)
	return m
}

// TestDecompositionRouterSurfacesOwnLimits proves the self-aware decomposition
// router (self-model task 4.2, req.8.1/8.2): a top-level agent that CAN spawn is
// handed its OWN context limit and current fill at the decision point, framed as
// a self-known-limits judgement rather than a hardcoded threshold. The limit is
// the real ContextWindowTokens the budget math meters against — not a magic
// constant.
func TestDecompositionRouterSurfacesOwnLimits(t *testing.T) {
	cfg := config.Default()
	cfg.ContextWindowTokens = 256000
	a := New(Options{Config: cfg, Tools: spawnWiredTools(t)})

	line := a.decompositionSelfKnowledge(37)
	if line == "" {
		t.Fatal("a top-level agent that can spawn must be given its own limits at the decision point")
	}
	if !strings.Contains(line, strconv.Itoa(cfg.ContextWindowTokens)) {
		t.Errorf("router must surface the agent's REAL context limit (%d); got:\n%s", cfg.ContextWindowTokens, line)
	}
	if !strings.Contains(line, "37%") {
		t.Errorf("router must surface the live context fill; got:\n%s", line)
	}
	if !strings.Contains(line, "spawn_subagents") {
		t.Errorf("router must point at the decomposition tool; got:\n%s", line)
	}
	// Self-aware, NOT a fixed cutoff (req.8.2): the framing hands the judgement to
	// the agent's known limits.
	if !strings.Contains(strings.ToLower(line), "not a fixed cutoff") {
		t.Errorf("router must decide by known limits, not a hardcoded threshold; got:\n%s", line)
	}
	// It rides the turn-varying tail (never the byte-stable charter prefix), so it
	// cannot break the prompt-cache invariant.
	tail := a.budgetTail(37)
	if !strings.Contains(tail, "[context: 37% used]") || !strings.Contains(tail, "spawn_subagents") {
		t.Errorf("budgetTail must carry both the context stat and the router:\n%s", tail)
	}
}

// TestDecompositionRouterTopLevelOnly proves the top-level-only / no-recursion
// constraint holds at the guidance layer (req.8.3): a SUB-AGENT (persona set,
// restricted surface) is never handed the decomposition router — it cannot
// spawn, so it is never given a decision it cannot act on.
func TestDecompositionRouterTopLevelOnly(t *testing.T) {
	cfg := config.Default()
	cfg.ContextWindowTokens = 256000
	sub := New(Options{
		Config:        cfg,
		Tools:         spawnWiredTools(t),
		Persona:       "a scoped Go reviewer",
		RestrictTools: true,
	})
	if got := sub.decompositionSelfKnowledge(50); got != "" {
		t.Errorf("a sub-agent must never see the decomposition router; got:\n%s", got)
	}
	// And its budget tail still carries the plain context stat (unchanged behavior
	// for sub-agents).
	if tail := sub.budgetTail(50); !strings.Contains(tail, "[context: 50% used]") || strings.Contains(tail, "spawn_subagents") {
		t.Errorf("a sub-agent's tail must keep the stat but omit the router:\n%s", tail)
	}
}

// TestDecompositionRouterAbsentWithoutSpawn proves the router informs only a
// decision the agent can act on: with the swarm runner NOT wired (spawn
// unavailable this session), a top-level agent is given no decomposition
// guidance.
func TestDecompositionRouterAbsentWithoutSpawn(t *testing.T) {
	cfg := config.Default()
	cfg.ContextWindowTokens = 256000
	a := New(Options{Config: cfg, Tools: &tools.Manager{}}) // no SetSwarm → SpawnEnabled() false
	if got := a.decompositionSelfKnowledge(80); got != "" {
		t.Errorf("without a wired swarm there is no decision to inform; got:\n%s", got)
	}
	if tail := a.budgetTail(80); strings.Contains(tail, "spawn_subagents") {
		t.Errorf("no router when spawn is unavailable:\n%s", tail)
	}
}
