// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"strings"
	"testing"
)

// TestExtractXMLToolCalls pins FIX 3: grok/xAI reasoning models sometimes emit
// tool calls as inline XML-ish text (`<function_call name="NAME">ARGS</function_call>`)
// inside content instead of the structured tool_calls array. extractXMLToolCalls
// pulls those spans out, normalizes ARGS to a JSON object, strips the markup from
// the visible content, and tolerates a missing/truncated close tag.
func TestExtractXMLToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantContent string
		wantCalls   []struct{ name, args string }
	}{
		{
			name:        "single call, content fully stripped",
			content:     `<function_call name="construct_render">{"value":"8.51"}</function_call>`,
			wantContent: "",
			wantCalls:   []struct{ name, args string }{{"construct_render", `{"value":"8.51"}`}},
		},
		{
			name:        "preamble preserved, call extracted",
			content:     `Putting that on your screen. <function_call name="paxeer__price">{"symbol":"PAX"}</function_call>`,
			wantContent: "Putting that on your screen.",
			wantCalls:   []struct{ name, args string }{{"paxeer__price", `{"symbol":"PAX"}`}},
		},
		{
			name:        "prose around args normalized to first JSON object",
			content:     `<function_call name="x">the args are {"a":1} ok</function_call>`,
			wantContent: "",
			wantCalls:   []struct{ name, args string }{{"x", `{"a":1}`}},
		},
		{
			name:        "no JSON args normalizes to empty object",
			content:     `<function_call name="x">no json here</function_call>`,
			wantContent: "",
			wantCalls:   []struct{ name, args string }{{"x", "{}"}},
		},
		{
			name:        "unterminated call consumes remainder, no leak",
			content:     `<function_call name="x">{"a":1`,
			wantContent: "",
			wantCalls:   []struct{ name, args string }{{"x", "{}"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotContent, gotCalls := extractXMLToolCalls(tt.content)
			if gotContent != tt.wantContent {
				t.Errorf("content = %q, want %q", gotContent, tt.wantContent)
			}
			if strings.Contains(gotContent, "<function_call") {
				t.Errorf("stripped content still contains raw markup: %q", gotContent)
			}
			if len(gotCalls) != len(tt.wantCalls) {
				t.Fatalf("calls = %d, want %d (%+v)", len(gotCalls), len(tt.wantCalls), gotCalls)
			}
			for i, want := range tt.wantCalls {
				if gotCalls[i].Function.Name != want.name {
					t.Errorf("call[%d].name = %q, want %q", i, gotCalls[i].Function.Name, want.name)
				}
				if gotCalls[i].Function.Arguments != want.args {
					t.Errorf("call[%d].args = %q, want %q", i, gotCalls[i].Function.Arguments, want.args)
				}
			}
		})
	}
}

// TestExtractXMLToolCallsPlainContent verifies plain content with no
// `<function_call` span is returned unchanged with zero calls.
func TestExtractXMLToolCallsPlainContent(t *testing.T) {
	content := "Just a normal answer with no tool calls."
	gotContent, gotCalls := extractXMLToolCalls(content)
	if gotContent != content {
		t.Errorf("content = %q, want unchanged %q", gotContent, content)
	}
	if len(gotCalls) != 0 {
		t.Errorf("calls = %d, want 0", len(gotCalls))
	}
}

// TestFromWireRespMessageExtractsXMLToolCalls proves the wire-in: a
// wireRespMessage with empty tool_calls and content carrying an inline
// `<function_call ...>` span yields a Message whose ToolCalls holds the
// extracted call and whose Content no longer contains the raw markup.
func TestFromWireRespMessageExtractsXMLToolCalls(t *testing.T) {
	m := fromWireRespMessage(wireRespMessage{
		Role:    "assistant",
		Content: `On it. <function_call name="get_price">{"symbol":"PAX"}</function_call>`,
	})
	if len(m.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(m.ToolCalls))
	}
	if m.ToolCalls[0].Function.Name != "get_price" {
		t.Errorf("tool call name = %q, want get_price", m.ToolCalls[0].Function.Name)
	}
	if m.ToolCalls[0].Function.Arguments != `{"symbol":"PAX"}` {
		t.Errorf("tool call args = %q, want %q", m.ToolCalls[0].Function.Arguments, `{"symbol":"PAX"}`)
	}
	if strings.Contains(m.Content, "<function_call") {
		t.Errorf("content leaked raw markup: %q", m.Content)
	}
	if m.Content != "On it." {
		t.Errorf("content = %q, want %q", m.Content, "On it.")
	}
}

// TestFromWireRespMessageStructuredCallsWin verifies the extractor does NOT run
// when structured tool_calls are already present: the raw content (including any
// stray `<function_call>` markup) is preserved verbatim and only the structured
// call is returned.
func TestFromWireRespMessageStructuredCallsWin(t *testing.T) {
	structured := ToolCall{
		ID:       "call_real",
		Type:     "function",
		Function: FunctionCall{Name: "real_tool", Arguments: `{"ok":true}`},
	}
	rawContent := `Answer. <function_call name="ghost">{"x":1}</function_call>`
	m := fromWireRespMessage(wireRespMessage{
		Role:      "assistant",
		Content:   rawContent,
		ToolCalls: []ToolCall{structured},
	})
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "real_tool" {
		t.Fatalf("tool calls = %+v, want only the structured real_tool", m.ToolCalls)
	}
	if !strings.Contains(m.Content, "<function_call") {
		t.Errorf("content = %q, want raw markup preserved (extractor must not run)", m.Content)
	}
}
