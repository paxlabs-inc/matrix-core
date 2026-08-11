// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"context"
	"encoding/json"
)

type ProtocolMessage struct {
	Sequence         int64      `json:"sequence"`
	TurnID           string     `json:"turn_id,omitempty"`
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	Reasoning        string     `json:"reasoning,omitempty"`
	ProviderMetadata []byte     `json:"provider_metadata,omitempty"`
	APIContent       []byte     `json:"api_content,omitempty"`
}

type ProviderFrame struct {
	Sequence int64  `json:"sequence"`
	TurnID   string `json:"turn_id,omitempty"`
	Kind     Kind   `json:"kind"`
	Bytes    []byte `json:"bytes"`
}

type Replay struct {
	ConversationID string
	Messages       []ProtocolMessage
	ProviderFrames []ProviderFrame
	Approvals      []Event
	Artifacts      []Event
	Effects        []Event
	Supervisor     []Event
	Recovery       []Event
}

func (store *Store) Replay(ctx context.Context, conversationID string) (Replay, error) {
	events, err := store.Read(ctx, conversationID, 1, 0)
	if err != nil {
		return Replay{}, err
	}
	replay := Replay{ConversationID: conversationID}
	messageByCall := make(map[string]int)
	for _, event := range events {
		if len(event.APIContent) > 0 {
			replay.ProviderFrames = append(replay.ProviderFrames, ProviderFrame{
				Sequence: event.Sequence, TurnID: event.TurnID,
				Kind: event.Kind, Bytes: append([]byte(nil), event.APIContent...),
			})
		}
		switch event.Kind {
		case KindUserMessage, KindAssistantMessage:
			message := ProtocolMessage{
				Sequence: event.Sequence, TurnID: event.TurnID,
				Role: event.Message.Role, Content: event.DisplayContent,
				ToolCalls:  cloneToolCalls(event.Message.ToolCalls),
				ToolCallID: event.Message.ToolCallID, Name: event.Message.Name,
				ProviderMetadata: append([]byte(nil), event.Message.ProviderMetadata...),
				APIContent:       append([]byte(nil), event.APIContent...),
			}
			for _, call := range message.ToolCalls {
				messageByCall[call.CallID] = len(replay.Messages)
			}
			replay.Messages = append(replay.Messages, message)
		case KindToolCall:
			if _, exists := messageByCall[event.ToolCall.CallID]; exists {
				continue
			}
			messageByCall[event.ToolCall.CallID] = len(replay.Messages)
			replay.Messages = append(replay.Messages, ProtocolMessage{
				Sequence: event.Sequence, TurnID: event.TurnID,
				Role: RoleAssistant, ToolCalls: []ToolCall{cloneToolCall(*event.ToolCall)},
				APIContent: append([]byte(nil), event.APIContent...),
			})
		case KindToolResult:
			replay.Messages = append(replay.Messages, ProtocolMessage{
				Sequence: event.Sequence, TurnID: event.TurnID,
				Role: "tool", Content: event.DisplayContent,
				ToolCallID: event.ToolResult.CallID, Name: event.ToolResult.Name,
				ProviderMetadata: append([]byte(nil), event.ToolResult.ProviderMetadata...),
				APIContent:       append([]byte(nil), event.APIContent...),
			})
		case KindReasoning:
			for index := len(replay.Messages) - 1; index >= 0; index-- {
				if replay.Messages[index].Role == RoleAssistant {
					replay.Messages[index].Reasoning = event.Reasoning.Text
					if len(event.Reasoning.ProviderMetadata) > 0 {
						replay.Messages[index].ProviderMetadata = append([]byte(nil), event.Reasoning.ProviderMetadata...)
					}
					break
				}
			}
		case KindProviderReplay:
			replay.ProviderFrames = append(replay.ProviderFrames, ProviderFrame{
				Sequence: event.Sequence, TurnID: event.TurnID,
				Kind: event.Kind, Bytes: append([]byte(nil), event.ProviderReplay.RequestBytes...),
			})
		case KindApproval:
			replay.Approvals = append(replay.Approvals, event)
		case KindArtifact:
			replay.Artifacts = append(replay.Artifacts, event)
		case KindUncertainEffect:
			replay.Effects = append(replay.Effects, event)
		case KindSupervisor:
			replay.Supervisor = append(replay.Supervisor, event)
		case KindRecovery:
			replay.Recovery = append(replay.Recovery, event)
		}
	}
	return replay, nil
}

func (replay Replay) ExactProviderBytes(turnID string) [][]byte {
	frames := make([][]byte, 0, len(replay.ProviderFrames))
	for _, frame := range replay.ProviderFrames {
		if turnID != "" && frame.TurnID != turnID {
			continue
		}
		frames = append(frames, append([]byte(nil), frame.Bytes...))
	}
	return frames
}

func (message ProtocolMessage) DecodeAPIContent(target any) error {
	return json.Unmarshal(message.APIContent, target)
}

func cloneToolCalls(calls []ToolCall) []ToolCall {
	cloned := make([]ToolCall, len(calls))
	for index := range calls {
		cloned[index] = cloneToolCall(calls[index])
	}
	return cloned
}

func cloneToolCall(call ToolCall) ToolCall {
	call.Arguments = append([]byte(nil), call.Arguments...)
	return call
}
