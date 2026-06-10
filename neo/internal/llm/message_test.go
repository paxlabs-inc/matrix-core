// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import "testing"

func TestParseArgs(t *testing.T) {
	// empty arguments -> empty map, no error (a no-arg call)
	tc := ToolCall{Function: FunctionCall{Name: "x", Arguments: ""}}
	args, err := tc.ParseArgs()
	if err != nil || len(args) != 0 {
		t.Fatalf("empty args: got %v err %v", args, err)
	}

	tc = ToolCall{Function: FunctionCall{Name: "x", Arguments: `{"path":"/tmp","n":3}`}}
	args, err = tc.ParseArgs()
	if err != nil {
		t.Fatalf("valid args err: %v", err)
	}
	if args["path"] != "/tmp" {
		t.Errorf("path = %v", args["path"])
	}

	tc = ToolCall{Function: FunctionCall{Name: "x", Arguments: `{not json}`}}
	if _, err := tc.ParseArgs(); err == nil {
		t.Error("expected error on invalid JSON args")
	}
}

func TestNewFunctionToolDefaults(t *testing.T) {
	tool := NewFunctionTool("do_thing", "desc", nil)
	if tool.Type != "function" || tool.Function.Name != "do_thing" {
		t.Fatalf("bad tool: %+v", tool)
	}
	if tool.Function.Parameters["type"] != "object" {
		t.Errorf("nil params should default to an object schema: %v", tool.Function.Parameters)
	}
	if _, ok := tool.Function.Parameters["properties"]; !ok {
		t.Error("default schema must carry a properties map")
	}
}

func TestMessageConstructors(t *testing.T) {
	if SystemMessage("s").Role != RoleSystem {
		t.Error("system role")
	}
	if UserMessage("u").Role != RoleUser {
		t.Error("user role")
	}
	tr := ToolResult("call-1", "fs__read_file", "contents")
	if tr.Role != RoleTool || tr.ToolCallID != "call-1" || tr.Name != "fs__read_file" {
		t.Errorf("tool result shape wrong: %+v", tr)
	}
}

func TestHasToolCalls(t *testing.T) {
	r := ChatResult{Message: Message{ToolCalls: []ToolCall{{ID: "1"}}}}
	if !r.HasToolCalls() {
		t.Error("should report tool calls")
	}
	if (ChatResult{}).HasToolCalls() {
		t.Error("empty result should have no tool calls")
	}
}

func TestWireRoundTrip(t *testing.T) {
	in := []Message{
		SystemMessage("be good"),
		UserMessage("hello"),
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
		ToolResult("c1", "f", "ok"),
	}
	w := toWireMessages(in)
	if len(w) != len(in) {
		t.Fatalf("wire length mismatch")
	}
	if w[3].ToolCallID != "c1" || w[3].Name != "f" {
		t.Errorf("tool-result wire shape wrong: %+v", w[3])
	}
}

func TestSplitInlineThink(t *testing.T) {
	cases := []struct {
		in, visible, reasoning string
	}{
		{"plain answer", "plain answer", ""},
		{"<think>hmm</think>the answer", "the answer", "hmm"},
		{"  <thinking>deep\nthought</thinking>  final", "final", "deep\nthought"},
		// Unterminated tag (truncated generation): the WHOLE remainder is
		// reasoning — nothing may leak to the visible channel.
		{"<think>I should run echo iVBOR...", "", "I should run echo iVBOR..."},
		{"mid <think>x</think> sentence", "mid <think>x</think> sentence", ""},
	}
	for _, c := range cases {
		v, r := splitInlineThink(c.in)
		if v != c.visible || r != c.reasoning {
			t.Errorf("splitInlineThink(%q) = (%q, %q), want (%q, %q)", c.in, v, r, c.visible, c.reasoning)
		}
	}
}

func TestFromWireRespMessageReasoningAndDefaults(t *testing.T) {
	// content null + only a reasoning channel + empty role.
	m := fromWireRespMessage(wireRespMessage{
		Role:             "",
		Content:          "",
		ReasoningContent: "thinking...",
		ToolCalls:        []ToolCall{{ID: "1"}},
	})
	if m.Role != RoleAssistant {
		t.Errorf("empty role should default to assistant, got %q", m.Role)
	}
	if m.Reasoning != "thinking..." {
		t.Errorf("reasoning channel lost: %q", m.Reasoning)
	}
	if len(m.ToolCalls) != 1 {
		t.Error("tool calls lost")
	}
}
