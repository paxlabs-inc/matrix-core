// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"encoding/json"
	"testing"
)

// A truncated/malformed generation can leave an assistant tool_call whose
// function.arguments is empty or non-JSON. Because the transcript is re-sent
// verbatim every turn, replaying that verbatim makes a strict provider 400 the
// whole request and wedge the conversation permanently. sanitizeToolCalls must
// coerce such arguments to "{}" while leaving well-formed calls untouched.
func TestSanitizeToolCallsCoercesMalformedArguments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "{}"},
		{"whitespace", "   ", "{}"},
		{"truncated_json", `{"path":"/index.html","content":"<htm`, "{}"},
		{"non_json", "not json at all", "{}"},
		{"valid_object", `{"path":"/x"}`, `{"path":"/x"}`},
		{"valid_empty_object", `{}`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeToolCalls([]ToolCall{
				{ID: "c1", Function: FunctionCall{Name: "fs_write", Arguments: tc.in}},
			})
			if len(got) != 1 {
				t.Fatalf("expected 1 call, got %d", len(got))
			}
			if got[0].Function.Arguments != tc.want {
				t.Errorf("arguments = %q, want %q", got[0].Function.Arguments, tc.want)
			}
			if got[0].Type != "function" {
				t.Errorf("type = %q, want \"function\"", got[0].Type)
			}
		})
	}
}

// Regression for the pitchdeck wedge: a malformed assistant tool_call must
// never reach the wire with invalid-JSON arguments, regardless of where it
// sits in the transcript.
func TestToWireMessagesNeverEmitsInvalidToolArguments(t *testing.T) {
	msgs := []Message{
		UserMessage("build the site"),
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "fs_write", Arguments: `{"path":"/index.html","content":"<htm`}},
		}},
		ToolResult("c1", "fs_write", "could not parse arguments (unexpected end of JSON input)."),
	}
	for _, w := range toWireMessages(msgs, false) {
		for _, tc := range w.ToolCalls {
			if !json.Valid([]byte(tc.Function.Arguments)) {
				t.Fatalf("replayed tool_call arguments not valid JSON: %q", tc.Function.Arguments)
			}
		}
	}
}
