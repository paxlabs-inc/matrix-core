// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/conversation"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/sessionjournal"
	"centra/agents/neo/internal/tools"
)

func TestRebuildAgentUsesJournalForInitialMintAndSupervisorRespawn(t *testing.T) {
	ctx := context.Background()
	const conversationID = "conv-journal-rebuild"
	journal, err := sessionjournal.Open(ctx, filepath.Join(t.TempDir(), "journal.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	call := llm.ToolCall{ID: "rebuild-call", Type: "function", Function: llm.FunctionCall{Name: "fs__write_file", Arguments: `{"path":"a.txt","content":"restart evidence"}`}}
	user := llm.UserMessage("write the restart evidence")
	assistant := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}
	result := llm.ToolResult(call.ID, call.Function.Name, "Successfully wrote a.txt")
	appendEvent := func(event sessionjournal.Event) {
		t.Helper()
		if _, err := journal.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	userAPI, _ := json.Marshal(user)
	assistantAPI, _ := json.Marshal(assistant)
	callAPI, _ := json.Marshal(call)
	resultAPI, _ := json.Marshal(result)
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindUserMessage, DisplayContent: user.Content, APIContent: userAPI, Message: &sessionjournal.Message{Role: sessionjournal.RoleUser}})
	journalCall := sessionjournal.ToolCall{CallID: call.ID, Type: call.Type, Name: call.Function.Name, Arguments: []byte(call.Function.Arguments), StateChanging: true}
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindAssistantMessage, APIContent: assistantAPI, Message: &sessionjournal.Message{Role: sessionjournal.RoleAssistant, ToolCalls: []sessionjournal.ToolCall{journalCall}}})
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindToolCall, APIContent: callAPI, ToolCall: &journalCall})
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindToolResult, DisplayContent: result.Content, APIContent: resultAPI, ToolResult: &sessionjournal.ToolResult{CallID: call.ID, Name: call.Function.Name, Result: []byte(result.Content)}})
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindApproval, DisplayContent: "approved", Approval: &sessionjournal.Approval{ApprovalID: "approval-1", Action: "write", Decision: "approved", Authority: "user"}})
	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindUncertainEffect, DisplayContent: "unknown", UncertainEffect: &sessionjournal.UncertainEffect{CallID: call.ID, ToolName: call.Function.Name, Status: "unknown"}})

	cfg := config.Default()
	cfg.SessionExactProjection = true
	e := &Engine{cfg: cfg, conv: conversation.Open(t.TempDir()), journal: journal, tools: &tools.Manager{}, runs: map[string]*run{}, broker: newBroker()}
	s := e.newSession(conversationID)
	assertJournalProtocolHistory(t, s.agent.ProtocolHistory(), call.ID)
	state := s.agent.ReplayState()
	if len(state.Approvals) != 1 || len(state.Effects) != 1 {
		t.Fatalf("replay side state: approvals=%d effects=%d", len(state.Approvals), len(state.Effects))
	}

	appendEvent(sessionjournal.Event{ConversationID: conversationID, Kind: sessionjournal.KindSupervisor, DisplayContent: "respawn", Supervisor: &sessionjournal.Supervisor{IntentID: "run-1", Attempt: 2, Action: "respawn", Reason: "test"}})
	s.cur = &run{id: "run-1", convID: conversationID, sess: s}
	s.rebuildAgent()
	assertJournalProtocolHistory(t, s.agent.ProtocolHistory(), call.ID)
	if got := len(s.agent.ReplayState().Supervisor); got != 1 {
		t.Fatalf("respawn did not cross journal replay boundary: supervisor events=%d", got)
	}
}

func TestRebuildAgentFallsBackToConversationTextWithoutJournal(t *testing.T) {
	const conversationID = "conv-text-fallback"
	store := conversation.Open(t.TempDir())
	store.AppendUser(conversationID, "run-1", "fallback user")
	store.AppendAssistant(conversationID, "run-1", "fallback assistant")
	cfg := config.Default()
	cfg.SessionExactProjection = true
	e := &Engine{cfg: cfg, conv: store, tools: &tools.Manager{}, runs: map[string]*run{}, broker: newBroker()}
	s := e.newSession(conversationID)
	history := s.agent.ProtocolHistory()
	if len(history) != 2 || history[0].Role != llm.RoleUser || history[0].Content != "fallback user" || history[1].Role != llm.RoleAssistant {
		t.Fatalf("text fallback history = %+v", history)
	}
	if len(s.agent.ReplayState().Messages) != 0 {
		t.Fatal("text fallback falsely reported journal replay")
	}
}

func assertJournalProtocolHistory(t *testing.T, history []llm.Message, callID string) {
	t.Helper()
	if len(history) != 3 {
		t.Fatalf("protocol history length = %d, want 3: %+v", len(history), history)
	}
	if history[0].Role != llm.RoleUser || history[1].Role != llm.RoleAssistant || len(history[1].ToolCalls) != 1 || history[1].ToolCalls[0].ID != callID || history[2].Role != llm.RoleTool || history[2].ToolCallID != callID {
		t.Fatalf("protocol history pairing drifted: %+v", history)
	}
}
