// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	exectool "centra/executor/tool"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/o1"
	"centra/agents/neo/internal/sessionjournal"
)

func (a *Agent) journalUser(ctx context.Context, message llm.Message) error {
	if a.journal == nil {
		return nil
	}
	apiContent, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("neo: encode journal user message: %w", err)
	}
	_, err = a.journal.Append(ctx, sessionjournal.Event{
		ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
		Attempt: a.supervisedAttempt, Kind: sessionjournal.KindUserMessage,
		DisplayContent: message.Content, APIContent: apiContent,
		Message: &sessionjournal.Message{Role: sessionjournal.RoleUser},
	})
	if err != nil {
		return fmt.Errorf("neo: persist user journal event: %w", err)
	}
	return nil
}

func (a *Agent) journalAssistant(ctx context.Context, message llm.Message) error {
	if a.journal == nil || message.IsGuidance() {
		return nil
	}
	apiContent, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("neo: encode journal assistant message: %w", err)
	}
	toolCalls := make([]sessionjournal.ToolCall, len(message.ToolCalls))
	for index, call := range message.ToolCalls {
		toolCalls[index] = a.journalToolCallPayload(call, index)
	}
	_, err = a.journal.Append(ctx, sessionjournal.Event{
		ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
		Attempt: a.supervisedAttempt, Kind: sessionjournal.KindAssistantMessage,
		DisplayContent: message.Content, APIContent: apiContent,
		Message: &sessionjournal.Message{
			Role: sessionjournal.RoleAssistant, ToolCalls: toolCalls,
		},
	})
	if err != nil {
		return fmt.Errorf("neo: persist assistant journal event: %w", err)
	}
	if strings.TrimSpace(message.Reasoning) != "" {
		if _, err := a.journal.Append(ctx, sessionjournal.Event{
			ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
			Attempt: a.supervisedAttempt, Kind: sessionjournal.KindReasoning,
			DisplayContent: message.Reasoning,
			Reasoning:      &sessionjournal.Reasoning{Text: message.Reasoning},
		}); err != nil {
			return fmt.Errorf("neo: persist reasoning journal event: %w", err)
		}
	}
	return nil
}

func (a *Agent) journalToolCalls(ctx context.Context, calls []llm.ToolCall) error {
	if a.journal == nil {
		return nil
	}
	for index, call := range calls {
		apiContent, err := json.Marshal(call)
		if err != nil {
			return fmt.Errorf("neo: encode journal tool call: %w", err)
		}
		payload := a.journalToolCallPayload(call, index)
		if _, err := a.journal.Append(ctx, sessionjournal.Event{
			ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
			Attempt: a.supervisedAttempt, Kind: sessionjournal.KindToolCall,
			DisplayContent: call.Function.Name, APIContent: apiContent,
			ToolCall: &payload,
		}); err != nil {
			return fmt.Errorf("neo: persist tool call before execution: %w", err)
		}
	}
	return nil
}

func (a *Agent) journalToolResult(
	ctx context.Context,
	call llm.ToolCall,
	index int,
	content string,
	evidence string,
	isErr bool,
	failureClass exectool.FailureClass,
) error {
	if a.journal == nil {
		return nil
	}
	name := call.Function.Name
	providerMessage := llm.ToolResult(call.ID, name, content)
	apiContent, err := json.Marshal(providerMessage)
	if err != nil {
		return fmt.Errorf("neo: encode journal tool result: %w", err)
	}
	if _, err := a.journal.Append(ctx, sessionjournal.Event{
		ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
		Attempt: a.supervisedAttempt, Kind: sessionjournal.KindToolResult,
		DisplayContent: evidence, APIContent: apiContent,
		ToolResult: &sessionjournal.ToolResult{
			CallID: a.journalCallID(call, index), Name: name,
			Result: []byte(evidence), IsError: isErr,
			FailureClass: string(failureClass),
		},
	}); err != nil {
		return fmt.Errorf("neo: persist tool result before exposure: %w", err)
	}
	return nil
}

func (a *Agent) journalUncertainEffect(
	ctx context.Context,
	call llm.ToolCall,
	index int,
	args map[string]interface{},
	evidence string,
	failureClass exectool.FailureClass,
) error {
	if a.journal == nil || failureClass != exectool.FailureTransport || a.o1Manifest(call.Function.Name).Effects == o1.EffectReadOnly {
		return nil
	}
	encodedArgs, _ := json.Marshal(args)
	if _, err := a.journal.Append(ctx, sessionjournal.Event{
		ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
		Attempt: a.supervisedAttempt, Kind: sessionjournal.KindUncertainEffect,
		DisplayContent: evidence,
		UncertainEffect: &sessionjournal.UncertainEffect{
			CallID:         a.journalCallID(call, index),
			IdempotencyKey: o1.ContentHash(encodedArgs),
			ToolName:       call.Function.Name, Status: "unknown",
			Evidence: []byte(evidence),
		},
	}); err != nil {
		return fmt.Errorf("neo: persist uncertain effect: %w", err)
	}
	return nil
}

func (a *Agent) journalRecovery(ctx context.Context, state, reason string) error {
	if a.journal == nil {
		return nil
	}
	_, err := a.journal.Append(ctx, sessionjournal.Event{
		ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
		Attempt: a.supervisedAttempt, Kind: sessionjournal.KindRecovery,
		DisplayContent: reason,
		Recovery:       &sessionjournal.Recovery{State: state, Reason: reason},
	})
	if err != nil {
		return fmt.Errorf("neo: persist recovery journal event: %w", err)
	}
	return nil
}

func (a *Agent) providerRequest(ctx context.Context, messages []llm.Message, schemas []llm.Tool, onDelta func(llm.Delta), maxTokens int) llm.ChatRequest {
	request := llm.ChatRequest{Messages: messages, Tools: schemas, OnDelta: onDelta, MaxTokens: maxTokens}
	if a.cfg.SessionExactProjection && a.journal != nil {
		request.OnRequest = func(body []byte) error {
			_, err := a.journal.Append(ctx, sessionjournal.Event{
				ConversationID: a.cmConvID(), TurnID: a.turnIntentID(),
				Attempt: a.supervisedAttempt, Kind: sessionjournal.KindProviderReplay,
				DisplayContent: "provider request",
				ProviderReplay: &sessionjournal.ProviderReplay{
					Provider: a.main.Provider(), Model: a.main.Model(), RequestBytes: body,
				},
			})
			if err != nil {
				return fmt.Errorf("neo: persist provider request: %w", err)
			}
			return nil
		}
	}
	return request
}

func (a *Agent) journalToolCallPayload(call llm.ToolCall, index int) sessionjournal.ToolCall {
	manifest := a.o1Manifest(call.Function.Name)
	return sessionjournal.ToolCall{
		CallID: a.journalCallID(call, index), Type: call.Type,
		Name: call.Function.Name, Arguments: []byte(call.Function.Arguments),
		StateChanging: manifest.Effects != o1.EffectReadOnly,
	}
}

func (a *Agent) journalCallID(call llm.ToolCall, index int) string {
	if id := strings.TrimSpace(call.ID); id != "" {
		return id
	}
	step := 0
	if a.turn != nil {
		step = a.turn.curStep
	}
	return fmt.Sprintf("neo-call-%d-%d-%d", a.turnSeq, step, index)
}
