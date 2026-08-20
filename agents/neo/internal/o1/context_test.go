// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"

	"centra/agents/neo/internal/llm"
)

func TestChannelProperties(t *testing.T) {
	// Assistant is the only channel that is assistant-authored.
	if !ChannelAssistant.IsAssistantAuthored() {
		t.Fatal("ChannelAssistant should be assistant-authored")
	}
	for _, ch := range []MessageChannel{ChannelUser, ChannelGuidance, ChannelProgress, ChannelSupervisor, ChannelNarration, ChannelReasoning} {
		if ch.IsAssistantAuthored() {
			t.Fatalf("%s should NOT be assistant-authored", ch)
		}
	}
}

func TestChannelUserVisibility(t *testing.T) {
	visible := []MessageChannel{ChannelUser, ChannelAssistant, ChannelToolResult, ChannelTerminal}
	for _, ch := range visible {
		if !ch.IsUserVisible() {
			t.Fatalf("%s should be user-visible", ch)
		}
	}
	hidden := []MessageChannel{ChannelGuidance, ChannelProgress, ChannelSupervisor, ChannelNarration, ChannelReasoning, ChannelToolCall}
	for _, ch := range hidden {
		if ch.IsUserVisible() {
			t.Fatalf("%s should NOT be user-visible", ch)
		}
	}
}

func TestChannelDurability(t *testing.T) {
	durable := []MessageChannel{ChannelUser, ChannelAssistant, ChannelToolCall, ChannelToolResult, ChannelSupervisor, ChannelTerminal}
	for _, ch := range durable {
		if !ch.IsDurable() {
			t.Fatalf("%s should be durable", ch)
		}
	}
	transient := []MessageChannel{ChannelReasoning, ChannelProgress, ChannelGuidance, ChannelNarration}
	for _, ch := range transient {
		if ch.IsDurable() {
			t.Fatalf("%s should NOT be durable", ch)
		}
	}
}

func TestContextAssemblerFiltersUnknownChannels(t *testing.T) {
	ca := NewContextAssembler()
	msgs := []ContextMessage{
		{Channel: ChannelUser, Content: "hello"},
		{Channel: "unknown_channel", Content: "should be dropped"},
		{Channel: ChannelAssistant, Content: "hi"},
	}
	assembled := ca.AssembleContext(msgs)
	if len(assembled) != 2 {
		t.Fatalf("unknown channels should be dropped, got %d", len(assembled))
	}
}

func TestContextAssemblerAssignsProvenance(t *testing.T) {
	ca := NewContextAssembler()
	msgs := []ContextMessage{
		{Channel: ChannelGuidance, Content: "nudge"},
	}
	assembled := ca.AssembleContext(msgs)
	if len(assembled) != 1 || assembled[0].Provenance != "runtime" {
		t.Fatalf("guidance should get runtime provenance, got %v", assembled)
	}
}

func TestCompactableMessagesExcludesTransient(t *testing.T) {
	ca := NewContextAssembler()
	msgs := []ContextMessage{
		{Channel: ChannelUser, Content: "user turn"},
		{Channel: ChannelAssistant, Content: "assistant turn"},
		{Channel: ChannelGuidance, Content: "nudge"},
		{Channel: ChannelProgress, Content: "step done"},
		{Channel: ChannelNarration, Content: "about to..."},
		{Channel: ChannelToolResult, Content: "result"},
		{Channel: ChannelTerminal, Content: "done"},
	}
	compacted := ca.CompactableMessages(msgs)
	if len(compacted) != 4 {
		t.Fatalf("expected 4 compactable messages, got %d", len(compacted))
	}
	for _, m := range compacted {
		if m.Channel == ChannelGuidance || m.Channel == ChannelProgress || m.Channel == ChannelNarration {
			t.Fatalf("transient channel %s should not survive compaction", m.Channel)
		}
	}
}

func TestValidateNoLeakageDetectsMisattributedProvenance(t *testing.T) {
	msgs := []ContextMessage{
		{Channel: ChannelAssistant, Content: "genuine", Provenance: "model"},
		{Channel: ChannelGuidance, Content: "nudge", Provenance: "model"},       // violation
		{Channel: ChannelProgress, Content: "step", Provenance: "assistant"},    // violation
		{Channel: ChannelSupervisor, Content: "stop", Provenance: "supervisor"}, // ok
	}
	violations := ValidateNoLeakage(msgs)
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(violations))
	}
}

func TestValidateNoLeakageClean(t *testing.T) {
	msgs := []ContextMessage{
		{Channel: ChannelUser, Content: "hello", Provenance: "user"},
		{Channel: ChannelAssistant, Content: "hi", Provenance: "model"},
		{Channel: ChannelGuidance, Content: "nudge", Provenance: "runtime"},
	}
	violations := ValidateNoLeakage(msgs)
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations, got %d", len(violations))
	}
}

func TestValidateConversationTruthSeparatesGuidanceFromAssistantHistory(t *testing.T) {
	messages := []llm.Message{
		llm.UserMessage("do the work"),
		llm.AssistantMessage("working"),
		llm.GuidanceMessage("verify before completion"),
		llm.ToolResult("call-1", "fs__read_file", "evidence"),
	}
	if err := ValidateConversationTruth(messages); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConversationTruthRejectsUnknownRole(t *testing.T) {
	if err := ValidateConversationTruth([]llm.Message{{Role: "supervisor", Content: "fiction"}}); err == nil {
		t.Fatal("unknown runtime prose must not enter conversational history")
	}
}
