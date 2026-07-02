// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llmtest provides a REAL scripted OpenAI-compatible SSE endpoint for
// tests: the code under test drives the real llm.Client over real HTTP and
// real SSE framing; only the model's decisions are scripted. This is the
// proven no-fakes recipe from the Neo turn-loop tests.
package llmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"matrix/cody/internal/llm"
	mcllm "matrix/mcl/llm"
)

// Msg is one request-side message as seen by the scripted endpoint.
type Msg struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name"`
	ToolCallID string `json:"tool_call_id"`
}

// Request is the decoded chat-completions request body.
type Request struct {
	Messages []Msg `json:"messages"`
}

// Call is one scripted tool call.
type Call struct {
	Name string
	Args map[string]interface{}
}

// Turn is one scripted assistant turn: either content or tool calls.
type Turn struct {
	Content string
	Calls   []Call
}

// Say scripts a plain content turn.
func Say(content string) Turn { return Turn{Content: content} }

// CallTool scripts a single-tool-call turn.
func CallTool(name string, args map[string]interface{}) Turn {
	return Turn{Calls: []Call{{Name: name, Args: args}}}
}

// LastToolResult returns the content of the most recent tool-role message.
func (r Request) LastToolResult() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "tool" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// NewServer starts a real HTTP server speaking OpenAI chat-completions SSE.
// Each request advances the script; the script sees the full live message
// window so steps can react to real tool results exactly as a model would.
func NewServer(t *testing.T, script func(step int, req Request) Turn) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	step := 0
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		mu.Lock()
		turn := script(step, req)
		step++
		mu.Unlock()

		rw.Header().Set("Content-Type", "text/event-stream")
		writeChunk := func(v interface{}) {
			data, err := json.Marshal(v)
			if err != nil {
				t.Errorf("llmtest: marshal chunk: %v", err)
				return
			}
			fmt.Fprintf(rw, "data: %s\n\n", data)
		}
		type deltaToolFn struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		type deltaTool struct {
			Index    int         `json:"index"`
			ID       string      `json:"id"`
			Type     string      `json:"type"`
			Function deltaToolFn `json:"function"`
		}
		type delta struct {
			Role      string      `json:"role,omitempty"`
			Content   string      `json:"content,omitempty"`
			ToolCalls []deltaTool `json:"tool_calls,omitempty"`
		}
		type choice struct {
			Index        int    `json:"index"`
			Delta        delta  `json:"delta"`
			FinishReason string `json:"finish_reason,omitempty"`
		}
		type chunk struct {
			Choices []choice `json:"choices"`
		}

		writeChunk(chunk{Choices: []choice{{Delta: delta{Role: "assistant"}}}})
		finish := "stop"
		if len(turn.Calls) > 0 {
			finish = "tool_calls"
			for i, c := range turn.Calls {
				args, err := json.Marshal(c.Args)
				if err != nil {
					t.Errorf("llmtest: marshal args: %v", err)
					return
				}
				writeChunk(chunk{Choices: []choice{{Delta: delta{ToolCalls: []deltaTool{{
					Index: i, ID: fmt.Sprintf("call_%d_%d", step, i), Type: "function",
					Function: deltaToolFn{Name: c.Name, Arguments: string(args)},
				}}}}}})
			}
		} else {
			writeChunk(chunk{Choices: []choice{{Delta: delta{Content: turn.Content}}}})
		}
		writeChunk(chunk{Choices: []choice{{Delta: delta{}, FinishReason: finish}}})
		fmt.Fprint(rw, "data: [DONE]\n\n")
	}))
}

// NewClient builds a real llm.Client pointed at the scripted server via the
// gateway path (no API key needed).
func NewClient(t *testing.T, srv *httptest.Server) *llm.Client {
	t.Helper()
	c, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/test-model",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
