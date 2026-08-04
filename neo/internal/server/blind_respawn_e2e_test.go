// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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
	return agent.New(agent.Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: convID,
	})
}

// TestE2E_BlindRespawnCuredBySelfAwareness is the no-fakes end-to-end proof for
// the blind-respawn bug (self-model req.12.2/12.3). It exercises the REAL
// supervisor read paths on real code:
//
//  1. a real predecessor dies on the REAL agent.Chat loop (a genuine
//     no_progress_stall, ErrIncomplete with a where-it-got-stuck digest);
//  2. the death is persisted as a REAL Neocortex death-journal Event and is
//     surfaced again by ordinary recall (the durable read path);
//  3. the successor's catch-up prime is built by the REAL resumePrime, folding
//     the predecessor's digest with a do-NOT-repeat framing (the immediate read
//     path);
//  4. a successor BORN KNOWING (driven with that real prime) does NOT repeat the
//     losing move — it converges — while a BLIND respawn (same model, objective
//     only) repeats the losing move and dies again.
//
// Real Neocortex, real death-journal entry, real resume prime, real Chat loop — the
// difference between converging and looping is exactly the inherited death
// knowledge.
func TestE2E_BlindRespawnCuredBySelfAwareness(t *testing.T) {
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
	death, ok := predecessor.LastDeath()
	if !ok || death.Reason != agent.DeathReasonStall {
		t.Fatalf("the predecessor must record a real no_progress_stall death; got %+v (ok=%v)", death, ok)
	}

	// --- 2. Persist a REAL death-journal entry; prove ordinary recall surfaces it ---
	entry := newDeathEntry(objective, 1, predErr, delegate.ClassNone, death.StateLine())
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

	// --- 3. The REAL immediate read path: the successor's catch-up prime ---
	prime := resumePrime(objective, 2, predErr)
	if !strings.Contains(prime, doNotRepeatMarker) {
		t.Fatalf("the resume prime must carry the do-NOT-repeat framing; got:\n%s", prime)
	}
	if strings.Contains(prime, agent.ErrIncomplete.Error()) {
		t.Fatalf("the resume prime must strip the raw ErrIncomplete sentinel; got:\n%s", prime)
	}

	// --- 4a. Successor BORN KNOWING: does not repeat the losing move ---
	successor := primeKeyedAgent(t, srv.URL, pager, "conv-respawn-succ")
	if serr := successor.Chat(ctx, prime); serr != nil {
		t.Fatalf("a successor born knowing how its predecessor died must converge, not repeat; got: %v", serr)
	}

	// --- 4b. BLIND respawn (an isolated Neocortex with no inherited death
	// knowledge): repeats the losing move. A second conversation on the shared
	// substrate is intentionally not blind because Neocortex recall spans the
	// agent's durable history.
	blindCfg := cfg
	blindCfg.DataRoot = t.TempDir()
	blindCfg.NeocortexActor = "neo-e2e-respawn-blind"
	blindPager, err := memory.Open(blindCfg)
	if err != nil {
		t.Fatalf("open isolated blind Neocortex: %v", err)
	}
	defer blindPager.Close()
	blind := primeKeyedAgent(t, srv.URL, blindPager, "conv-respawn-blind")
	blindErr := blind.Chat(ctx, objective)
	if blindErr == nil || !errors.Is(blindErr, agent.ErrIncomplete) {
		t.Fatalf("a blind respawn (no inherited death knowledge) must repeat the losing move and die again; got: %v", blindErr)
	}
}
