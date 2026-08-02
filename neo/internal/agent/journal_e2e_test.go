// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/tools"
	"matrix/vault"
)

func TestSessionJournalRealLoopOrderingReplayAndFailClosedDispatch(t *testing.T) {
	ctx := context.Background()
	const conversationID = "conv-journal-real-loop"
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
	target := filepath.Join(workdir, "journal-effect.txt")

	arguments, err := json.Marshal(map[string]string{
		"path":    target,
		"content": "journal transaction boundary",
		"expect":  "the requested file exists with the supplied content",
	})
	if err != nil {
		t.Fatal(err)
	}
	toolCall := fmt.Sprintf(`{"index":0,"id":"journal-call-1","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(arguments))

	var (
		modelMu           sync.Mutex
		mainCalls         int
		callBeforeExecute bool
		callInspectErr    error
		resultBeforeModel bool
		modelInspectErr   error
		resultBeforeUI    bool
		uiInspectErr      error
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(body, "stated expectation") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"match"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		modelMu.Lock()
		mainCalls++
		call := mainCalls
		modelMu.Unlock()
		if call == 1 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", toolCall)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		events, readErr := journal.Read(ctx, conversationID, 1, 0)
		modelMu.Lock()
		if readErr != nil {
			modelInspectErr = readErr
		} else {
			resultBeforeModel = hasJournalKind(events, sessionjournal.KindToolResult)
		}
		modelMu.Unlock()
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"the data is fetched"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	client := revisionTestClient(t, model.URL)
	agent := New(Options{
		Config: cfg, Main: client, Cheap: client, Tools: manager,
		ConvID: conversationID, Journal: journal,
		Observer: func(event ToolEvent) {
			if event.Phase == ToolStart {
				events, readErr := journal.Read(ctx, conversationID, 1, 0)
				_, statErr := os.Stat(target)
				modelMu.Lock()
				if readErr != nil {
					callInspectErr = readErr
				} else {
					callBeforeExecute = hasJournalKind(events, sessionjournal.KindToolCall) && !hasJournalKind(events, sessionjournal.KindToolResult) && errors.Is(statErr, os.ErrNotExist)
				}
				modelMu.Unlock()
				return
			}
			if event.Phase != ToolEnd {
				return
			}
			events, readErr := journal.Read(ctx, conversationID, 1, 0)
			modelMu.Lock()
			if readErr != nil {
				uiInspectErr = readErr
			} else {
				resultBeforeUI = hasJournalKind(events, sessionjournal.KindToolResult)
			}
			modelMu.Unlock()
		},
	})
	if err := agent.Chat(ctx, "write the requested file and report"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	modelMu.Lock()
	beforeEffect, callErr := callBeforeExecute, callInspectErr
	modelMu.Unlock()
	if callErr != nil || !beforeEffect {
		t.Fatalf("call-before-effect invariant: durable_before=%v err=%v", beforeEffect, callErr)
	}
	written, err := os.ReadFile(target)
	if err != nil || string(written) != "journal transaction boundary" {
		t.Fatalf("real filesystem effect = %q, %v", written, err)
	}
	modelMu.Lock()
	beforeModel, beforeUI, modelErr, uiErr := resultBeforeModel, resultBeforeUI, modelInspectErr, uiInspectErr
	modelMu.Unlock()
	if modelErr != nil || uiErr != nil || !beforeModel || !beforeUI {
		t.Fatalf("result-before-exposure invariant: model=%v ui=%v model_err=%v ui_err=%v", beforeModel, beforeUI, modelErr, uiErr)
	}

	events, err := journal.Read(ctx, conversationID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []sessionjournal.Kind{
		sessionjournal.KindUserMessage,
		sessionjournal.KindAssistantMessage,
		sessionjournal.KindToolCall,
		sessionjournal.KindToolResult,
		sessionjournal.KindAssistantMessage,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("journal event count = %d, want %d: %+v", len(events), len(wantKinds), events)
	}
	for index, want := range wantKinds {
		if events[index].Kind != want || events[index].Sequence != int64(index+1) {
			t.Fatalf("event[%d] = kind %q sequence %d, want %q/%d", index, events[index].Kind, events[index].Sequence, want, index+1)
		}
	}
	if events[2].ToolCall == nil || !events[2].ToolCall.StateChanging {
		t.Fatalf("write call was not classified as state-changing: %+v", events[2].ToolCall)
	}

	replay, err := journal.Replay(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Messages) != 4 {
		t.Fatalf("protocol replay messages = %d, want 4", len(replay.Messages))
	}
	if len(replay.Messages[1].ToolCalls) != 1 || replay.Messages[1].ToolCalls[0].CallID != "journal-call-1" || replay.Messages[2].ToolCallID != "journal-call-1" {
		t.Fatalf("protocol tool pairing drifted: %+v", replay.Messages)
	}
	var wantProviderBytes [][]byte
	for _, event := range events {
		if len(event.APIContent) > 0 {
			wantProviderBytes = append(wantProviderBytes, event.APIContent)
		}
	}
	gotProviderBytes := replay.ExactProviderBytes("")
	if len(gotProviderBytes) != len(wantProviderBytes) {
		t.Fatalf("provider frame count = %d, want %d", len(gotProviderBytes), len(wantProviderBytes))
	}
	for index := range wantProviderBytes {
		if !bytes.Equal(gotProviderBytes[index], wantProviderBytes[index]) {
			t.Fatalf("provider bytes[%d] are not byte-exact", index)
		}
	}

	closedJournal := openRealAgentJournal(t)
	if err := closedJournal.Close(); err != nil {
		t.Fatal(err)
	}
	blocked := New(Options{Config: cfg, Tools: manager, ConvID: "conv-journal-fail-closed", Journal: closedJournal})
	blockedTarget := filepath.Join(workdir, "blocked-effect.txt")
	blockedArguments, err := json.Marshal(map[string]string{
		"path": blockedTarget, "content": "must not exist", "expect": "the file exists",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = blocked.runToolCalls(ctx, []llm.ToolCall{{
		ID: "blocked-call", Type: "function",
		Function: llm.FunctionCall{Name: "fs__write_file", Arguments: string(blockedArguments)},
	}})
	if !errors.Is(err, sessionjournal.ErrClosed) {
		t.Fatalf("closed journal dispatch error = %v, want ErrClosed", err)
	}
	if _, statErr := os.Stat(blockedTarget); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tool effect occurred after call persistence failed: %v", statErr)
	}
}

func hasJournalKind(events []sessionjournal.Event, kind sessionjournal.Kind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func openRealAgentJournal(t *testing.T) *sessionjournal.Store {
	t.Helper()
	dir := t.TempDir()
	vaultSession, err := vault.Boot(context.Background(), vault.Config{
		Required: true,
		DataDir:  filepath.Join(dir, "vault"),
		UserDID:  "did:matrix:agent-journal-test",
		KEKHex:   hex.EncodeToString(bytes.Repeat([]byte{0x71}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionjournal.Open(context.Background(), filepath.Join(dir, "journal.db"), vaultSession)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
