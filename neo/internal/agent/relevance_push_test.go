// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"bytes"
	"context"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// Q2 — first-message relevance push. These drive the REAL turn loop (agent.Chat)
// over a REAL Pager + cortex store, with only the model boundary scripted. They
// prove the opening turn carries relevance-matched memory (not just the
// recency-based, query-independent Activate tiers) WITHOUT a reactive
// memory_recall tool call, and that the push is first-turn-only.

const relevanceLabel = "Relevant to your message"

// newRelevanceAgent builds a continuous-memory agent over a real pager, with the
// model served by the scripted SSE endpoint. lastBody captures the exact window
// the agent sent so a test can assert what the model actually saw.
func newRelevanceAgent(t *testing.T, cfg config.Config, lastBody *[]byte, mu *sync.Mutex) *Agent {
	t.Helper()
	srv := sseChatServer(t, "noted.", lastBody, mu)
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })

	// Seed a distinctive durable fact the push must surface. RememberFact writes
	// durably (the async embed worker is irrelevant — Retrieve's always-on
	// salience lane finds it via the type-filtered cortex scan).
	if _, err := pager.RememberFact(context.Background(), "the deploy sentinel token is zephyrdeploy42"); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}

	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: "conv-relevance",
	})
	return a
}

func relevanceCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-relevance-push"
	return cfg
}

func snapshotBody(lastBody *[]byte, mu *sync.Mutex) []byte {
	mu.Lock()
	defer mu.Unlock()
	return append([]byte(nil), (*lastBody)...)
}

// TestFirstTurnRelevancePush_InjectsOnOpeningTurnOnly proves the push fires on
// the FIRST turn (label + the seeded fact appear in the window) and does NOT
// fire on the second turn (both absent), so the pull-not-push philosophy resumes.
func TestFirstTurnRelevancePush_InjectsOnOpeningTurnOnly(t *testing.T) {
	var lastBody []byte
	var mu sync.Mutex
	cfg := relevanceCfg(t)
	if !cfg.FirstTurnRelevancePush {
		t.Fatal("precondition: FirstTurnRelevancePush must default on")
	}
	a := newRelevanceAgent(t, cfg, &lastBody, &mu)

	// Turn 1: the opening message. The push must surface the seeded fact.
	if err := a.Chat(context.Background(), "where do we deploy the service?"); err != nil {
		t.Fatalf("Chat turn 1: %v", err)
	}
	turn1 := snapshotBody(&lastBody, &mu)
	if !bytes.Contains(turn1, []byte(relevanceLabel)) {
		t.Fatalf("turn 1 window missing relevance-push label %q:\n%s", relevanceLabel, turn1)
	}
	if !bytes.Contains(turn1, []byte("zephyrdeploy42")) {
		t.Fatalf("turn 1 window missing the seeded fact token (push did not surface it):\n%s", turn1)
	}

	// Turn 2: a follow-up. The push is first-turn-only, so neither the label nor
	// the (non-pinned, non-episode) fact should be in the window.
	if err := a.Chat(context.Background(), "and what region?"); err != nil {
		t.Fatalf("Chat turn 2: %v", err)
	}
	turn2 := snapshotBody(&lastBody, &mu)
	if bytes.Contains(turn2, []byte(relevanceLabel)) {
		t.Fatalf("turn 2 window unexpectedly carries the relevance-push label — push must be first-turn-only:\n%s", turn2)
	}
	if bytes.Contains(turn2, []byte("zephyrdeploy42")) {
		t.Fatalf("turn 2 window unexpectedly carries the seeded fact — it should only arrive via the first-turn push:\n%s", turn2)
	}
}

// TestFirstTurnRelevancePush_DisabledByFlag proves the flag gates the behavior:
// with FirstTurnRelevancePush off, the opening turn carries no push block even
// though the fact exists in cortex.
func TestFirstTurnRelevancePush_DisabledByFlag(t *testing.T) {
	var lastBody []byte
	var mu sync.Mutex
	cfg := relevanceCfg(t)
	cfg.FirstTurnRelevancePush = false
	a := newRelevanceAgent(t, cfg, &lastBody, &mu)

	if err := a.Chat(context.Background(), "where do we deploy the service?"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := snapshotBody(&lastBody, &mu)
	if bytes.Contains(body, []byte(relevanceLabel)) {
		t.Fatalf("relevance-push label present despite the flag being off:\n%s", body)
	}
}
