// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"strings"
	"testing"
)

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

// TestAggregateStreamReaderDeltas verifies the live-streaming path: reasoning
// and visible-content fragments are delivered incrementally via onDelta as the
// SSE frames arrive, the concatenated deltas reconstruct exactly the folded
// turn's reasoning + content, and a final flush leaves nothing buffered.
func TestAggregateStreamReaderDeltas(t *testing.T) {
	stream := "" +
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"The user wants the block "}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"height; I will read it live."}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"The current block "}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"height is 12,345,678."}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n" +
		`data: [DONE]` + "\n"

	var content, reasoning strings.Builder
	onDelta := func(d Delta) {
		content.WriteString(d.Content)
		reasoning.WriteString(d.Reasoning)
	}
	msg, finish, _, err := aggregateStreamReader(strings.NewReader(stream), onDelta)
	if err != nil {
		t.Fatalf("aggregateStreamReader: %v", err)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	wantContent := "The current block height is 12,345,678."
	wantReasoning := "The user wants the block height; I will read it live."
	if msg.Content != wantContent {
		t.Errorf("folded content = %q, want %q", msg.Content, wantContent)
	}
	if content.String() != wantContent {
		t.Errorf("streamed content = %q, want %q", content.String(), wantContent)
	}
	if reasoning.String() != wantReasoning {
		t.Errorf("streamed reasoning = %q, want %q", reasoning.String(), wantReasoning)
	}
}

// TestAggregateStreamReaderHoldsBackToolGrammar verifies the visible-content
// stream never leaks the Kimi tool-call token grammar: the streamed fragments
// stop at the section marker, and the folded turn extracts the structured call
// (so a consumer renders clean prose while the call still executes).
func TestAggregateStreamReaderHoldsBackToolGrammar(t *testing.T) {
	// The visible preamble, then the Kimi token-grammar tool call inlined in
	// content, split across frames so the marker arrives partially first.
	stream := "" +
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check that. "}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"<|tool_calls_section_begin|><|tool_call_begin|>functions.chain_info:0"}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"<|tool_call_argument_begin|>{}<|tool_call_end|><|tool_calls_section_end|>"}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		`data: [DONE]` + "\n"

	var content strings.Builder
	onDelta := func(d Delta) { content.WriteString(d.Content) }
	msg, _, _, err := aggregateStreamReader(strings.NewReader(stream), onDelta)
	if err != nil {
		t.Fatalf("aggregateStreamReader: %v", err)
	}
	if got := content.String(); strings.Contains(got, "<|tool_call") || strings.Contains(got, "functions.") {
		t.Errorf("streamed content leaked tool grammar: %q", got)
	}
	if got := strings.TrimSpace(content.String()); got != "Let me check that." {
		t.Errorf("streamed visible content = %q, want %q", got, "Let me check that.")
	}
	// The structured call + clean content are recovered by the authoritative
	// fold (fromWireRespMessage), the same normalization Chat applies.
	final := fromWireRespMessage(msg)
	if len(final.ToolCalls) != 1 || final.ToolCalls[0].Function.Name != "chain_info" {
		t.Fatalf("folded tool calls = %+v, want one chain_info", final.ToolCalls)
	}
	if strings.Contains(final.Content, "<|tool_call") {
		t.Errorf("folded content leaked tool grammar: %q", final.Content)
	}
}
