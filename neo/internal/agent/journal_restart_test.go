// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

func TestJournalReplayHundredTurnRestartAndLongRangeToolReference(t *testing.T) {
	ctx := context.Background()
	const conversationID = "conv-hundred-turn-restart"
	journal := openRealAgentJournal(t)
	workdir := t.TempDir()
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, workdir), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	target := filepath.Join(workdir, "restart-evidence.txt")
	arguments, err := json.Marshal(map[string]string{"path": target, "content": "restart evidence"})
	if err != nil {
		t.Fatal(err)
	}
	toolCall := fmt.Sprintf(`{"index":0,"id":"restart-call","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(arguments))

	var (
		mu              sync.Mutex
		modelCalls      int
		referenceBodies [][]byte
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &request)
		currentReference := false
		for index := len(request.Messages) - 1; index >= 0; index-- {
			if request.Messages[index].Role == llm.RoleUser {
				currentReference = strings.HasPrefix(request.Messages[index].Content, "REFERENCE_THE_RESTART_CALL")
				break
			}
		}
		mu.Lock()
		modelCalls++
		call := modelCalls
		if currentReference {
			referenceBodies = append(referenceBodies, append([]byte(nil), raw...))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", toolCall)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"acknowledged"},"finish_reason":"stop"}]}`+"\n")
		}
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.SessionCurrentIntent = true
	cfg.SessionExactProjection = true
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	newAgent := func() *Agent {
		return New(Options{Config: cfg, Main: windowLawClient(t, model.URL), Tools: manager, ConvID: conversationID, Journal: journal})
	}
	a := newAgent()
	if err := a.Chat(ctx, "write durable restart evidence"); err != nil {
		t.Fatal(err)
	}
	for turn := 1; turn < 40; turn++ {
		prompt := fmt.Sprintf("ordinary turn %d", turn)
		if turn == 20 {
			prompt = "REFERENCE_THE_RESTART_CALL from twenty turns ago"
		}
		if err := a.Chat(ctx, prompt); err != nil {
			t.Fatalf("pre-restart turn %d: %v", turn, err)
		}
	}
	replay, err := journal.Replay(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := newAgent()
	seeded, err := fresh.SeedReplay(replay)
	if err != nil || !seeded {
		t.Fatalf("SeedReplay = %v, %v", seeded, err)
	}
	if fresh.turnSeq != 40 {
		t.Fatalf("restored turn sequence = %d, want 40", fresh.turnSeq)
	}
	assertReplayToolPair(t, fresh.ProtocolHistory(), "restart-call")
	for turn := 40; turn < 100; turn++ {
		prompt := fmt.Sprintf("ordinary turn %d", turn)
		if turn == 60 {
			prompt = "REFERENCE_THE_RESTART_CALL after the process restart"
		}
		if err := fresh.Chat(ctx, prompt); err != nil {
			t.Fatalf("post-restart turn %d: %v", turn, err)
		}
	}
	if fresh.turnSeq != 100 {
		t.Fatalf("final turn sequence = %d, want 100", fresh.turnSeq)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "restart evidence" {
		t.Fatalf("restart evidence file = %q, %v", content, err)
	}
	mu.Lock()
	references := append([][]byte(nil), referenceBodies...)
	mu.Unlock()
	if len(references) != 2 {
		t.Fatalf("long-range reference requests = %d, want 2", len(references))
	}
	for index, body := range references {
		if !bytesContainAll(body, `"id":"restart-call"`, `"tool_call_id":"restart-call"`, "Successfully wrote") {
			t.Fatalf("reference request %d lost protocol-shaped tool history", index)
		}
	}
	finalReplay, err := journal.Replay(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	toolCalls := 0
	for _, message := range finalReplay.Messages {
		for _, call := range message.ToolCalls {
			if call.CallID == "restart-call" {
				toolCalls++
			}
		}
	}
	if toolCalls != 1 {
		t.Fatalf("restart tool call replayed as a new effect: count=%d", toolCalls)
	}
}

func assertReplayToolPair(t *testing.T, history []llm.Message, callID string) {
	t.Helper()
	callSeen, resultSeen := false, false
	for _, message := range history {
		for _, call := range message.ToolCalls {
			if call.ID == callID {
				callSeen = true
			}
		}
		if message.Role == llm.RoleTool && message.ToolCallID == callID {
			resultSeen = true
		}
	}
	if !callSeen || !resultSeen {
		t.Fatalf("replay tool pairing: call=%v result=%v", callSeen, resultSeen)
	}
}

func bytesContainAll(body []byte, values ...string) bool {
	text := string(body)
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}
