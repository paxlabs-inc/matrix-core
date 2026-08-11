// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// The single window law (MORPHEUS req.1.3/1.4, task 1.1): byte-stable system
// prefix at index 0 across every step, append-only transcript, ONE trailing
// USER-role tail in fixed section order — and ONE oversize-recovery path
// (cmTrimWorking) proven at both the 1M default and a small 32K window, on the
// REAL loop (real Agent.Chat, real pager over a temp Neocortex, real httptest SSE
// model endpoint).

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func decodeWireMessages(t *testing.T, body []byte) []wireMessage {
	t.Helper()
	var wire struct {
		Messages []wireMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	return wire.Messages
}

func windowLawClient(t *testing.T, url string) *llm.Client {
	t.Helper()
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  url,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return client
}

func windowLawAgent(t *testing.T, cfg config.Config, url string) *Agent {
	t.Helper()
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	return New(Options{
		Config: cfg,
		Main:   windowLawClient(t, url),
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: "conv-window-law",
	})
}

// TestWindowLaw_GoldenOnRealMultiStepTurn drives a genuine multi-step turn
// (tool call, then final answer) and asserts the exact window shape on every
// request the model actually received.
func TestWindowLaw_GoldenOnRealMultiStepTurn(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		call := len(bodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			tc := `{"index":0,"id":"call_0","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\",\"expect\":\"a search result list\"}"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-window-law"
	a := windowLawAgent(t, cfg, srv.URL)

	if err := a.Chat(context.Background(), "look something up and report"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	captured := make([][]byte, len(bodies))
	copy(captured, bodies)
	mu.Unlock()
	if len(captured) < 2 {
		t.Fatalf("the turn must be multi-step; got %d model calls", len(captured))
	}

	var prefix string
	var prevTranscript []wireMessage
	for step, raw := range captured {
		msgs := decodeWireMessages(t, raw)
		if len(msgs) < 3 {
			t.Fatalf("step %d: window must be [system, transcript..., user tail]; got %d messages", step, len(msgs))
		}

		// Byte-stable system prefix at index 0, identical across steps.
		if msgs[0].Role != "system" {
			t.Fatalf("step %d: window[0].role = %q, want system", step, msgs[0].Role)
		}
		if step == 0 {
			prefix = msgs[0].Content
		} else if msgs[0].Content != prefix {
			t.Fatalf("step %d: system prefix not byte-stable across steps (len %d vs %d)", step, len(msgs[0].Content), len(prefix))
		}

		// ONE trailing SYSTEM context sidecar, in the fixed section order.
		tail := msgs[len(msgs)-1]
		if tail.Role != "system" {
			t.Fatalf("step %d: trailing message role = %q, want system context", step, tail.Role)
		}
		prev := -1
		for _, s := range []string{"Reference notes, not a new message", "Standing objective for this conversation", "[context:"} {
			idx := strings.Index(tail.Content, s)
			if idx < 0 {
				t.Fatalf("step %d: tail missing fixed section %q:\n%s", step, s, tail.Content)
			}
			if idx <= prev {
				t.Fatalf("step %d: tail section order broken at %q", step, s)
			}
			prev = idx
		}

		// Append-only transcript: this step's middle extends the previous
		// step's middle without rewriting it.
		transcript := msgs[1 : len(msgs)-1]
		if len(transcript) < len(prevTranscript) {
			t.Fatalf("step %d: transcript shrank (%d < %d) — not append-only", step, len(transcript), len(prevTranscript))
		}
		for i, m := range prevTranscript {
			if transcript[i].Role != m.Role || transcript[i].Content != m.Content {
				t.Fatalf("step %d: transcript message %d rewritten (was role=%q %q, now role=%q %q) — not append-only",
					step, i, m.Role, m.Content, transcript[i].Role, transcript[i].Content)
			}
		}
		prevTranscript = transcript
	}
}

// TestWindowLaw_TrimFiresUnderPressureAt32K proves the token-budget trim on
// the one recovery path at a small window: a seeded transcript far over the
// 32K hard budget is trimmed BEFORE the model is called, the turn completes,
// and the window the model received is within the budget shape.
func TestWindowLaw_TrimFiresUnderPressureAt32K(t *testing.T) {
	var (
		mu       sync.Mutex
		lastBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = raw
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.ContextWindowTokens = 32000
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-window-32k"
	a := windowLawAgent(t, cfg, srv.URL)

	// ~45K tokens of history: decisively over the 32K window's hard budget.
	filler := strings.Repeat("padding words that occupy the window with real bytes ", 280) // ~15KB per turn
	var history []llm.Message
	for i := 0; i < 12; i++ {
		history = append(history,
			llm.UserMessage(fmt.Sprintf("turn %d: %s", i, filler)),
			llm.AssistantMessage(fmt.Sprintf("reply %d acknowledged", i)),
		)
	}
	a.Seed(history, "long-running task")
	seeded := len(a.working)

	if err := a.Chat(context.Background(), "and now finish up"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(a.working) >= seeded {
		t.Fatalf("the 32K hard budget must trim the seeded window: seeded=%d after=%d", seeded, len(a.working))
	}
	mu.Lock()
	body := append([]byte(nil), lastBody...)
	mu.Unlock()
	msgs := decodeWireMessages(t, body)
	if msgs[0].Role != "system" || msgs[len(msgs)-1].Role != "system" {
		t.Fatalf("trimmed window must keep the single shape (system head, context sidecar); head=%q tail=%q", msgs[0].Role, msgs[len(msgs)-1].Role)
	}
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
	}
	if full := len(filler) * 12; total >= full {
		t.Fatalf("the model must have received a genuinely trimmed window: %d bytes >= %d seeded bytes", total, full)
	}
}

// TestWindowLaw_NoTrimWithoutPressureAt1M proves the trim triggers only under
// genuine pressure at the 1M default: a modest transcript passes untouched.
func TestWindowLaw_NoTrimWithoutPressureAt1M(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-window-1m"
	a := windowLawAgent(t, cfg, srv.URL)

	var history []llm.Message
	for i := 0; i < 10; i++ {
		history = append(history,
			llm.UserMessage(fmt.Sprintf("turn %d with ordinary content", i)),
			llm.AssistantMessage(fmt.Sprintf("reply %d", i)),
		)
	}
	a.Seed(history, "ordinary task")
	seeded := len(a.working)

	if err := a.Chat(context.Background(), "carry on"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// user message + assistant reply appended; nothing trimmed.
	if want := seeded + 2; len(a.working) != want {
		t.Fatalf("no trim may fire without pressure at 1M: want %d working messages, got %d", want, len(a.working))
	}
}

// TestWindowLaw_413RecoveryReentersTheOnePath proves the request-byte
// oversize recovery on the real loop: the "provider" rejects large bodies
// with HTTP 413, the loop recovers via the ONE path (image strip, then
// cmTrimWorking) and re-sends a smaller window through the same assembly
// site, completing the turn.
func TestWindowLaw_413RecoveryReentersTheOnePath(t *testing.T) {
	const byteCeiling = 200000
	var (
		mu       sync.Mutex
		rejected int
		accepted int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > byteCeiling {
			mu.Lock()
			rejected++
			mu.Unlock()
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"type":"body_too_large","message":"request body exceeds the limit"}}`))
			return
		}
		mu.Lock()
		accepted++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default() // 1M window: no token pressure, only the byte cap
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-window-413"
	a := windowLawAgent(t, cfg, srv.URL)

	filler := strings.Repeat("bytes that inflate the serialized request body well past the ceiling ", 600) // ~40KB per turn
	var history []llm.Message
	for i := 0; i < 8; i++ {
		history = append(history,
			llm.UserMessage(fmt.Sprintf("turn %d: %s", i, filler)),
			llm.AssistantMessage(fmt.Sprintf("reply %d", i)),
		)
	}
	a.Seed(history, "big-body task")
	seeded := len(a.working)

	if err := a.Chat(context.Background(), "finish"); err != nil {
		t.Fatalf("Chat must recover from the 413, got: %v", err)
	}

	mu.Lock()
	nRejected, nAccepted := rejected, accepted
	mu.Unlock()
	if nRejected == 0 {
		t.Fatal("test must actually exercise the 413 reject (no oversized request was sent)")
	}
	if nAccepted == 0 {
		t.Fatal("the recovery must re-send a smaller window that the provider accepts")
	}
	if len(a.working) >= seeded {
		t.Fatalf("the 413 recovery must trim the working window: seeded=%d after=%d", seeded, len(a.working))
	}
	// The recovery re-enters the ONE assembly site (MORPHEUS req.3.2): each
	// retry adds an assembly through prepareWindow, never a private window.
	if a.windowAssemblies < 2 {
		t.Fatalf("the 413 recovery must re-enter the one assembly site: %d assemblies", a.windowAssemblies)
	}
}
