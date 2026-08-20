// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"encoding/json"
	"testing"
)

// A strict provider deserializer (xAI) rejects the whole chat request with a
// non-retryable 422 ("missing field `content`") when any message on the wire
// omits the content key. An assistant turn that carries only tool calls and a
// tool result whose output is empty both have Content == "", so the wire
// shape must serialize an explicit "content":"" — never drop the key.
func TestWireMessagesAlwaysCarryContentKey(t *testing.T) {
	msgs := []Message{
		SystemMessage("sys"),
		UserMessage("do the thing"),
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c1", Type: "function",
			Function: FunctionCall{Name: "run_command", Arguments: `{"cmd":"true"}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: ""},
	}
	data, err := json.Marshal(toWireMessages(msgs, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != len(msgs) {
		t.Fatalf("expected %d wire messages, got %d", len(msgs), len(decoded))
	}
	for i, m := range decoded {
		raw, ok := m["content"]
		if !ok {
			t.Errorf("message %d omits the content key — a strict provider 422s the whole request", i)
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("message %d content is not a string: %s", i, raw)
		}
	}
}
