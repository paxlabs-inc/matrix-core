// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// This file proves PROPERTY 2 (task 6.2): a healthy run receives ZERO
// modifications (silent when healthy), and the resulting window is byte-identical
// to the controller being disabled. It drives the REAL agent.Chat loop with a
// scripted llm.Client that reproduces a clean multi-step self-correction (the
// mimo-2.5-pro baseline shape): distinct productive steps, then a bare final
// answer — no repeat, no cycle, no premature/unverified close. No fakes: the
// controller, the Agent, the loop, and every llm type are real.

// scriptStep is one scripted model turn: an assistant preamble (may be empty),
// and EITHER a single tool call (toolName != "") or a bare-answer close (toolName
// == "", the turn ends).
type scriptStep struct {
	content  string
	toolName string
	toolArgs string
}

// scriptedServer streams the scripted steps in order (clamping to the last step
// once exhausted, so an over-running loop keeps getting the final turn). It is a
// real OpenAI-compatible SSE endpoint the real llm.Client talks to.
func scriptedServer(t *testing.T, steps []scriptStep, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()
		if idx >= len(steps) {
			idx = len(steps) - 1
		}
		step := steps[idx]

		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"role": "assistant"}
		if step.content != "" {
			delta["content"] = step.content
		}
		finish := "stop"
		if step.toolName != "" {
			finish = "tool_calls"
			delta["tool_calls"] = []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", idx), "type": "function",
				"function": map[string]any{"name": step.toolName, "arguments": step.toolArgs},
			}}
		}
		frame := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n", finish)
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runScripted drives the REAL agent.Chat loop against the scripted server with
// the controller enabled or disabled, and returns the agent (its a.working IS the
// window the model saw) plus the terminal error.
func runScripted(t *testing.T, steps []scriptStep, enabled bool, goal string) (*Agent, error) {
	t.Helper()
	var calls int
	var mu sync.Mutex
	srv := scriptedServer(t, steps, &calls, &mu)

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
	cfg.NeocortexActor = "neo-cassandra-silent"
	cfg.CassandraEnabled = enabled

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { pager.Close() })

	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: "conv-cassandra-silent",
	})
	err = a.Chat(context.Background(), goal)
	return a, err
}

// cleanRun is a clean self-correction: four DISTINCT productive steps (each a new
// tool → no repeat, no cycle, no semantic repeat), then a bare final answer while
// work is done and no repeat pressure remains → the healthy close (no premature/
// unverified trigger). The controller must stay completely silent.
func cleanRun() []scriptStep {
	return []scriptStep{
		{content: "Let me look at the first file.", toolName: "read_alpha", toolArgs: `{"path":"/a"}`},
		{content: "Now the second.", toolName: "read_beta", toolArgs: `{"path":"/b"}`},
		{content: "Checking the third.", toolName: "read_gamma", toolArgs: `{"path":"/c"}`},
		{content: "And the fourth.", toolName: "read_delta", toolArgs: `{"path":"/d"}`},
		{content: "All four are consistent — here is the summary."}, // bare-answer close
	}
}

func TestCassandraStaysSilentOnCleanRun(t *testing.T) {
	a, err := runScripted(t, cleanRun(), true, "review the four files and summarize")
	if err != nil {
		t.Fatalf("a clean run must finish cleanly, got: %v", err)
	}
	if len(a.turn.casRecord) != 0 {
		t.Fatalf("a healthy run must receive ZERO mods, got %d: %+v", len(a.turn.casRecord), a.turn.casRecord)
	}
}

// TestCassandraCleanRunByteIdenticalToDisabled proves req.4.3 / req.10.2: with no
// trigger firing, the window the model saw is byte-identical to the controller
// being disabled. Both runs are deterministic (same scripted server, same call
// order), so the only possible difference would be a Cassandra edit — and there
// is none on a clean run.
func TestCassandraCleanRunByteIdenticalToDisabled(t *testing.T) {
	on, errOn := runScripted(t, cleanRun(), true, "review the four files and summarize")
	off, errOff := runScripted(t, cleanRun(), false, "review the four files and summarize")
	if errOn != nil || errOff != nil {
		t.Fatalf("clean runs must both finish cleanly, got on=%v off=%v", errOn, errOff)
	}
	if !reflect.DeepEqual(on.working, off.working) {
		t.Fatalf("enabled window must be byte-identical to disabled on a clean run\n enabled=%s\ndisabled=%s",
			renderTranscript(on.working), renderTranscript(off.working))
	}
}
