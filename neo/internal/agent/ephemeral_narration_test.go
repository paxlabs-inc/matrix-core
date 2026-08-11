// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
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
	"matrix/neo/internal/tools"
)

// channelReporter records which Reporter channel each message went to, so a
// test can prove the synthetic narrate-before-act stub rides the EPHEMERAL
// Progress channel while the real answer is delivered via Say.
type channelReporter struct {
	mu       sync.Mutex
	said     []string
	sayFinal []bool
	status   []string
	progress []string
	thinking []string
	deltas   []string
}

func (c *channelReporter) Say(text string, completion bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.said = append(c.said, text)
	c.sayFinal = append(c.sayFinal, completion)
}
func (c *channelReporter) Status(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Mirror the production durable-narration reporters (sseReporter.Status /
	// the swarm reporter): the "• <tool>" tool-start markers are DROPPED — they
	// never become durable narration and never set run.narrated. Only genuine
	// model narration is recorded here, which is what the invariant is about.
	if strings.HasPrefix(strings.TrimSpace(text), "• ") {
		return
	}
	c.status = append(c.status, text)
}
func (c *channelReporter) Progress(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progress = append(c.progress, text)
}
func (c *channelReporter) Notice(string) {}
func (c *channelReporter) Think(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.thinking = append(c.thinking, text)
}
func (c *channelReporter) Delta(_ int, kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deltas = append(c.deltas, kind+":"+text)
}

// straightToCompleteServer scripts the exact grok shape from the 2026-07-05
// LayerX drop, adapted to Cassandra 2.0 (the completion gate is retired): the
// model goes STRAIGHT to a tool with NO preamble content (first call), then
// closes with a lone BARE ANSWER message and no genuine preceding narration
// (second call). The only "narration" is the synthetic intent stub. If that stub
// is treated as durable narration, the real answer is suppressed.
func straightToCompleteServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			// Straight to a real tool, no preamble content.
			tcJSON := map[string]any{
				"index": 0, "id": "call_search", "type": "function",
				"function": map[string]any{
					"name":      "web_search",
					"arguments": `{"query":"layerx balance"}`,
				},
			}
			frame := map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role":              "assistant",
						"reasoning_content": "Let me assess where I am.",
						"tool_calls":        []any{tcJSON},
					},
				}},
			}
			fb, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n", fb)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		// Close with a lone bare answer (no tool calls) — the single completion
		// path now that the gate is retired.
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Your LayerX balance is 1000.000000 USDX (all in escrow)."},
			}},
		}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStraightToCompleteDeliversAnswerViaEphemeralStub is the 2026-07-05 LayerX
// regression guard: when the model goes straight to a tool and then a lone
// task_complete (no genuine narration), the synthetic intent stub MUST ride the
// ephemeral Progress channel (never durable Status), and the real answer MUST be
// delivered via Say. Routing the stub through durable Status was what marked the
// turn "already narrated" so the real task_complete summary got hidden and the
// user saw only "Layerx deposit." / "Layerx balance.".
func TestStraightToCompleteDeliversAnswerViaEphemeralStub(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := straightToCompleteServer(t, &calls, &mu)

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-straight-complete"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	rep := &channelReporter{}
	a := New(Options{
		Config:   cfg,
		Main:     client,
		Tools:    &tools.Manager{},
		Pager:    pager,
		Reporter: rep,
		ConvID:   "conv-straight-complete",
	})

	if err := a.Chat(context.Background(), "check my layerx balance"); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	rep.mu.Lock()
	defer rep.mu.Unlock()

	// The synthetic intent stub must have ridden the EPHEMERAL Progress channel.
	if len(rep.progress) == 0 {
		t.Fatal("expected the synthetic narrate-before-act stub on the ephemeral Progress channel, got none")
	}
	// It must NOT have been emitted as durable Status narration — that is the
	// exact routing that suppressed the real answer.
	if len(rep.status) != 0 {
		t.Fatalf("synthetic stub must NOT ride durable Status (would suppress the real answer); got Status=%v", rep.status)
	}
	// Tool-call working text and reasoning are private. Neither the streaming
	// delta channel nor the post-hoc thinking fallback may expose them.
	if len(rep.thinking) != 0 {
		t.Fatalf("tool-call reasoning must not reach Think; got %v", rep.thinking)
	}
	for _, delta := range rep.deltas {
		if strings.Contains(delta, "Let me assess where I am") {
			t.Fatalf("tool-call reasoning must not reach Delta; got %v", rep.deltas)
		}
	}
	// The real answer must be delivered to the user via Say.
	if len(rep.said) != 1 {
		t.Fatalf("expected exactly one delivered answer via Say, got %d: %v", len(rep.said), rep.said)
	}
	if !strings.Contains(rep.said[0], "1000.000000 USDX") {
		t.Fatalf("delivered answer must carry the real balance, got %q", rep.said[0])
	}
	// With the completion gate retired, the answer is delivered on the single
	// bare-answer completion path, so completion=false (there is no separate
	// task_complete summary anymore). What matters is that the REAL answer was
	// delivered via Say and the synthetic stub did not suppress it.
	if rep.sayFinal[0] {
		t.Fatalf("a bare-answer completion must be delivered with completion=false (the gate is retired)")
	}
}
