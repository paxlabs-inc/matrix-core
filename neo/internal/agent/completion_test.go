// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// completeCall builds a task_complete tool call with the given JSON args.
func completeCall(args string) *llm.ToolCall {
	c := tc(tools.TaskCompleteTool, args)
	return &c
}

func TestSplitCompletionSeparatesGate(t *testing.T) {
	calls := []llm.ToolCall{
		tc("read", `{"p":"/a"}`),
		tc(tools.TaskCompleteTool, `{"summary":"done","coverage":"full"}`),
		tc("write", `{"p":"/b"}`),
	}
	cc, rest := splitCompletion(calls)
	if cc == nil || cc.Function.Name != tools.TaskCompleteTool {
		t.Fatalf("expected task_complete extracted, got %+v", cc)
	}
	if len(rest) != 2 {
		t.Fatalf("expected 2 sibling calls, got %d", len(rest))
	}
	for _, r := range rest {
		if r.Function.Name == tools.TaskCompleteTool {
			t.Error("task_complete must not remain in rest")
		}
	}

	// No completion in the batch → (nil, all).
	cc2, rest2 := splitCompletion([]llm.ToolCall{tc("read", "{}")})
	if cc2 != nil || len(rest2) != 1 {
		t.Errorf("non-completion batch should pass through: cc=%v rest=%d", cc2, len(rest2))
	}
}

func TestBatchTouchesState(t *testing.T) {
	reversible := []llm.ToolCall{tc("shell", "{}"), tc("read", "{}")}
	if batchTouchesState(reversible) {
		t.Error("reversible tools must not be state-touching")
	}
	stateful := []llm.ToolCall{tc("read", "{}"), tc(tools.CoreExecuteTool, `{"intent":"pay"}`)}
	if !batchTouchesState(stateful) {
		t.Error("core_execute must be state-touching")
	}
}

func TestValidateCompletionStructural(t *testing.T) {
	a := New(Options{Config: config.Default()})

	// Empty summary rejects.
	if v := a.validateCompletion(context.Background(), completeCall(`{"summary":"  ","coverage":"full"}`), false, false, ""); v.ok {
		t.Error("empty summary must reject")
	}
	// Missing / bad coverage rejects.
	if v := a.validateCompletion(context.Background(), completeCall(`{"summary":"hi"}`), false, false, ""); v.ok {
		t.Error("missing coverage must reject")
	}
	if v := a.validateCompletion(context.Background(), completeCall(`{"summary":"hi","coverage":"mostly"}`), false, false, ""); v.ok {
		t.Error("invalid coverage must reject")
	}
}

func TestValidateCompletionCoherenceGuardG1(t *testing.T) {
	a := New(Options{Config: config.Default()})
	// coverage=full with a non-empty open_gaps list is incoherent → reject.
	v := a.validateCompletion(context.Background(), completeCall(`{"summary":"all done","coverage":"full","open_gaps":["the API key is still unset"]}`), false, false, "")
	if v.ok {
		t.Error("coverage=full with open gaps must reject (g1)")
	}
	// Same content as partial is honest → accept.
	if v := a.validateCompletion(context.Background(), completeCall(`{"summary":"got most of it","coverage":"partial","open_gaps":["the API key is still unset"]}`), false, false, ""); !v.ok {
		t.Errorf("honest partial must accept, got reject: %s", v.feedback)
	}
}

func TestValidateCompletionLightPath(t *testing.T) {
	a := New(Options{Config: config.Default()})
	// Reversible turn (stateTouched=false): a well-formed object accepts even
	// with no evidence — the light path carries no false-alarm tax.
	v := a.validateCompletion(context.Background(), completeCall(`{"summary":"Hello! How can I help?","coverage":"full"}`), false, false, "")
	if !v.ok || v.answer != "Hello! How can I help?" {
		t.Errorf("light path must accept and carry the summary, got ok=%v answer=%q", v.ok, v.answer)
	}
}

func TestValidateCompletionStrictRequiresEvidence(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("what's the block height?"),
		llm.ToolResult("chain", "chain_info", `{"blockNumber":20531991,"chainId":125}`),
	}
	// State-touching turn with NO cited evidence → reject (positive proof).
	if v := a.validateCompletion(context.Background(), completeCall(`{"summary":"The block height is 20531991.","coverage":"full"}`), true, true, ""); v.ok {
		t.Error("state-touching completion with no evidence must reject")
	}
}

func TestValidateCompletionStrictRejectsPhantomEvidence(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("what's the block height?"),
		llm.ToolResult("chain", "chain_info", `{"blockNumber":20531991,"chainId":125}`),
	}
	// The classic failure: claim a fact (123456) that NO tool result supports.
	// The cited evidence is phantom → reject (g3-analog: no phantom evidence).
	v := a.validateCompletion(context.Background(), completeCall(`{"summary":"The block height is 123456.","coverage":"full","evidence":["chain_info returned blockNumber 123456"]}`), true, true, "")
	if v.ok {
		t.Error("phantom evidence (123456 absent from transcript) must reject")
	}
}

func TestValidateCompletionStrictAcceptsGroundedEvidence(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("what's the block height?"),
		llm.ToolResult("chain", "chain_info", `{"blockNumber":20531991,"chainId":125}`),
	}
	// Evidence whose salient token (the real block number) appears in the
	// transcript is grounded → accept.
	v := a.validateCompletion(context.Background(), completeCall(`{"summary":"The block height is 20531991.","coverage":"full","evidence":["chain_info returned blockNumber 20531991"]}`), true, true, "")
	if !v.ok {
		t.Errorf("grounded evidence must accept, got reject: %s", v.feedback)
	}
}

func TestStringSliceShapes(t *testing.T) {
	got := stringSlice([]interface{}{"a", "  ", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("stringSlice should clean+drop empties, got %v", got)
	}
	if s := stringSlice("single"); len(s) != 1 || s[0] != "single" {
		t.Errorf("stringSlice should wrap a bare string, got %v", s)
	}
	if stringSlice(nil) != nil {
		t.Error("nil → nil")
	}
}

func TestSalientTokens(t *testing.T) {
	got := salientTokens("blockNumber 20531991 0xAbCd")
	want := map[string]bool{"blocknumber": true, "20531991": true, "0xabcd": true}
	for _, g := range got {
		delete(want, g)
	}
	if len(want) != 0 {
		t.Errorf("salientTokens missing %v (got %v)", want, got)
	}
	// Short/punctuation-only yields nothing.
	if len(salientTokens("a, b! c?")) != 0 {
		t.Error("no token >=4 chars should yield empty")
	}
}

func TestFirstUngrounded(t *testing.T) {
	corpus := `{"blocknumber":20531991,"chainid":125}`
	if firstUngrounded([]string{"blockNumber 20531991"}, corpus) != "" {
		t.Error("grounded token should pass")
	}
	if firstUngrounded([]string{"value 99999999"}, corpus) == "" {
		t.Error("ungrounded token must be flagged")
	}
}
