// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// TestCloseGuardChain_OrderIsTheDocumentedTable pins the chain's order as a
// proven artifact (MORPHEUS req.4.2): the table below IS the documented
// gauntlet order, and any reorder or insertion must consciously update this
// test alongside the chain.
func TestCloseGuardChain_OrderIsTheDocumentedTable(t *testing.T) {
	want := []string{
		"inbox_drain",
		"truncated_answer",
		"empty_answer",
		"unread_overflow",
		"source_fetch",
		"identity_leak",
		"deliver",
	}
	if len(closeGuardChain) != len(want) {
		t.Fatalf("chain has %d guards, want %d", len(closeGuardChain), len(want))
	}
	for i, g := range closeGuardChain {
		if g.name != want[i] {
			t.Errorf("chain[%d] = %q, want %q", i, g.name, want[i])
		}
		if g.eval == nil {
			t.Errorf("chain[%d] %q has no eval func", i, g.name)
		}
	}
}

// bareResult builds a bare-answer model result (no tool calls) for driving the
// chain the same way closeTurn does.
func bareResult(content, finishReason string) *llm.ChatResult {
	return &llm.ChatResult{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: content},
		FinishReason: finishReason,
	}
}

// chainAgent builds a real Agent (real config, real turn) for direct chain
// evaluation. No model/tools are needed: the chain reads only agent + turn
// state.
func chainAgent(t *testing.T, mutate func(cfg *config.Config), inbox func() []string) *Agent {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	return New(Options{Config: cfg, Inbox: inbox})
}

// unreadOverflow arms the read-full latch with a real stored overflow payload.
func unreadOverflow(t *testing.T, a *Agent) {
	t.Helper()
	a.turn.overflow = &overflowStore{}
	if _, _, ok := a.turn.overflow.put("an oversized tool result the model has not read"); !ok {
		t.Fatal("overflow put failed")
	}
	t.Cleanup(a.turn.overflow.cleanup)
}

// TestCloseGuardChain_FirstFiringGuardDecides proves the ORDER on stacked
// conditions (req.4.2): when several guards' conditions hold simultaneously,
// the earliest guard in the table decides — peeling one condition per case
// walks the decision down the chain.
func TestCloseGuardChain_FirstFiringGuardDecides(t *testing.T) {
	leakAnswer := "I'm Grok, and here is the result."
	inbox := func() []string { return []string{"a mid-task note from the user"} }

	cases := []struct {
		name        string
		inbox       func() []string
		finish      string
		answer      string
		overflow    bool
		sourceGap   bool
		wantGuard   string
		wantVerdict closeVerdict
	}{
		{
			name:  "inbox outranks everything",
			inbox: inbox, finish: "length", answer: leakAnswer, overflow: true,
			wantGuard: "inbox_drain", wantVerdict: verdictSuppress,
		},
		{
			name:   "truncation outranks overflow, identity, cassandra",
			finish: "length", answer: leakAnswer, overflow: true,
			wantGuard: "truncated_answer", wantVerdict: verdictNudge,
		},
		{
			name:   "empty answer outranks overflow, cassandra",
			finish: "stop", answer: "", overflow: true,
			wantGuard: "empty_answer", wantVerdict: verdictNudge,
		},
		{
			name:   "unread overflow outranks identity, cassandra",
			finish: "stop", answer: leakAnswer, overflow: true,
			wantGuard: "unread_overflow", wantVerdict: verdictNudge,
		},
		{
			name:   "source fetch outranks identity and cassandra",
			finish: "stop", answer: leakAnswer, sourceGap: true,
			wantGuard: "source_fetch", wantVerdict: verdictNudge,
		},
		{
			name:   "identity leak outranks cassandra",
			finish: "stop", answer: leakAnswer,
			wantGuard: "identity_leak", wantVerdict: verdictNudge,
		},
		{
			name:   "cassandra observations do not veto a clean close",
			finish: "stop", answer: "here is the verified result",
			wantGuard: "deliver", wantVerdict: verdictDeliver,
		},
		{
			name:   "nothing blocking delivers",
			finish: "stop", answer: "here is the verified result",
			wantGuard: "deliver", wantVerdict: verdictDeliver,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := chainAgent(t, nil, tc.inbox)
			if tc.overflow {
				unreadOverflow(t, a)
			}
			if tc.sourceGap {
				a.turn.webSourceURLs = map[string]struct{}{"https://example.test/source": {}}
			}
			cc := &closeContext{res: bareResult(tc.answer, tc.finish), answer: strings.TrimSpace(tc.answer)}
			name, dec := a.evalCloseChain(cc)
			if name != tc.wantGuard {
				t.Fatalf("fired guard = %q, want %q", name, tc.wantGuard)
			}
			if dec.verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", dec.verdict, tc.wantVerdict)
			}
			if dec.err != nil {
				t.Fatalf("no escalation expected below the cap, got err: %v", dec.err)
			}
			// A nudge verdict must have pushed exactly one guidance message;
			// suppress/deliver must not.
			nudges := 0
			for _, m := range a.working {
				if m.IsGuidance() {
					nudges++
				}
			}
			switch tc.wantVerdict {
			case verdictNudge:
				if nudges != 1 {
					t.Fatalf("a nudge verdict must push exactly one guidance message, got %d", nudges)
				}
			default:
				if nudges != 0 {
					t.Fatalf("verdict %q must push no guidance, got %d", tc.wantVerdict, nudges)
				}
			}
			// The identity guard rewrites the delivered answer in place.
			if name == "identity_leak" || (tc.answer == leakAnswer && tc.wantGuard == "identity_leak") {
				if strings.Contains(cc.answer, "Grok") || !strings.Contains(cc.answer, a.agentName()) {
					t.Fatalf("identity guard must scrub the answer in place, got %q", cc.answer)
				}
			}
		})
	}
}

