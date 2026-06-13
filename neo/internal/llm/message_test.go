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

func TestExtractTokenToolCalls(t *testing.T) {
	// Full section with preamble + one call: the model wrote a sentence then
	// inlined a Kimi-grammar tool call. The call must be extracted and the
	// control tokens + JSON args must NOT leak into visible content.
	pre := "On it. Let me write that file.\n"
	in := pre + kimiSectionBegin + kimiCallBegin + "functions.write_file:0" +
		kimiArgBegin + `{"path":"/tmp/a.md","content":"hi"}` + kimiCallEnd + kimiSectionEnd
	visible, calls := extractTokenToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "write_file" {
		t.Errorf("name = %q, want write_file", calls[0].Function.Name)
	}
	if calls[0].Type != "function" || calls[0].ID == "" {
		t.Errorf("call not normalized: %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"path":"/tmp/a.md","content":"hi"}` {
		t.Errorf("args = %q", calls[0].Function.Arguments)
	}
	if visible != "On it. Let me write that file." {
		t.Errorf("visible leaked tokens: %q", visible)
	}

	// Two calls in one section get distinct synthesized IDs.
	multi := kimiSectionBegin +
		kimiCallBegin + "functions.fs__read_file:0" + kimiArgBegin + `{"path":"/a"}` + kimiCallEnd +
		kimiCallBegin + "functions.fs__read_file:1" + kimiArgBegin + `{"path":"/b"}` + kimiCallEnd +
		kimiSectionEnd
	_, calls = extractTokenToolCalls(multi)
	if len(calls) != 2 || calls[0].ID == calls[1].ID {
		t.Fatalf("two-call section: got %d calls, ids %q/%q", len(calls), idOf(calls, 0), idOf(calls, 1))
	}

	// Truncated/unterminated section (finish_reason=length): still extract and
	// leak nothing to the visible channel.
	trunc := "preface " + kimiSectionBegin + kimiCallBegin + "functions.deploy:0" + kimiArgBegin + `{"chain":125`
	visible, calls = extractTokenToolCalls(trunc)
	if len(calls) != 1 || calls[0].Function.Name != "deploy" {
		t.Fatalf("truncated extract failed: %+v", calls)
	}
	if visible != "preface" {
		t.Errorf("truncated visible leaked: %q", visible)
	}

	// Clean text with no token grammar is returned untouched, no calls.
	if v, c := extractTokenToolCalls("just a normal answer"); v != "just a normal answer" || c != nil {
		t.Errorf("clean content disturbed: %q %v", v, c)
	}
}

func idOf(calls []ToolCall, i int) string {
	if i < len(calls) {
		return calls[i].ID
	}
	return ""
}

func TestFromWireRespMessageExtractsTokenToolCalls(t *testing.T) {
	// End-to-end: a response with empty tool_calls but token-grammar calls in
	// content must surface structured ToolCalls (so HasToolCalls is true and
	// the agent runs them instead of saying the raw blob).
	in := kimiSectionBegin + kimiCallBegin + "functions.web_search:0" +
		kimiArgBegin + `{"q":"pax price"}` + kimiCallEnd + kimiSectionEnd
	m := fromWireRespMessage(wireRespMessage{Content: in})
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "web_search" {
		t.Fatalf("token tool call not surfaced: %+v", m.ToolCalls)
	}
	if m.Content != "" {
		t.Errorf("content should be empty after extraction, got %q", m.Content)
	}

	// A well-behaved structured response is never disturbed by the extractor.
	m = fromWireRespMessage(wireRespMessage{
		Content:   "here you go",
		ToolCalls: []ToolCall{{ID: "x", Type: "function", Function: FunctionCall{Name: "f"}}},
	})
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "x" || m.Content != "here you go" {
		t.Errorf("structured response disturbed: %+v content=%q", m.ToolCalls, m.Content)
	}
}
