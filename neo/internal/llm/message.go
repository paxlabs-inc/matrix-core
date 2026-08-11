// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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

import (
	"encoding/json"
	"strings"
)

// Role values for a conversation message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Guidance-channel envelope tags. System steering injected through the guidance
// channel is wrapped in these so the model can recognize it as a non-answerable
// hint (the Cascade <system_guidance> contract), and so the harness can detect
// and strip guidance turns from every user-facing surface.
const (
	GuidanceOpen  = "<system_guidance>"
	GuidanceClose = "</system_guidance>"
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
	AudioData  string     `json:"-"`
	// ImageData is a data: URI (data:image/png;base64,…) carried as one MiMo
	// image_url part on a user turn — the stateless vision-grounding input for
	// DOJO's desktop_look. Process-only (never serialized to the durable
	// transcript); the durable record keeps Content plus a media reference.
	ImageData string `json:"-"`

	// Reasoning carries a reasoning model's chain-of-thought when the
	// provider surfaces it as a separate channel. Never serialized onto the
	// wire and never treated as the answer; surfaced as a distinct channel
	// only (mirrors MCL's DecodeWithReasoning posture).
	Reasoning string `json:"-"`

	// Guidance marks this as a guidance-channel turn: system steering the
	// model is instructed to act on but never acknowledge or echo (the Cascade
	// <system_guidance> contract). It rides on the wire as an ordinary
	// role+content turn (the flag is process-only, never serialized), so the
	// provider accepts it; the flag lets the harness keep guidance out of every
	// user-facing surface and out of the durable transcript.
	Guidance bool `json:"-"`
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

// UserAudioMessage builds one user turn whose on-wire content contains the
// visible transcript/marker and one MiMo input_audio part. AudioData is
// process-only: durable transcript and Neocortex records store Content plus the
// sealed media reference, never raw audio bytes.
func UserAudioMessage(content, dataURL string) Message {
	return Message{Role: RoleUser, Content: content, AudioData: dataURL}
}

// UserImageMessage builds one user turn whose on-wire content contains the
// instruction text and one MiMo image_url part. ImageData is a data: URI and is
// process-only: it is the stateless grounding input for desktop_look, never
// stored in the durable transcript.
func UserImageMessage(content, dataURL string) Message {
	return Message{Role: RoleUser, Content: content, ImageData: dataURL}
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

// GuidanceMessage builds a guidance-channel turn: system steering the model is
// instructed to act on but never acknowledge or echo (the Cascade
// <system_guidance> contract). It rides as a SYSTEM-role turn — the steering
// comes from the system, not the user, and every OpenAI-compatible provider
// accepts a system message mid-conversation — with the content wrapped in the
// guidance envelope and the Guidance flag set so the harness keeps it off every
// user-facing surface (reasoning/thinking deltas and the durable transcript).
//
// The system role matters for correctness, not just labeling: the transcript's
// compaction/tail logic (safeTail, splitForCompaction) anchors the retained
// window on the last USER message. A guidance nudge delivered as a user turn
// becomes that anchor, so once the window compacts the real task transcript is
// trimmed away and the model — now seeing only the system prompt plus a bare
// nudge — greets as if a fresh conversation and never completes, the nudge
// spiral. Keeping guidance on the system role leaves the true user task as the
// anchor. Blank content yields a zero Message (no empty guidance turn emitted).
func GuidanceMessage(content string) Message {
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}
	}
	return Message{
		Role:     RoleSystem,
		Content:  GuidanceOpen + "\n" + content + "\n" + GuidanceClose,
		Guidance: true,
	}
}

// IsGuidance reports whether m is a guidance-channel turn (set by
// GuidanceMessage). The harness uses it to exclude internal steering from every
// user-facing surface and from the persisted transcript.
func (m Message) IsGuidance() bool { return m.Guidance }

