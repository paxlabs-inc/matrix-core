// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	mcllm "matrix/mcl/llm"
)

// fakeConsolidator captures Consolidate and ConsolidateSync calls for testing.
// It implements the Consolidator interface.
type fakeConsolidator struct {
	mu         sync.Mutex
	asyncCalls []string
	syncCalls  []string
}

func (f *fakeConsolidator) Consolidate(transcript string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asyncCalls = append(f.asyncCalls, transcript)
}

func (f *fakeConsolidator) ConsolidateSync(ctx context.Context, transcript string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls = append(f.syncCalls, transcript)
}

func (f *fakeConsolidator) syncCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncCalls)
}

func (f *fakeConsolidator) asyncCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asyncCalls)
}

func (f *fakeConsolidator) lastSyncCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.syncCalls) == 0 {
		return ""
	}
	return f.syncCalls[len(f.syncCalls)-1]
}

// newCompactionTestAgent builds an Agent with an httptest LLM that returns a
// valid (but minimal) summary response, plus a fake consolidator. The agent
// has enough configuration to call compact() without panicking.
func newCompactionTestAgent(t *testing.T) (*Agent, *fakeConsolidator) {
	t.Helper()
	// httptest server that returns a minimal SSE-formatted chat response
	// so compact()'s LLM call succeeds. The response just needs to be non-empty.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Minimal SSE: one data chunk with content, then [DONE]
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"GOAL: test\\nDECISIONS: none\\nARTIFACTS: none\\nOPEN: none\\nLAST_RESULTS: none\\nNEXT: none\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	client, err := llm.New(mcllm.Config{
		Model:      "test-model",
		Endpoint:   srv.URL,
		GatewayURL: srv.URL,
		Provider:   mcllm.ProviderFireworks,
		ProviderSet: true,
		APIKey:     "test-key",
	})
	if err != nil {
		// If llm.New fails (e.g. unknown provider), fall back to a client
		// that will error on Chat — compact() handles that path gracefully.
		t.Logf("llm.New failed (%v); compact() will use the error path", err)
		client = nil
	}

	fc := &fakeConsolidator{}
	a := New(Options{
		Config:       config.Default(),
		Main:          client,
		Consolidator:  fc,
	})
	return a, fc
}

// TestCompact_FlushesEvictedTurnsSynchronously verifies that compact() calls
// ConsolidateSync with the about-to-be-evicted turns' transcript BEFORE
// replacing a.working. The sync call must happen before the eviction so
// durable facts reach cortex first.
func TestCompact_FlushesEvictedTurnsSynchronously(t *testing.T) {
	a, fc := newCompactionTestAgent(t)

	// Build enough working history to trigger compaction (need > keepRecentUserTurns*2 turns).
	// Include high-entropy tokens that must survive to cortex.
	verbatim := "0x742d35Cc6634C0532925a3b844Bc9e7a5C42d8fc"
	for i := 0; i < 12; i++ {
		a.working = append(a.working, llm.UserMessage("turn "+string(rune('a'+i))+" "+verbatim))
		a.working = append(a.working, llm.AssistantMessage("response "+string(rune('a'+i))))
	}

	workingBefore := len(a.working)
	a.compact(context.Background(), "hard")

	// compact() must have called ConsolidateSync at least once.
	if fc.syncCallCount() == 0 {
		t.Fatal("compact() did not call ConsolidateSync — evicted turns were not synchronously consolidated")
	}

	// The sync call must have received a transcript containing the evicted turns.
	syncTranscript := fc.lastSyncCall()
	if !strings.Contains(syncTranscript, verbatim) {
		t.Errorf("ConsolidateSync transcript does not contain verbatim token %q; got %d chars", verbatim, len(syncTranscript))
	}

	// Working must have shrunk (eviction happened).
	if len(a.working) >= workingBefore {
		t.Errorf("working not shrunk: before=%d after=%d", workingBefore, len(a.working))
	}
}

// TestCompact_SyncFlushIndependentOfAsyncQueue verifies that the sync
// consolidation path is independent of the async queue. compact() should call
// ConsolidateSync directly (not through Consolidate/async). We verify this by
// checking that async was NOT called from compact() — only sync was.
func TestCompact_SyncFlushIndependentOfAsyncQueue(t *testing.T) {
	a, fc := newCompactionTestAgent(t)

	// Build enough working history to trigger compaction.
	for i := 0; i < 12; i++ {
		a.working = append(a.working, llm.UserMessage("msg "+string(rune('a'+i))))
		a.working = append(a.working, llm.AssistantMessage("resp "+string(rune('a'+i))))
	}

	asyncBefore := fc.asyncCallCount()
	a.compact(context.Background(), "hard")

	// compact() should have called sync, NOT async.
	if fc.syncCallCount() == 0 {
		t.Error("compact() should have called ConsolidateSync")
	}
	if fc.asyncCallCount() != asyncBefore {
		t.Errorf("compact() should not call async Consolidate; before=%d after=%d", asyncBefore, fc.asyncCallCount())
	}
}

// TestCompact_VerbatimTokensSurviveToCortex verifies that high-entropy tokens
// (addresses, tx hashes, file paths) in the evicted turns appear VERBATIM in
// the transcript passed to ConsolidateSync. This is the i3 trust contract:
// a paraphrased 0x... is a corrupted memory.
func TestCompact_VerbatimTokensSurviveToCortex(t *testing.T) {
	a, fc := newCompactionTestAgent(t)

	tokens := []string{
		"0x742d35Cc6634C0532925a3b844Bc9e7a5C42d8fc",
		"0xabc123def4567890123456789012345678901234567890123456789012345678",
		"/root/project/src/main.go",
		"tx_4a5e1e3ba096a3b42cf2e7e8b3a5d4c1f9e8d7c6b5a4e3f2d1c0b9a8e7f6d5c4",
	}

	for i, tok := range tokens {
		a.working = append(a.working, llm.UserMessage("turn "+string(rune('a'+i))+" ref: "+tok))
		a.working = append(a.working, llm.AssistantMessage("response "+string(rune('a'+i))))
	}
	// Add enough extra turns to exceed the keepRecent threshold.
	for i := 0; i < 6; i++ {
		a.working = append(a.working, llm.UserMessage("extra "+string(rune('a'+i))))
		a.working = append(a.working, llm.AssistantMessage("extra resp "+string(rune('a'+i))))
	}

	a.compact(context.Background(), "hard")

	if fc.syncCallCount() == 0 {
		t.Fatal("ConsolidateSync was not called")
	}

	transcript := fc.lastSyncCall()
	for _, tok := range tokens {
		if !strings.Contains(transcript, tok) {
			t.Errorf("verbatim token NOT in sync transcript: %q", tok)
		}
	}
}
