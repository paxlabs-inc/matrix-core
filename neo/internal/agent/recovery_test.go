// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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
	"sync/atomic"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/tools"
)

func TestLocalRecoveryTurnsInvalidCallsIntoContinuedObservations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, root), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}

	tests := []struct {
		name     string
		toolCall string
		kind     string
	}{
		{
			name:     "invalid tool name",
			toolCall: `{"index":0,"id":"bad-name","type":"function","function":{"name":"fs__definitely_missing","arguments":"{}"}}`,
			kind:     "invalid_tool_name",
		},
		{
			name:     "malformed arguments",
			toolCall: `{"index":0,"id":"bad-json","type":"function","function":{"name":"fs__write_file","arguments":"{\"path\":"}}`,
			kind:     "invalid_tool_arguments",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var secondBody string
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if calls.Add(1) == 1 {
					writeSSEToolCall(w, tc.toolCall)
					return
				}
				secondBody = string(raw)
				writeSSEAnswer(w, "I corrected the invalid call locally.")
			}))
			t.Cleanup(model.Close)

			cfg := config.Default()
			cfg.InteractionPosture = true
			cfg.CassandraEnabled = false
			client := revisionTestClient(t, model.URL)
			a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: manager})
			if err := a.Chat(ctx, "write a file after validating the operation"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || a.LocalRecoveryCount() == 0 {
				t.Fatalf("calls/recoveries = %d/%d, want 2/>0", calls.Load(), a.LocalRecoveryCount())
			}
			if !strings.Contains(secondBody, `\"kind\":\"`+tc.kind+`\"`) {
				t.Fatalf("continued request lacks structured %s observation: %s", tc.kind, secondBody)
			}
		})
	}
}

func TestProtocolTailRepairsMissingAndOrphanedResults(t *testing.T) {
	cfg := config.Default()
	cfg.InteractionPosture = true
	a := New(Options{Config: cfg})
	a.turn = newTurn()
	a.turn.posture = PostureExploration
	a.working = []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "missing", Type: "function", Function: llm.FunctionCall{Name: "fs__read_file", Arguments: `{}`}}}},
		llm.UserMessage("continue"),
		llm.ToolResult("orphan", "fs__read_file", "historical orphan evidence"),
	}
	a.repairWorkingProtocol()
	if a.LocalRecoveryCount() != 2 {
		t.Fatalf("local recovery count = %d, want 2", a.LocalRecoveryCount())
	}
	if len(a.working) != 4 || a.working[1].Role != llm.RoleTool || a.working[1].ToolCallID != "missing" || !strings.Contains(a.working[1].Content, "missing_tool_result") {
		t.Fatalf("missing result was not repaired: %+v", a.working)
	}
	if !a.working[3].Guidance || !strings.Contains(a.working[3].Content, "orphaned_tool_result") {
		t.Fatalf("orphan was not preserved as recovery guidance: %+v", a.working[3])
	}
}

func TestProviderFailureAfterRealEffectRecoversWithoutDuplicateDispatch(t *testing.T) {
	ctx := context.Background()
	const conversationID = "provider-after-effect"
	journal := openRealAgentJournal(t)
	root := t.TempDir()
	target := filepath.Join(root, "single-effect.txt")
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, root), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	args, _ := json.Marshal(map[string]string{"path": target, "content": "exactly once", "expect": "the file contains exactly once"})
	toolCall := fmt.Sprintf(`{"index":0,"id":"effect-1","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(args))

	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch modelCalls.Add(1) {
		case 1:
			writeSSEToolCall(w, toolCall)
		case 2:
			http.Error(w, `{"error":{"message":"temporary upstream failure"}}`, http.StatusServiceUnavailable)
		default:
			writeSSEAnswer(w, "The file is present and reconciled.")
		}
	}))
	t.Cleanup(model.Close)

	var dispatches atomic.Int32
	var mu sync.Mutex
	var phases []ToolPhase
	cfg := config.Default()
	cfg.InteractionPosture = true
	cfg.SessionExactProjection = true
	cfg.CassandraEnabled = false
	client := revisionTestClient(t, model.URL)
	a := New(Options{
		Config: cfg, Main: client, Cheap: client, Tools: manager, ConvID: conversationID, Journal: journal,
		Observer: func(event ToolEvent) {
			if event.Phase == ToolStart || event.Phase == ToolEnd {
				mu.Lock()
				phases = append(phases, event.Phase)
				mu.Unlock()
			}
			if event.Phase == ToolStart {
				dispatches.Add(1)
			}
		},
	})
	if err := a.Chat(ctx, "write the single-effect file and verify it"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "exactly once" {
		t.Fatalf("effect = %q, %v", content, err)
	}
	if dispatches.Load() != 1 || modelCalls.Load() != 3 {
		t.Fatalf("dispatch/model calls = %d/%d, want 1/3", dispatches.Load(), modelCalls.Load())
	}
	events, err := journal.Read(ctx, conversationID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasJournalKind(events, sessionjournal.KindToolCall) || !hasJournalKind(events, sessionjournal.KindToolResult) || !hasJournalKind(events, sessionjournal.KindProviderReplay) {
		t.Fatalf("journal lacks replayable effect boundary: %+v", events)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(phases) != 2 || phases[0] != ToolStart || phases[1] != ToolEnd {
		t.Fatalf("effect observer phases = %+v", phases)
	}
}