// TestCloseGuardChain_IdentityCapDeliversScrubbed pins the identity guard's
// distinct past-the-cap behavior: unlike the other nudging guards (which
// escalate to an honest stop-and-ask), a persistent identity leak DELIVERS the
// scrubbed answer once the nudge budget is spent — the user gets the honest,
// contained answer rather than a dead turn.
func TestCloseGuardChain_IdentityCapDeliversScrubbed(t *testing.T) {
	a := chainAgent(t, func(cfg *config.Config) { cfg.MaxGuidanceNudges = 1 }, nil)
	a.turn.identityNudges = 1 // this guard's budget is already spent
	cc := &closeContext{res: bareResult("I'm Grok, done.", "stop"), answer: "I'm Grok, done."}
	name, dec := a.evalCloseChain(cc)
	if name != "identity_leak" {
		t.Fatalf("fired guard = %q, want identity_leak", name)
	}
	if dec.verdict != verdictDeliver || dec.err != nil {
		t.Fatalf("past the cap the identity guard must deliver without escalation, got verdict=%q err=%v", dec.verdict, dec.err)
	}
	if strings.Contains(cc.answer, "Grok") {
		t.Fatalf("the delivered answer must be scrubbed, got %q", cc.answer)
	}
}

// TestCloseGuardChain_EscalationIsTerminal pins the unproductive-cap
// escalation through the chain: a nudging guard whose push exceeds the unified
// bound returns a terminal ErrIncomplete decision (escalateGuidance), and the
// death record carries the unproductive-cap reason.
func TestCloseGuardChain_EscalationIsTerminal(t *testing.T) {
	a := chainAgent(t, func(cfg *config.Config) { cfg.MaxGuidanceNudges = 1 }, nil)
	a.turn.emptyAnswerNudges = 1
	cc := &closeContext{res: bareResult("", "stop"), answer: ""}
	name, dec := a.evalCloseChain(cc)
	if name != "empty_answer" {
		t.Fatalf("fired guard = %q, want empty_answer", name)
	}
	if dec.err == nil || !errors.Is(dec.err, ErrIncomplete) {
		t.Fatalf("past the cap the guard must escalate with ErrIncomplete, got %v", dec.err)
	}
	death, ok := a.LastDeath()
	if !ok || death.Reason != DeathReasonUnproductive {
		t.Fatalf("escalation must record the unproductive-cap death, got ok=%v reason=%q", ok, death.Reason)
	}
}

func TestTruncatedAnswerHasOneIndependentSynthesisRetry(t *testing.T) {
	a := chainAgent(t, func(cfg *config.Config) { cfg.MaxGuidanceNudges = 1 }, nil)
	a.turn.repeats = a.cfg.NoProgressStall - 1
	a.turn.emptyAnswerNudges = a.cfg.MaxGuidanceNudges
	a.turn.identityNudges = a.cfg.MaxGuidanceNudges

	first := &closeContext{res: bareResult("partial synthesis", "length"), answer: "partial synthesis"}
	name, dec := a.evalCloseChain(first)
	if name != "truncated_answer" || dec.verdict != verdictNudge || dec.err != nil {
		t.Fatalf("first truncation = guard %q verdict %q err %v", name, dec.verdict, dec.err)
	}
	if a.turn.finalSynthesisRetries != 1 {
		t.Fatalf("synthesis retries = %d, want 1", a.turn.finalSynthesisRetries)
	}

	full := &closeContext{res: bareResult("the complete verified answer", "stop"), answer: "the complete verified answer"}
	name, dec = a.evalCloseChain(full)
	if name != "deliver" || dec.verdict != verdictDeliver || dec.err != nil {
		t.Fatalf("full synthesis after retry = guard %q verdict %q err %v", name, dec.verdict, dec.err)
	}
}

