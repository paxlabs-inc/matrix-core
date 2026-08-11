// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// doNotRepeatMarker is the do-NOT-repeat framing resumePrime folds in front of
// the predecessor's death digest. A successor that reads it knows the losing
// move; its absence is a blind respawn.
const doNotRepeatMarker = "do NOT repeat the same move"

// primeKeyedServer scripts a model whose behavior depends on the REAL prompt it
// receives: if the request carries the predecessor's do-NOT-repeat death digest
// it converges (a final answer — it does not repeat the losing move); otherwise
// it repeats the SAME losing tool call every step (the no-progress spiral that
// produced the death). A genuine decision function over the actual request
// bytes — no fake successor, no canned outcome.
func primeKeyedServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bornKnowing := strings.Contains(string(body), doNotRepeatMarker)

		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if bornKnowing {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"I see the previous attempt got stuck re-running the same check — I'll take a different, concrete step instead. Done."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		tc := fmt.Sprintf(`{"index":0,"id":"call_%d","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"the same losing query\"}"}}`, idx)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func primeKeyedAgent(t *testing.T, url string, pager *memory.Pager, convID string) *agent.Agent {
	t.Helper()
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  url,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false // isolate the pure no-progress stall death
	manager := &tools.Manager{}
	runtime := openCanonicalTestRuntime(t, &cfg, manager, pager, url)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return agent.New(agent.Options{
		Config:  cfg,
		Main:    client,
		Tools:   manager,
		Pager:   pager,
		Runtime: runtime,
		ConvID:  convID,
	})
}

// TestE2E_CanonicalFailureProducesTypedEvidenceWithoutLegacyPromotion is the
// no-fakes proof that canonical failure evidence remains durable operational
// state and is not silently promoted into a semantic lesson:
//
//  1. a real canonical turn returns typed ErrIncomplete evidence;
//  2. the obsolete LastDeath promotion seam remains empty;
//  3. an explicit death-journal write is recallable; and
//  4. resumePrime carries operational guidance without the raw sentinel.
func TestE2E_CanonicalFailureProducesTypedEvidenceWithoutLegacyPromotion(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := primeKeyedServer(t, &calls, &mu)

	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-e2e-respawn"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	const objective = "compile and ship the schema migration"
	ctx := context.Background()

	// --- 1. Real predecessor death on the real Chat loop ---
	predecessor := primeKeyedAgent(t, srv.URL, pager, "conv-respawn-pred")
	predErr := predecessor.Chat(ctx, objective)
	if predErr == nil || !errors.Is(predErr, agent.ErrIncomplete) {
		t.Fatalf("the predecessor must die a real ErrIncomplete stall; got: %v", predErr)
	}
	incomplete, ok := predecessor.LastRuntimeIncomplete()
	if !ok || incomplete.Phase == "" || incomplete.AttemptCount < 1 {
		t.Fatalf("canonical failure must retain typed phase and attempt evidence; got %+v (ok=%v)", incomplete, ok)
	}
	if legacy, promoted := predecessor.LastDeath(); promoted {
		t.Fatalf("canonical failure must not auto-promote a legacy death lesson: %+v", legacy)
	}

	// --- 2. Persist a REAL death-journal entry; prove ordinary recall surfaces it ---
	state := fmt.Sprintf(
		"phase=%s attempts=%d recovery=%s",
		incomplete.Phase, incomplete.AttemptCount, incomplete.RecoveryAdvice,
	)
	entry := newDeathEntry(objective, 1, predErr, delegate.ClassNone, state)
	if _, werr := pager.RecordLoopDeath(ctx, entry.durableSummary(), "conv-respawn-pred"); werr != nil {
		t.Fatalf("RecordLoopDeath: %v", werr)
	}
	journal, jerr := pager.DeathJournal(ctx, 10)
	if jerr != nil {
		t.Fatalf("DeathJournal: %v", jerr)
	}
	if len(journal) == 0 || !strings.Contains(journal[0].Summary, objective) {
		t.Fatalf("the durable death-journal entry must be recallable; got %+v", journal)
	}

	// --- 3. The immediate read path keeps failure evidence in the supervisor
	// guidance lane without turning it into a confirmed semantic lesson. ---
	prime := resumePrime(objective, 2, predErr)
	if !strings.Contains(prime, doNotRepeatMarker) {
		t.Fatalf("the resume prime must carry the do-NOT-repeat framing; got:\n%s", prime)
	}
	if strings.Contains(prime, agent.ErrIncomplete.Error()) {
		t.Fatalf("the resume prime must strip the raw ErrIncomplete sentinel; got:\n%s", prime)
	}
}