// StripGuidance removes any guidance-channel envelope (and stray lone tags) from
// a string before it reaches a user-facing surface (reasoning/thinking deltas,
// the assistant transcript). It is a defense-in-depth backstop for req.1.3: the
// model is instructed never to echo <system_guidance>, but if it does, internal
// steering must not stream to the user. Complete <system_guidance>…</system_guidance>
// blocks are dropped; an unterminated open tag drops everything from the tag
// onward; bare tags are removed. The surrounding (legitimate) text is otherwise
// preserved verbatim — no trimming — so streamed fragments keep their spacing.
func StripGuidance(s string) string {
	if !strings.Contains(s, GuidanceOpen) && !strings.Contains(s, GuidanceClose) {
		return s
	}
	for {
		i := strings.Index(s, GuidanceOpen)
		if i < 0 {
			break
		}
		rest := s[i+len(GuidanceOpen):]
		j := strings.Index(rest, GuidanceClose)
		if j < 0 {
			// Open tag with no close: drop from the tag to the end.
			s = s[:i]
			break
		}
		s = s[:i] + rest[j+len(GuidanceClose):]
	}
	s = strings.ReplaceAll(s, GuidanceOpen, "")
	s = strings.ReplaceAll(s, GuidanceClose, "")
	return s
}

// ChatRequest is one round-trip to the model.
type ChatRequest struct {
	Messages []Message
	Tools    []Tool
	// ToolChoice is "auto" (default when Tools present), "none", or "required".
	ToolChoice string
	// MaxTokens, when > 0, overrides the client's configured output-token limit
	// for THIS request only (0 keeps the client default). The agent raises it on a
	// truncated generation so a long final synthesis can finish instead of
	// re-truncating at the same limit. For MiMo/xAI it maps to max_completion_tokens
	// (visible output only); for every other provider to max_tokens.
	MaxTokens int
	// OnDelta, when set, is invoked with incremental fragments of the model's
	// CURRENT turn as they stream from the provider, BEFORE the folded turn is
	// returned. It lets the caller surface a live "typing" channel (the visible
	// answer being typed) and a live "thinking" channel (the reasoning). The
	// visible-content fragments are already cleaned (leading <think> blocks and
	// the Kimi tool-call token grammar are withheld), so they are safe to show
	// verbatim. Fragments are best-effort coalesced and never delivered after
	// Chat returns. Leave nil to disable streaming (buffered turn only).
	OnDelta   func(Delta)
	OnRequest func([]byte) error
}

func ContextMessage(content string) Message {
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}
	}
	return Message{Role: RoleSystem, Content: "<session_context>\n" + content + "\n</session_context>", Guidance: true}
}

// Delta is one coalesced streaming fragment of an in-flight assistant turn.
// At most one of the channels is non-empty per call.
type Delta struct {
	// Content is a fragment of the visible answer text being generated.
	Content string
	// Reasoning is a fragment of the model's chain-of-thought (reasoning
	// channel). Never part of the answer.
	Reasoning string
	// Tool is a fragment of a STREAMING tool call's argument JSON — the live
	// "Neo is typing a file" channel. It carries the raw bytes exactly as the
	// model generates them (append-only per Index); the consumer decodes them
	// incrementally. The folded ToolCalls on the returned message stay the
	// authoritative complete arguments.
	Tool *ToolCallDelta
}

// ToolCallDelta is one incremental fragment of an in-flight tool call.
type ToolCallDelta struct {
	// Index is the call's position in the assistant turn (stable across
	// fragments of the same call).
	Index int
	// ID and Name carry the call id / function name as soon as the stream has
	// revealed them (repeated verbatim on every fragment thereafter).
	ID   string
	Name string
	// Args is the next raw argument-JSON fragment (append-only).
	Args string
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
	PromptTokens        int          `json:"prompt_tokens"`
	CompletionTokens    int          `json:"completion_tokens"`
	TotalTokens         int          `json:"total_tokens"`
	PromptTokensDetails TokenDetails `json:"prompt_tokens_details,omitempty"`
}

// TokenDetails carries provider accounting for non-text prompt modalities.
type TokenDetails struct {
	AudioTokens int `json:"audio_tokens,omitempty"`
}
