// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import "testing"

// TestAggregateStreamToolCalls verifies that an OpenAI-style SSE stream folds
// back into one assistant turn: content concatenates, a tool_call whose
// arguments arrive across several frames is merged by index (id + name seen
// once), the terminal finish_reason wins, and the final usage chunk is read.
func TestAggregateStreamToolCalls(t *testing.T) {
	stream := "" +
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Look"}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"ing."}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"web_search","arguments":"{\"q\":"}}]}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"matrix\"}"}}]}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n" +
		`data: [DONE]` + "\n"

	msg, finish, usage, err := aggregateStream([]byte(stream))
	if err != nil {
		t.Fatalf("aggregateStream: %v", err)
	}
	if msg.Content != "Looking." {
		t.Errorf("content = %q, want %q", msg.Content, "Looking.")
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_a" || tc.Function.Name != "web_search" {
		t.Errorf("tool call id/name = %q/%q", tc.ID, tc.Function.Name)
	}
	if tc.Function.Arguments != `{"q":"matrix"}` {
		t.Errorf("merged arguments = %q, want %q", tc.Function.Arguments, `{"q":"matrix"}`)
	}
	if usage == nil || usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want total 18", usage)
	}
}

// TestAggregateStreamErrorFrame surfaces an error frame mid-stream rather than
// silently returning a half-built turn.
func TestAggregateStreamErrorFrame(t *testing.T) {
	stream := `data: {"error":{"message":"This model only supports streaming.","type":"invalid_request_error"}}` + "\n"
	if _, _, _, err := aggregateStream([]byte(stream)); err == nil {
		t.Fatal("expected error from error frame, got nil")
	}
}

// TestAggregateStreamEmpty rejects a body with no data frames.
func TestAggregateStreamEmpty(t *testing.T) {
	if _, _, _, err := aggregateStream([]byte("\n: keepalive\n")); err == nil {
		t.Fatal("expected empty-stream error, got nil")
	}
}
