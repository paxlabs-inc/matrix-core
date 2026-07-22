// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestExtractTagToolCalls pins the MiMo/Qwen tag-grammar fallback: models on
// this stack sometimes emit tool calls as inline tag text
// (`<tool_call> <function=NAME> <parameter=key>value …`) inside content
// instead of the structured tool_calls array. Left unparsed, the call leaked
// into the chat as a "final answer" and the intended action never ran (the
// 2026-07-22 loopty-loop incident: an intended call judged as a bare answer by
// the close chain). extractTagToolCalls pulls those spans out, builds a JSON
// argument object, strips the markup from the visible content, and tolerates
// every closing tag being absent (truncated generation).
func TestExtractTagToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantContent string
		wantCalls   []struct {
			name string
			args map[string]interface{}
		}
	}{
		{
			// The exact emission shape observed in the incident transcript:
			// spaces for newlines, no </parameter> or </function> closers.
			name: "incident shape: unclosed parameters, tool_call closer only",
			content: `All search engines are CAPTCHA-blocking the browser. Let me switch to curl.` +
				`<tool_call> <function=exec__shell> <parameter=command>curl -s "https://api.github.com/repos/langchain-ai/langchain/releases?per_page=3" | head -100 <parameter=expect>JSON array of LangChain's 3 most recent GitHub releases </tool_call>`,
			wantContent: "All search engines are CAPTCHA-blocking the browser. Let me switch to curl.",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{{
				name: "exec__shell",
				args: map[string]interface{}{
					"command": `curl -s "https://api.github.com/repos/langchain-ai/langchain/releases?per_page=3" | head -100`,
					"expect":  "JSON array of LangChain's 3 most recent GitHub releases",
				},
			}},
		},
		{
			name: "canonical shape: newlines and full closers",
			content: "<tool_call>\n<function=read_overflow>\n<parameter=token>\novf-1\n</parameter>\n<parameter=offset>\n8000\n</parameter>\n</function>\n</tool_call>",
			wantContent: "",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{{
				name: "read_overflow",
				args: map[string]interface{}{"token": "ovf-1", "offset": float64(8000)},
			}},
		},
		{
			name:        "bare function header without tool_call wrapper",
			content:     `<function=fs__list_directory> <parameter=path>/workspace</parameter></function> trailing prose`,
			wantContent: "trailing prose",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{{
				name: "fs__list_directory",
				args: map[string]interface{}{"path": "/workspace"},
			}},
		},
		{
			name:        "truncated mid-value consumes remainder without leaking",
			content:     `Working on it. <tool_call> <function=exec__shell> <parameter=command>echo hi`,
			wantContent: "Working on it.",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{{
				name: "exec__shell",
				args: map[string]interface{}{"command": "echo hi"},
			}},
		},
		{
			name: "two calls in one message",
			content: `<tool_call><function=a><parameter=x>1</parameter></function></tool_call>` +
				`<tool_call><function=b><parameter=y>two</parameter></function></tool_call>`,
			wantContent: "",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{
				{name: "a", args: map[string]interface{}{"x": float64(1)}},
				{name: "b", args: map[string]interface{}{"y": "two"}},
			},
		},
		{
			name:        "shell redirection and comparison operators survive in values",
			content:     `<tool_call> <function=exec__shell> <parameter=command>grep -c x f.txt > /tmp/n && test $(cat /tmp/n) -gt 0 </tool_call>`,
			wantContent: "",
			wantCalls: []struct {
				name string
				args map[string]interface{}
			}{{
				name: "exec__shell",
				args: map[string]interface{}{"command": "grep -c x f.txt > /tmp/n && test $(cat /tmp/n) -gt 0"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotContent, gotCalls := extractTagToolCalls(tt.content)
			if gotContent != tt.wantContent {
				t.Errorf("content = %q, want %q", gotContent, tt.wantContent)
			}
			if strings.Contains(gotContent, "<tool_call") || strings.Contains(gotContent, "<function=") {
				t.Errorf("tag markup leaked into content: %q", gotContent)
			}
			if len(gotCalls) != len(tt.wantCalls) {
				t.Fatalf("got %d calls, want %d: %+v", len(gotCalls), len(tt.wantCalls), gotCalls)
			}
			for i, want := range tt.wantCalls {
				if gotCalls[i].Function.Name != want.name {
					t.Errorf("call[%d].name = %q, want %q", i, gotCalls[i].Function.Name, want.name)
				}
				var gotArgs map[string]interface{}
				if err := json.Unmarshal([]byte(gotCalls[i].Function.Arguments), &gotArgs); err != nil {
					t.Fatalf("call[%d] arguments are not valid JSON: %q (%v)", i, gotCalls[i].Function.Arguments, err)
				}
				if !reflect.DeepEqual(gotArgs, want.args) {
					t.Errorf("call[%d].args = %#v, want %#v", i, gotArgs, want.args)
				}
			}
		})
	}
}

// TestFromWireRespMessage_DuplicateTagMarkupStripped pins the double-emission
// case observed live 2026-07-22: MiMo returned provider-parsed structured
// tool_calls AND re-emitted the same call as tag text inside content. The fold
// must keep the authoritative structured calls and strip the duplicated markup
// from content — before this fix the markup leaked into user-facing narration.
func TestFromWireRespMessage_DuplicateTagMarkupStripped(t *testing.T) {
	m := wireRespMessage{
		Role: "assistant",
		Content: "Let me check the releases.<tool_call>\n<function=exec__shell>\n<parameter=command>\ncurl -sL https://example.test\n</parameter>\n</function>\n</tool_call>",
		ToolCalls: []ToolCall{{
			ID:       "call_fb3142a8ef2b4ab28ccd7e98",
			Type:     "function",
			Function: FunctionCall{Name: "exec__shell", Arguments: `{"command":"curl -sL https://example.test"}`},
		}},
	}
	got := fromWireRespMessage(m)
	if got.Content != "Let me check the releases." {
		t.Errorf("content = %q, want the narration with the tag markup stripped", got.Content)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call_fb3142a8ef2b4ab28ccd7e98" {
		t.Errorf("structured calls must stay authoritative, got %+v", got.ToolCalls)
	}
}

// TestExtractTagToolCalls_NoFalsePositives proves ordinary prose — including
// prose that mentions angle brackets or HTML — is returned untouched.
func TestExtractTagToolCalls_NoFalsePositives(t *testing.T) {
	for _, content := range []string{
		"a plain final answer",
		"use <b>bold</b> and <i>italics</i> in the page",
		"the operator < compares and > redirects",
		"",
	} {
		got, calls := extractTagToolCalls(content)
		if got != content || calls != nil {
			t.Errorf("extractTagToolCalls(%q) = (%q, %v), want unchanged and nil", content, got, calls)
		}
	}
}
