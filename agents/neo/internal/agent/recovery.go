// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"centra/agents/neo/internal/llm"
)

type localRecoveryObservation struct {
	Kind       string `json:"kind"`
	Tool       string `json:"tool,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	Detail     string `json:"detail"`
	Correction string `json:"correction"`
}

func recoveryObservation(kind, tool, callID, detail, correction string) string {
	payload, _ := json.Marshal(localRecoveryObservation{
		Kind: kind, Tool: tool, CallID: callID, Detail: detail, Correction: correction,
	})
	return string(payload)
}

func (a *Agent) malformedArgumentsObservation(call llm.ToolCall, err error) string {
	if !a.cfg.InteractionPosture {
		return fmt.Sprintf("could not parse arguments (%v). Re-issue the call with valid JSON arguments.", err)
	}
	a.turn.localRecoveries++
	return recoveryObservation(
		"invalid_tool_arguments", call.Function.Name, call.ID, err.Error(),
		"Re-issue this call with one valid JSON object matching the advertised schema.",
	)
}

func (a *Agent) unavailableToolObservation(name string) string {
	if !a.cfg.InteractionPosture {
		return fmt.Sprintf("tool %q is not available in this session — use only the tools you were given.", name)
	}
	a.turn.localRecoveries++
	return recoveryObservation(
		"invalid_tool_name", name, "", "the requested function is not in the advertised tool surface",
		"Choose an exact function name from the currently advertised tools or answer without a tool.",
	)
}

func (a *Agent) LocalRecoveryCount() int {
	if a == nil || a.turn == nil {
		return 0
	}
	return a.turn.localRecoveries
}

func (a *Agent) normalizeProviderResult(result *llm.ChatResult) (repairable bool) {
	if !a.cfg.InteractionPosture {
		return false
	}
	if result == nil {
		return false
	}
	if result.Message.Role == "" {
		result.Message.Role = llm.RoleAssistant
	}
	if strings.TrimSpace(result.Message.Content) == "" && len(result.Message.ToolCalls) == 0 {
		a.turn.localRecoveries++
		if result.FinishReason == "" {
			result.FinishReason = "stop"
		}
		return true
	}
	for index := range result.Message.ToolCalls {
		call := &result.Message.ToolCalls[index]
		if strings.TrimSpace(call.ID) == "" {
			call.ID = fmt.Sprintf("local-recovery-%d-%d", a.turnSeq, index)
			a.turn.localRecoveries++
		}
		if call.Type == "" {
			call.Type = "function"
		}
		if strings.TrimSpace(call.Function.Name) == "" {
			call.Function.Name = "invalid_tool_name"
			a.turn.localRecoveries++
			repairable = true
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args != "" && !json.Valid([]byte(args)) {
			repairable = true
		}
	}
	return repairable
}

func (a *Agent) repairWorkingProtocol() {
	if !a.cfg.InteractionPosture || len(a.working) == 0 {
		return
	}
	pending := make(map[string]llm.ToolCall)
	order := make([]string, 0)
	result := make([]llm.Message, 0, len(a.working)+2)
	flushMissing := func() {
		for _, id := range order {
			call, ok := pending[id]
			if !ok {
				continue
			}
			result = append(result, llm.ToolResult(id, call.Function.Name, recoveryObservation(
				"missing_tool_result", call.Function.Name, id, "the prior call ended without a matching result",
				"Inspect current state before deciding whether to retry; do not assume the effect did or did not happen.",
			)))
			a.turn.localRecoveries++
			delete(pending, id)
		}
		order = order[:0]
	}
	for _, message := range a.working {
		if message.Role != llm.RoleTool && len(pending) > 0 {
			flushMissing()
		}
		if message.Role == llm.RoleAssistant && len(message.ToolCalls) > 0 {
			for index := range message.ToolCalls {
				call := &message.ToolCalls[index]
				if strings.TrimSpace(call.ID) == "" {
					call.ID = fmt.Sprintf("repaired-history-%d-%d", len(result), index)
					a.turn.localRecoveries++
				}
				pending[call.ID] = *call
				order = append(order, call.ID)
			}
			result = append(result, message)
			continue
		}
		if message.Role == llm.RoleTool {
			if _, ok := pending[message.ToolCallID]; ok {
				result = append(result, message)
				delete(pending, message.ToolCallID)
				continue
			}
			guidance := a.pushGuidance(recoveryObservation(
				"orphaned_tool_result", message.Name, message.ToolCallID, message.Content,
				"Treat this as unpaired historical evidence and do not infer that a current call completed.",
			))
			result = append(result, guidance)
			a.turn.localRecoveries++
			continue
		}
		result = append(result, message)
	}
	flushMissing()
	a.working = result
}