func TestTruncatedSynthesisRetriesThenDeliversThroughAgentLoop(t *testing.T) {
	var calls int
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"partial internal synthesis"},"finish_reason":"length"}]}`+"\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"the complete verified answer"},"finish_reason":"stop"}]}`+"\n")
		}
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	reporter := &captureReporter{}
	client := revisionTestClient(t, model.URL)
	cfg := config.Default()
	cfg.CassandraEnabled = true
	a := New(Options{Config: cfg, Main: client, Cheap: client, Reporter: reporter})
	if err := a.Chat(context.Background(), "finish the verified work and report the result"); err != nil {
		t.Fatalf("agent loop: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want one truncated call plus one synthesis retry", calls)
	}
	if len(reporter.said) != 1 || reporter.said[0] != "the complete verified answer" {
		t.Fatalf("delivered answers = %v", reporter.said)
	}
}

// recordingConsolidator counts real invocations of the loop's write-back seam
// (the Consolidator observer): it fabricates nothing and gates nothing — it
// measures how many times the REAL terminal paths handed the transcript over.
type recordingConsolidator struct {
	mu sync.Mutex
	n  int
}

func (c *recordingConsolidator) Consolidate(consolidation.Job) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *recordingConsolidator) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// sseAnswerServer scripts a model that immediately delivers a bare final
// answer (the clean-delivery terminal path).
func sseAnswerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"all done here"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// distinctToolServer scripts a model that emits a DIFFERENT tool name every
// step — genuine breadth, so the no-progress stall never trips and the turn
// runs to the step budget.
func distinctToolServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		tc := fmt.Sprintf(`{"index":0,"id":"call_%d","type":"function","function":{"name":"probe_%d","arguments":"{}"}}`, idx, idx)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingModelServer always returns HTTP 500 — the model-error terminal path.
func failingModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestConsolidationOncePerTerminalPath proves req.4.3 on the real loop: every
// terminal path — clean delivery, no-progress stall, step-budget exhaustion,
// and a terminal model error — hands the transcript to consolidation exactly
// ONCE. Neither zero (a turn whose learnings are lost) nor twice (duplicate
// write-back pressure).
func TestConsolidationOncePerTerminalPath(t *testing.T) {
	newLoopAgent := func(t *testing.T, url string, rec *recordingConsolidator, mutate func(cfg *config.Config)) *Agent {
		t.Helper()
		cfg := config.Default()
		cfg.CassandraEnabled = false
		if mutate != nil {
			mutate(&cfg)
		}
		return New(Options{
			Config:       cfg,
			Main:         revisionTestClient(t, url),
			Tools:        &tools.Manager{},
			Consolidator: rec,
		})
	}

	t.Run("delivery", func(t *testing.T) {
		rec := &recordingConsolidator{}
		a := newLoopAgent(t, sseAnswerServer(t).URL, rec, nil)
		if err := a.Chat(context.Background(), "say hi"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if got := rec.count(); got != 1 {
			t.Fatalf("delivery path consolidated %d times, want exactly 1", got)
		}
	})

	t.Run("stall", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		rec := &recordingConsolidator{}
		a := newLoopAgent(t, spiralServer(t, &calls, &mu).URL, rec, nil)
		err := a.Chat(context.Background(), "keep searching for the same query")
		if err == nil || !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "repeating the same step") {
			t.Fatalf("the spiral must die on the stall, got: %v", err)
		}
		if got := rec.count(); got != 1 {
			t.Fatalf("stall path consolidated %d times, want exactly 1", got)
		}
	})

	t.Run("step_budget", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		rec := &recordingConsolidator{}
		a := newLoopAgent(t, distinctToolServer(t, &calls, &mu).URL, rec, func(cfg *config.Config) {
			cfg.StepBudget = 2
			cfg.StepBudgetMax = 2
		})
		err := a.Chat(context.Background(), "probe everything")
		if err == nil || !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "step budget") {
			t.Fatalf("distinct-tool breadth must exhaust the step budget, got: %v", err)
		}
		if got := rec.count(); got != 1 {
			t.Fatalf("step-budget path consolidated %d times, want exactly 1", got)
		}
	})

	t.Run("model_error", func(t *testing.T) {
		rec := &recordingConsolidator{}
		a := newLoopAgent(t, failingModelServer(t).URL, rec, nil)
		err := a.Chat(context.Background(), "do anything")
		if err == nil || errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "model call failed") {
			t.Fatalf("a dead model must be a terminal model error, got: %v", err)
		}
		if got := rec.count(); got != 1 {
			t.Fatalf("model-error path consolidated %d times, want exactly 1", got)
		}
	})
}
