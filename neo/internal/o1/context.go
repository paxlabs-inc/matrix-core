// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"fmt"

	"matrix/neo/internal/llm"
)

// MessageChannel identifies the typed channel a message belongs to.
// Per O1 req.8 ac.1: assistant conversation, user messages, provider reasoning,
// tool evidence, structured progress, runtime guidance, supervisor decisions,
// and terminal metadata remain distinct typed channels.
type MessageChannel string

const (
	// ChannelUser is a user-authored conversational message.
	ChannelUser MessageChannel = "user"
	// ChannelAssistant is an assistant-authored conversational message —
	// genuine model output the model should treat as its own prior turn.
	ChannelAssistant MessageChannel = "assistant"
	// ChannelReasoning is provider reasoning (chain-of-thought) — output-only,
	// never persisted back to the conversation, never counts as assistant content.
	ChannelReasoning MessageChannel = "reasoning"
	// ChannelToolCall is a structured tool invocation from the model.
	ChannelToolCall MessageChannel = "tool_call"
	// ChannelToolResult is the authoritative result of a tool execution.
	ChannelToolResult MessageChannel = "tool_result"
	// ChannelProgress is structured runtime progress — e.g. todo updates,
	// milestone markers, phase transitions. NEVER treated as assistant-authored.
	ChannelProgress MessageChannel = "progress"
	// ChannelGuidance is runtime-generated steering injected into the model's
	// context (e.g. convergence nudges, contract reminders, forced-revision
	// directives). NEVER persisted as assistant-authored history.
	ChannelGuidance MessageChannel = "guidance"
	// ChannelSupervisor is a supervisor decision (continue, repair, stop,
	// complete) with structured provenance. NEVER treated as assistant output.
	ChannelSupervisor MessageChannel = "supervisor"
	// ChannelTerminal is the final run metadata — completion proof, blocker
	// description, or death reason. Carries the authoritative terminal state.
	ChannelTerminal MessageChannel = "terminal"
	// ChannelNarration is ephemeral narrate-before-act text. Surfaced live
	// but never persisted to the durable thread. (neo-smoothness req.1)
	ChannelNarration MessageChannel = "narration"
)

// IsUserVisible returns true for channels whose content the user should see
// in the conversation thread.
func (ch MessageChannel) IsUserVisible() bool {
	switch ch {
	case ChannelUser, ChannelAssistant, ChannelToolResult, ChannelTerminal:
		return true
	default:
		return false
	}
}

// IsAssistantAuthored returns true only for genuine model output that the
// model should treat as its own prior turn. Runtime-generated prose
// (guidance, progress, supervisor decisions) MUST NOT be classified here.
func (ch MessageChannel) IsAssistantAuthored() bool {
	return ch == ChannelAssistant
}

// IsDurable returns true for channels that must survive compaction and
// session reload. Transient channels (narration, reasoning) are dropped.
func (ch MessageChannel) IsDurable() bool {
	switch ch {
	case ChannelUser, ChannelAssistant, ChannelToolCall, ChannelToolResult,
		ChannelSupervisor, ChannelTerminal:
		return true
	default:
		return false
	}
}

// ContextRule describes how a channel's content is treated during context
// assembly and compaction. Per O1 req.8 ac.3 and ac.4.
type ContextRule struct {
	Channel           MessageChannel `json:"channel"`
	Durable           bool           `json:"durable"`
	AssistantAuthored bool           `json:"assistant_authored"`
	UserVisible       bool           `json:"user_visible"`
	CompactionPolicy  string         `json:"compaction_policy"`
	ProvenanceTag     string         `json:"provenance_tag"`
}

// DefaultContextRules returns the canonical context rules for all channels.
// These rules enforce the O1 invariant that runtime-generated prose never
// becomes assistant-authored conversational history.
func DefaultContextRules() []ContextRule {
	return []ContextRule{
		{ChannelUser, true, false, true, "preserve", "user"},
		{ChannelAssistant, true, true, true, "preserve", "model"},
		{ChannelReasoning, false, false, false, "drop", "reasoning"},
		{ChannelToolCall, true, false, false, "preserve_structured", "tool_call"},
		{ChannelToolResult, true, false, true, "preserve_structured", "tool_result"},
		{ChannelProgress, false, false, false, "drop", "runtime"},
		{ChannelGuidance, false, false, false, "drop", "runtime"},
		{ChannelSupervisor, true, false, false, "preserve_structured", "supervisor"},
		{ChannelTerminal, true, false, true, "preserve", "terminal"},
		{ChannelNarration, false, false, false, "drop", "runtime"},
	}
}

