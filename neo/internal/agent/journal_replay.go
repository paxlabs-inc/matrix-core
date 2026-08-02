// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"matrix/neo/internal/llm"
	"matrix/neo/internal/sessionjournal"
)

func (a *Agent) SeedReplay(replay sessionjournal.Replay) (bool, error) {
	if a == nil || len(a.working) > 0 || len(replay.Messages) == 0 {
		return false, nil
	}
	messages := make([]llm.Message, 0, len(replay.Messages))
	for index, protocol := range replay.Messages {
		message, err := protocolMessage(protocol)
		if err != nil {
			return false, fmt.Errorf("neo: replay message %d: %w", index, err)
		}
		messages = append(messages, message)
	}
	a.working = messages
	a.restoredReplay = replay
	for _, protocol := range replay.Messages {
		index := strings.LastIndex(protocol.TurnID, ":")
		if index < 0 {
			continue
		}
		sequence, err := strconv.Atoi(protocol.TurnID[index+1:])
		if err == nil && sequence > a.turnSeq {
			a.turnSeq = sequence
		}
	}
	return true, nil
}

func protocolMessage(protocol sessionjournal.ProtocolMessage) (llm.Message, error) {
	var message llm.Message
	if len(protocol.APIContent) > 0 && json.Unmarshal(protocol.APIContent, &message) == nil && message.Role != "" {
		message.Reasoning = protocol.Reasoning
		return message, nil
	}
	message = llm.Message{
		Role: protocol.Role, Content: protocol.Content,
		ToolCallID: protocol.ToolCallID, Name: protocol.Name,
		Reasoning: protocol.Reasoning,
	}
	for _, call := range protocol.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, llm.ToolCall{
			ID: call.CallID, Type: call.Type,
			Function: llm.FunctionCall{Name: call.Name, Arguments: string(call.Arguments)},
		})
	}
	if message.Role == "" {
		return llm.Message{}, fmt.Errorf("missing protocol role")
	}
	return message, nil
}

func (a *Agent) ProtocolHistory() []llm.Message {
	if a == nil {
		return nil
	}
	history := make([]llm.Message, len(a.working))
	copy(history, a.working)
	for index := range history {
		history[index].ToolCalls = append([]llm.ToolCall(nil), history[index].ToolCalls...)
	}
	return history
}

func (a *Agent) ReplayState() sessionjournal.Replay {
	if a == nil {
		return sessionjournal.Replay{}
	}
	return a.restoredReplay
}
