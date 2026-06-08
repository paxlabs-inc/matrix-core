// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llm is Neo's OpenAI-compatible function-calling transport.
//
// Neo is a NORMAL agent: a system prompt + a running message list + native
// tool_calls + tool-result messages, looped until the model stops asking for
// tools. The conversation transcript IS the state.
//
// This is deliberately distinct from matrix/mcl's interpreter.LLM, whose
// Message is only {Role, Content} and which drives "tools" through grammar
// JSON + a planned walk (the compile→plan→execute paradigm). That message
// type is replay-critical (the D11 byte-identity invariant), so Neo does not
// extend it; it owns its own tool-calling message shape here. It DOES reuse
// matrix/mcl/llm's Config + provider detection + model registry so gateway
// metering and provider routing come for free (see client.go).
package llm

import "encoding/json"

// Role values for a conversation message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one turn in the conversation. It is both the on-wire shape and
// the in-transcript shape (Neo's state). An assistant turn may carry
// ToolCalls; a tool-result turn sets Role=tool + ToolCallID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`

	// Reasoning carries a reasoning model's chain-of-thought when the
	// provider surfaces it as a separate channel. Never serialized onto the
	// wire and never treated as the answer; surfaced as a distinct channel
	// only (mirrors MCL's DecodeWithReasoning posture).
	Reasoning string `json:"-"`
}

// ToolCall is a single function invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + JSON-encoded arguments of a requested call.
// Arguments is a STRING per the OpenAI contract (the model emits a JSON
// document as text); use ParseArgs to decode it.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseArgs decodes the tool call's JSON-encoded arguments. Empty/blank
// arguments decode to an empty map (a no-arg call).
func (tc ToolCall) ParseArgs() (map[string]interface{}, error) {
	args := map[string]interface{}{}
	s := tc.Function.Arguments
	if s == "" {
		return args, nil
	}
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		return nil, err
	}
	return args, nil
}

// Tool is a function schema advertised to the model.
type Tool struct {
	Type     string      `json:"type"` // always "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function. Parameters is a JSON Schema
// object (the same shape MCP tools publish as their inputSchema).
type FunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// NewFunctionTool builds a function Tool from a name, description and a JSON
// Schema parameters object.
func NewFunctionTool(name, description string, parameters map[string]interface{}) Tool {
	if parameters == nil {
		parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	return Tool{
		Type: "function",
		Function: FunctionDef{
			Name:        name,
			Description: description,
			Parameters:  parameters,
		},
	}
}

// SystemMessage builds a system-role message.
func SystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}

// UserMessage builds a user-role message.
func UserMessage(content string) Message {
	return Message{Role: RoleUser, Content: content}
}

// AssistantMessage builds an assistant-role text message (no tool calls). Used
// to seed a resumed conversation's transcript from durable history.
func AssistantMessage(content string) Message {
	return Message{Role: RoleAssistant, Content: content}
}

// ToolResult builds a tool-role message answering a specific tool call.
func ToolResult(callID, name, content string) Message {
	return Message{Role: RoleTool, ToolCallID: callID, Name: name, Content: content}
}

// ChatRequest is one round-trip to the model.
type ChatRequest struct {
	Messages []Message
	Tools    []Tool
	// ToolChoice is "auto" (default when Tools present), "none", or "required".
	ToolChoice string
}

// ChatResult is the model's single assistant turn plus metadata.
type ChatResult struct {
	Message      Message
	FinishReason string
	Usage        Usage
}

// HasToolCalls reports whether the assistant asked to call any tools.
func (r ChatResult) HasToolCalls() bool { return len(r.Message.ToolCalls) > 0 }

// Usage is the provider's token accounting for a call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
