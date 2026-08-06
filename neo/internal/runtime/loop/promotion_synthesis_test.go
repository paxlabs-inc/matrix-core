// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/neo/internal/runtime/records"
	"matrix/neo/internal/runtime/turnstate"
)

func TestBriefMePromotesExecutesAndSynthesizesInSameTurn(t *testing.T) {
	manager, root := realNativeManager(t)
	if err := os.WriteFile(filepath.Join(root, "briefing-notes.txt"), []byte("briefing evidence: system healthy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var generations atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded gatewayRequest
		_ = json.Unmarshal(body, &decoded)
		if handleCapabilityCanary(writer, decoded) {
			return
		}
		if generations.Add(1) == 1 {
			writeSSETool(writer, "brief-search", "search_files", map[string]interface{}{
				"path": ".", "query": "briefing evidence", "max_results": 10,
			})
			return
		}
		writeSSEText(writer, "Briefing delivered: the committed search found that the system is healthy.")
	}))
	defer gateway.Close()

	turnID := "brief-me-promoted-turn"
	request := "Brief me on the current system evidence."
	store := realTurnStore(t, turnID, request)
	journal := &DurableEffectJournal{Store: store}
	adapter, err := NewToolManagerAdapter(manager, journal)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(realMiMoGenerator(t, gateway.URL), adapter, store, Config{
		TurnID: turnID, ConversationID: "brief-conversation", Model: "mimo-v2",
		SystemPrompt: "Produce an evidence-backed briefing.", MaxToolCalls: 4,
		IdleTimeout: 20 * time.Second, MaxTurnTokens: 220_000,
	}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if generations.Load() != 2 || len(response.ToolEvents) != 1 || response.ToolEvents[0].Error != "" || !strings.Contains(response.Content, "system is healthy") {
		t.Fatalf("briefing flow did not select, execute, and synthesize: generations=%d response=%+v", generations.Load(), response)
	}
	turn, err := store.LoadTurnRecord(t.Context(), turnID)
	if err != nil || turn.CurrentState != records.StateDelivered || turn.SynthesisDebt.Owed {
		t.Fatalf("turn did not finish debt-free: %#v err=%v", turn, err)
	}
	convergence, err := store.LoadConvergenceRecord(t.Context(), turnID)
	if err != nil || convergence.Tuning["provider_calls"] != 8 || convergence.ToolCalls != 1 || convergence.ProviderUsage[0].RequestCount != 2 {
		t.Fatalf("promotion/counters were not durable: %#v err=%v", convergence, err)
	}
	state, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil || state.Status != turnstate.StatusCompleted {
		t.Fatalf("logical turn respawned or failed terminal delivery: %#v err=%v", state, err)
	}
}
