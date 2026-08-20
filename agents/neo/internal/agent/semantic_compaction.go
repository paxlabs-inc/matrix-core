// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"strings"

	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/sessionjournal"
)

func (a *Agent) compactExactWorking() {
	if a == nil || len(a.working) <= 2 {
		return
	}
	events := a.checkpointEvents()
	if len(events) > 0 {
		a.semanticCheckpoint = sessionjournal.BuildSemanticCheckpoint(events)
	}
	a.working = protectedProtocolTail(a.working, a.protectedTailBudget())
}

func (a *Agent) checkpointEvents() []sessionjournal.Event {
	if a.journal != nil {
		if events, err := a.journal.Read(context.Background(), a.cmConvID(), 1, 0); err == nil && len(events) > 0 {
			return events
		}
	}
	events := make([]sessionjournal.Event, 0, len(a.working))
	for index, message := range a.working {
		apiContent, _ := json.Marshal(message)
		event := sessionjournal.Event{
			ConversationID: a.cmConvID(), Sequence: int64(index + 1),
			DisplayContent: message.Content, APIContent: apiContent,
		}
		switch message.Role {
		case llm.RoleUser:
			event.Kind = sessionjournal.KindUserMessage
			event.Message = &sessionjournal.Message{Role: sessionjournal.RoleUser}
		case llm.RoleAssistant:
			event.Kind = sessionjournal.KindAssistantMessage
			event.Message = &sessionjournal.Message{Role: sessionjournal.RoleAssistant}
		case llm.RoleTool:
			event.Kind = sessionjournal.KindToolResult
			event.ToolResult = &sessionjournal.ToolResult{CallID: message.ToolCallID, Name: message.Name, Result: []byte(message.Content)}
		default:
			continue
		}
		events = append(events, event)
	}
	return events
}

func (a *Agent) protectedTailBudget() int {
	budget := a.cfg.ContextWindowTokens / 4
	if budget < 2000 {
		budget = 2000
	}
	if budget > 24000 {
		budget = 24000
	}
	return budget
}

func protectedProtocolTail(messages []llm.Message, budget int) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	starts := []int{0}
	for index := 1; index < len(messages); index++ {
		if messages[index].Role == llm.RoleUser && !messages[index].IsGuidance() {
			starts = append(starts, index)
		}
	}
	start := starts[len(starts)-1]
	for group := len(starts) - 2; group >= 0; group-- {
		candidate := starts[group]
		if estimateMessagesTokens(messages[candidate:]) > budget {
			break
		}
		start = candidate
	}
	tail := append([]llm.Message(nil), messages[start:]...)
	return repairProtocolTail(tail)
}

func repairProtocolTail(messages []llm.Message) []llm.Message {
	calls := make(map[string]struct{})
	for _, message := range messages {
		if message.Role != llm.RoleAssistant {
			continue
		}
		for _, call := range message.ToolCalls {
			calls[call.ID] = struct{}{}
		}
	}
	out := messages[:0]
	for _, message := range messages {
		if message.Role == llm.RoleTool {
			if _, ok := calls[strings.TrimSpace(message.ToolCallID)]; !ok {
				continue
			}
		}
		out = append(out, message)
	}
	return out
}

func (a *Agent) SemanticCheckpoint() sessionjournal.SemanticCheckpoint {
	if a == nil {
		return sessionjournal.SemanticCheckpoint{}
	}
	return a.semanticCheckpoint
}