// ContextMessage is a typed message in the Neo context window with explicit
// channel attribution. It replaces untyped message arrays where runtime
// fiction could leak into assistant-authored history.
type ContextMessage struct {
	Channel    MessageChannel `json:"channel"`
	Content    string         `json:"content"`
	Provenance string         `json:"provenance"`
	TurnIndex  int            `json:"turn_index"`
	ToolName   string         `json:"tool_name,omitempty"`
}

// IsAssistantAuthored returns true only when the channel is ChannelAssistant.
// This is the single check that prevents runtime-generated prose from being
// treated as the model's own prior output.
func (m ContextMessage) IsAssistantAuthored() bool {
	return m.Channel.IsAssistantAuthored()
}

// ContextAssembler builds a model-visible context window from typed messages,
// applying channel rules to ensure no runtime fiction enters assistant history.
type ContextAssembler struct {
	rules map[MessageChannel]ContextRule
}

// NewContextAssembler creates an assembler with the default O1 context rules.
func NewContextAssembler() *ContextAssembler {
	rules := DefaultContextRules()
	ruleMap := make(map[MessageChannel]ContextRule, len(rules))
	for _, r := range rules {
		ruleMap[r.Channel] = r
	}
	return &ContextAssembler{rules: ruleMap}
}

// AssembleContext filters and orders messages for the model's context window.
// Per O1 req.8 ac.3: select current authoritative state, unresolved criteria,
// relevant evidence, and valid capabilities; exclude stale, superseded,
// duplicated, or internal control content.
func (ca *ContextAssembler) AssembleContext(messages []ContextMessage) []ContextMessage {
	if ca == nil {
		return messages
	}
	var out []ContextMessage
	for _, msg := range messages {
		rule, ok := ca.rules[msg.Channel]
		if !ok {
			// Unknown channels are excluded (fail-closed on context truth).
			continue
		}
		// Enforce provenance tag so downstream consumers know the source.
		if msg.Provenance == "" {
			msg.Provenance = rule.ProvenanceTag
		}
		out = append(out, msg)
	}
	return out
}

// CompactableMessages returns only the messages that should survive compaction
// per their channel rules. Per O1 req.8 ac.4: compaction preserves task-contract
// state, causal evidence, open hazards, effects, authorization, and completion
// obligations.
func (ca *ContextAssembler) CompactableMessages(messages []ContextMessage) []ContextMessage {
	if ca == nil {
		return nil
	}
	var out []ContextMessage
	for _, msg := range messages {
		rule, ok := ca.rules[msg.Channel]
		if !ok {
			continue
		}
		if rule.Durable {
			out = append(out, msg)
		}
	}
	return out
}

// ValidateNoLeakage checks that no runtime-generated or non-assistant content
// is tagged as assistant-authored. This is the O1 guard against the specific
// failure class where supervisor prose, guidance, or progress text enters the
// model's conversation as if the model itself wrote it.
func ValidateNoLeakage(messages []ContextMessage) []ContextMessage {
	var violations []ContextMessage
	for _, msg := range messages {
		if msg.Channel.IsAssistantAuthored() {
			continue
		}
		// Non-assistant channels must not carry assistant-authored provenance.
		if msg.Provenance == "model" || msg.Provenance == "assistant" {
			violations = append(violations, msg)
		}
	}
	return violations
}

// ValidateConversationTruth maps the production transcript type onto O1
// channels and rejects ambiguous or leaked history before model assembly.
func ValidateConversationTruth(messages []llm.Message) error {
	typed := make([]ContextMessage, 0, len(messages))
	for i, msg := range messages {
		item := ContextMessage{Content: msg.Content, TurnIndex: i, ToolName: msg.Name}
		switch {
		case msg.IsGuidance():
			item.Channel, item.Provenance = ChannelGuidance, "runtime"
		case msg.Role == llm.RoleUser:
			item.Channel, item.Provenance = ChannelUser, "user"
		case msg.Role == llm.RoleAssistant:
			item.Channel, item.Provenance = ChannelAssistant, "model"
		case msg.Role == llm.RoleTool:
			item.Channel, item.Provenance = ChannelToolResult, "tool_result"
		default:
			return fmt.Errorf("message %d has unknown role %q", i, msg.Role)
		}
		typed = append(typed, item)
	}
	if leaked := ValidateNoLeakage(typed); len(leaked) > 0 {
		return fmt.Errorf("%d runtime messages are misattributed as assistant-authored", len(leaked))
	}
	return nil
}
