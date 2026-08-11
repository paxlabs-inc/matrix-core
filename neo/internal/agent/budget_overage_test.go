// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// budget_overage_test.go pins the 2026-07-25 research-task incident: a COMPLETE
// long answer must reach the user even when the generation exceeds the O1
// representation budget. The regression it guards is exact — the main
// conversational client is built with an 8192-token request cap while
// o1.DefaultBudget() capped completion tokens at 4096, so a finished
// 4098-token report was converted into "model call failed: o1 provider
// protocol" by chatWithRetry, killing the run. The supervisor then respawned
// the agent seeded with that death reason, and the successor stopped trying to
// answer at all.
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

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/o1"
	"matrix/neo/internal/tools"
)

// deliveryReporter captures what the user actually received on the substantive
// channel. Say(completion=false) is the conversational answer the user reads.
type deliveryReporter struct {
	mu        sync.Mutex
	delivered []string
}

func (r *deliveryReporter) Say(text string, _ bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivered = append(r.delivered, text)
}
func (r *deliveryReporter) Status(string)             {}
func (r *deliveryReporter) Progress(string)           {}
func (r *deliveryReporter) Notice(string)             {}
func (r *deliveryReporter) Think(string)              {}
func (r *deliveryReporter) Delta(int, string, string) {}

func (r *deliveryReporter) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.delivered, "\n")
}

// TestOversizedAnswerIsDelivered drives the REAL agent loop against a real
// llm.Client and a real OpenAI-style SSE endpoint that returns a long final
// answer reporting MORE completion tokens than the o1 budget used to allow.
// The answer must reach the user instead of the turn dying.
func TestOversizedAnswerIsDelivered(t *testing.T) {
	// A genuine long-form synthesis, the shape that triggered the incident.
	report := strings.Repeat("The agent landscape splits into three tiers. ", 400)
	if len(report) < 16000 {
		t.Fatalf("precondition: the report must be long-form, got %d bytes", len(report))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// finish_reason=stop: the model COMPLETED the answer. The only thing
		// out of bounds is its reported size — exactly the 4098-vs-4096 case.
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":900,\"completion_tokens\":4098,\"total_tokens\":4998}}\n", report)
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-budget-overage"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	out := &deliveryReporter{}
	a := New(Options{
		Config:   cfg,
		Main:     client,
		Tools:    &tools.Manager{},
		Pager:    pager,
		ConvID:   "conv-budget-overage",
		Reporter: out,
	})

	if err := a.Chat(context.Background(), "research the agent landscape and report the full analysis"); err != nil {
		t.Fatalf("Chat returned an error for a COMPLETE answer that merely exceeded the representation budget: %v", err)
	}

	got := out.joined()
	if got == "" {
		t.Fatal("the finished answer was never delivered to the user")
	}
	if !strings.Contains(got, "The agent landscape splits into three tiers.") {
		t.Fatalf("delivered text is not the model's answer: %.200q", got)
	}
}

// TestChatWithRetryKeepsOversizedGeneration isolates the seam: chatWithRetry
// must hand back a usable result on a pure SIZE verdict, and still hard-fail on
// a genuine protocol violation. Conflating the two is what discarded finished
// work.
func TestChatWithRetryKeepsOversizedGeneration(t *testing.T) {
	cases := []struct {
		name     string
		sse      string
		wantErr  bool
		wantText string
	}{
		{
			name:     "oversized but complete generation survives",
			sse:      `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"finished analysis"},"finish_reason":"stop"}],"usage":{"completion_tokens":900000,"total_tokens":900000}}`,
			wantErr:  false,
			wantText: "finished analysis",
		},
		{
			name:    "reasoning-only generation is still a protocol failure",
			sse:     `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"completion_tokens":10,"total_tokens":10}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.sse+"\n")
				fmt.Fprint(w, "data: [DONE]\n")
			}))
			t.Cleanup(srv.Close)

			client, err := llm.New(mcllm.Config{
				Model:       "accounts/fireworks/models/gpt-oss-120b",
				Provider:    mcllm.ProviderFireworks,
				ProviderSet: true,
				GatewayURL:  srv.URL,
			})
			if err != nil {
				t.Fatalf("llm.New: %v", err)
			}
			a := New(Options{Config: config.Default(), Main: client, Tools: &tools.Manager{}})

			res, err := a.chatWithRetry(context.Background(), llm.ChatRequest{
				Messages: []llm.Message{llm.UserMessage("go")},
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("a malformed generation must still fail the conformance check")
				}
				return
			}
			if err != nil {
				t.Fatalf("a complete generation must survive a size verdict: %v", err)
			}
			if res == nil || !strings.Contains(res.Message.Content, tc.wantText) {
				t.Fatalf("result did not carry the generation: %+v", res)
			}
		})
	}
}

// TestDefaultBudgetAccommodatesTheClientRequestCap pins the invariant whose
// violation caused the incident: the conformer's ceiling must never sit below
// what the agent itself is willing to ASK the provider for. cmd/neo/main.go
// builds the main client with an 8192-token cap and the truncated-answer ladder
// escalates to answerTokenCeiling, so a budget under either bound rejects
// generations the agent deliberately requested.
func TestDefaultBudgetAccommodatesTheClientRequestCap(t *testing.T) {
	budget := o1.DefaultBudget()
	if budget.MaxTokens < answerTokenCeiling {
		t.Fatalf("o1 budget MaxTokens = %d, must be >= the answer escalation ceiling %d — otherwise bumpAnswerBudget raises a limit the conformer then rejects",
			budget.MaxTokens, answerTokenCeiling)
	}
	if budget.MaxTokens < answerTokenFloor {
		t.Fatalf("o1 budget MaxTokens = %d is below the answer floor %d", budget.MaxTokens, answerTokenFloor)
	}
	// The historical failure, pinned by value so it can never come back.
	if budget.MaxTokens < 4098 {
		t.Fatalf("o1 budget MaxTokens = %d would still reject the 4098-token report from the 2026-07-25 incident", budget.MaxTokens)
	}
}

// TestBudgetErrorsAreDistinguishable pins the sentinel every caller relies on
// to tell "too big" apart from "unusable".
func TestBudgetErrorsAreDistinguishable(t *testing.T) {
	budget := o1.RepresentationBudget{MaxTokens: 10, MaxToolCalls: 1, MaxArgumentSize: 8}
	gen := &o1.NormalizedGeneration{
		Content:      "hello",
		FinishReason: "stop",
		Usage:        o1.Usage{CompletionTokens: 4098},
	}
	err := budget.BudgetCheck(gen)
	if err == nil {
		t.Fatal("an over-budget generation must report a verdict")
	}
	if !errors.Is(err, o1.ErrBudgetExceeded) {
		t.Fatalf("budget verdict must wrap ErrBudgetExceeded so callers can keep the generation, got %v", err)
	}
}
