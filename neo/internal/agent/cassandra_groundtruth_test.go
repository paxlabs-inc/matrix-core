// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/cortex"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// This file proves PROPERTY 5 (task 6.5): cortex stays ground truth — the
// in-place mod is an in-WINDOW overlay only and NEVER reaches the durable
// transcript (AppendMessage). It drives the REAL agent.Chat loop on the
// continuous-memory path with a scripted spiral whose assistant turns carry
// content, so the controller folds doubt into the in-window copy while the
// durable copy retains exactly what the agent originally said.

const gtOriginalContent = "Let me search for the same thing again."

func TestCassandraCortexGroundTruth(t *testing.T) {
	// A spiral WITH assistant content each step: same content + same tool call, so
	// the loop trigger fires and the controller folds a doubt mod into the
	// in-window assistant content — while cmRecordAssistant already durably stored
	// the ORIGINAL content before the edit.
	spiral := []scriptStep{
		{content: gtOriginalContent, toolName: "web_search", toolArgs: `{"query":"same"}`},
	}
	var calls int
	var mu sync.Mutex

	srv := scriptedServer(t, spiral, &calls, &mu)
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-cassandra-groundtruth"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	conv := "conv-cassandra-groundtruth"
	a := New(Options{Config: cfg, Main: client, Tools: &tools.Manager{}, Pager: pager, ConvID: conv})

	err = a.Chat(context.Background(), "keep searching the same thing")
	if err == nil || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a spiral must terminate with ErrIncomplete, got: %v", err)
	}

	// Precondition: the controller actually fired (else the property is vacuous).
	if len(a.turn.casRecord) == 0 {
		t.Fatal("the controller must have injected at least one mod for this test to be meaningful")
	}
	// The in-WINDOW copy carries the folded doubt (overlay applied).
	foldedSeen := false
	for _, m := range a.working {
		if m.Role == llm.RoleAssistant && strings.HasPrefix(m.Content, gtOriginalContent) && len(m.Content) > len(gtOriginalContent) {
			foldedSeen = true
		}
	}
	if !foldedSeen {
		t.Fatal("the in-window transcript must show the folded (modded) assistant content")
	}

	// GROUND TRUTH: read the DURABLE transcript back from cortex. Every assistant
	// message must be EXACTLY the original content — the mod never reached
	// AppendMessage.
	msgs, err := pager.Transcript(conv, 0, 0)
	if err != nil {
		t.Fatalf("pager.Transcript: %v", err)
	}
	assistantCount := 0
	for _, m := range msgs {
		if m.Role != cortex.RoleAssistant {
			continue
		}
		assistantCount++
		if m.Content != gtOriginalContent {
			t.Errorf("durable assistant content was rewritten: got %q want %q", m.Content, gtOriginalContent)
		}
		// Belt-and-braces: no fragment of the injected doubt leaked into cortex.
		for _, marker := range []string{"different approach", "repeating myself", "step back"} {
			if strings.Contains(strings.ToLower(m.Content), marker) {
				t.Errorf("the Cassandra mod leaked into the durable transcript: %q", m.Content)
			}
		}
	}
	if assistantCount == 0 {
		t.Fatal("expected durable assistant messages in the transcript")
	}
}
