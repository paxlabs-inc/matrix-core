// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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

	"matrix/cortex"
	"matrix/cortex/store"
	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// Continuous-memory task 8.5 — Property 5: the agent is a thin client of the
// cortex brain, with durable memory (validates req.9.1, 9.2, 9.3, 9.4, 12.5).
//
// These run against the REAL turn loop (agent.Chat) driving a REAL Pager over a
// REAL cortex store, with the model boundary served by a scripted OpenAI SSE
// endpoint (the external LLM is the only boundary stubbed — every code path
// under test, cortex + activation + the agent loop, is real; req.12.7).

// sseChatServer returns an httptest server that answers the neo llm client's
// streamed chat-completions POST with a single scripted assistant turn
// (content, finish_reason=stop, no tool calls) and records the last request
// body so the test can assert on the window the agent actually sent.
func sseChatServer(t *testing.T, answer string, lastBody *[]byte, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		mu.Lock()
		*lastBody = body
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": answer},
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

// TestContinuousCollapse_TurnConsumesActivateAndAppends drives one real turn
// with the continuous-memory collapse active and proves:
//   - Neo APPENDS the user + assistant messages to the durable cortex
//     transcript (req.9.1) — read back through cortex, not the working slice.
//   - Neo CONSUMES cortex.Activate and RENDERS the bundle as a trailing
//     USER-role message, with the byte-stable system prefix at index 0
//     (req.9.2, 9.4) — the legacy dynamicTail path did not produce this shape.
//   - a.summary is never populated on the collapse path (a.summary retired,
//     req.9.3).
func TestContinuousCollapse_TurnConsumesActivateAndAppends(t *testing.T) {
	var lastBody []byte
	var mu sync.Mutex
	srv := sseChatServer(t, "Hi Andrew, noted.", &lastBody, &mu)

	cfg := config.Default()
	cfg.ContinuousMemory = true
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-collapse-8-5"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	const conv = "conv-collapse-8-5"
	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: conv,
	})
	if !a.continuousMemory() {
		t.Fatal("precondition: continuous-memory path must be active")
	}

	const userMsg = "hello, what do you know about me?"
	if err := a.Chat(context.Background(), userMsg); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// a.summary must never be set on the collapse path (a.summary retired).
	if a.summary != "" {
		t.Errorf("a.summary must stay empty on the continuous-memory path, got %q", a.summary)
	}

	// The window the agent actually sent: system at index 0, and the LAST
	// message is a USER-role message carrying the rendered Activate bundle.
	mu.Lock()
	body := append([]byte(nil), lastBody...)
	mu.Unlock()
	if len(body) == 0 {
		t.Fatal("no request body captured from the model endpoint")
	}
	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal request body: %v\nbody=%s", err, body)
	}
	if len(wire.Messages) < 2 {
		t.Fatalf("expected >=2 messages (system + user tail), got %d", len(wire.Messages))
	}
	if wire.Messages[0].Role != "system" {
		t.Errorf("message[0].role = %q, want system (byte-stable prefix at index 0)", wire.Messages[0].Role)
	}
	tail := wire.Messages[len(wire.Messages)-1]
	if tail.Role != "user" {
		t.Errorf("trailing message role = %q, want user (Qwen-template portability, req.9.4)", tail.Role)
	}
	// The header string is produced ONLY by renderActivationBundle — its
	// presence proves the trailing message is the rendered cortex.Activate
	// bundle, not the legacy dynamicTail.
	if !strings.Contains(tail.Content, "Your durable memory") {
		t.Errorf("trailing message must be the rendered Activate bundle; got:\n%s", tail.Content)
	}
	if !strings.Contains(tail.Content, "[context:") {
		t.Errorf("trailing message must carry the context-budget stat; got:\n%s", tail.Content)
	}

	// Neo appended the user + assistant messages to the DURABLE cortex
	// transcript — read them back through cortex.Activate's T2 slice.
	bundle, err := pager.Activate(conv, "", cortex.Budget{})
	if err != nil {
		t.Fatalf("pager.Activate: %v", err)
	}
	var sawUser, sawAssistant bool
	for _, m := range bundle.Transcript {
		if m.Role == cortex.RoleUser && strings.Contains(m.Content, "what do you know about me") {
			sawUser = true
		}
		if m.Role == cortex.RoleAssistant && strings.Contains(m.Content, "noted") {
			sawAssistant = true
		}
	}
	if !sawUser {
		t.Errorf("cortex transcript missing the appended user message; got %d messages", len(bundle.Transcript))
	}
	if !sawAssistant {
		t.Errorf("cortex transcript missing the appended assistant message; got %d messages", len(bundle.Transcript))
	}
}

// TestContinuousCollapse_StorySoFarSurvivesRestart proves the durable
// story-so-far replacement for a.summary survives a simulated process restart
// (req.9.3 durability): a story built over a conversation transcript is
// re-loadable, byte-for-byte, from a FRESH cortex opened over the same store
// after the first is closed.
func TestContinuousCollapse_StorySoFarSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	const actor = "neo-story-restart"
	const conv = "conv-story-restart"

	s1, err := store.Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("store.Open (first): %v", err)
	}
	c1 := cortex.New(s1)

	msgs := []cortex.Message{
		{ConversationID: conv, Role: cortex.RoleUser, Content: "launch the PAX token"},
		{ConversationID: conv, Role: cortex.RoleAssistant, Content: "on it — routing through the secure path"},
		{ConversationID: conv, Role: cortex.RoleToolCall, ToolName: "core_execute", ToolArgs: []byte(`{"verb":"launch"}`)},
		{ConversationID: conv, Role: cortex.RoleToolResult, ToolName: "core_execute", Content: "launched at 0xabc"},
		{ConversationID: conv, Role: cortex.RoleUser, Content: "thanks, what next?"},
	}
	for i, m := range msgs {
		if _, err := c1.AppendMessage(m); err != nil {
			t.Fatalf("AppendMessage #%d: %v", i, err)
		}
	}

	uri, err := c1.BuildStorySoFar(conv)
	if err != nil {
		t.Fatalf("BuildStorySoFar: %v", err)
	}
	if uri == "" {
		t.Fatal("BuildStorySoFar returned empty URI for a non-empty transcript")
	}
	before, err := c1.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar (before restart): %v", err)
	}
	if before == nil || strings.TrimSpace(before.ShortForm) == "" {
		t.Fatalf("story-so-far must have a non-empty short form before restart: %+v", before)
	}

	// Simulate a process restart: close the store, reopen a fresh cortex over
	// the same on-disk state.
	if err := s1.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	s2, err := store.Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("store.Open (restart): %v", err)
	}
	defer func() { _ = s2.Close() }()
	c2 := cortex.New(s2)

	after, err := c2.LoadStorySoFar(conv)
	if err != nil {
		t.Fatalf("LoadStorySoFar (after restart): %v", err)
	}
	if after == nil {
		t.Fatal("story-so-far did not survive the restart (nil after reopen)")
	}
	if after.ShortForm != before.ShortForm {
		t.Errorf("story short-form drifted across restart:\nbefore=%q\nafter=%q", before.ShortForm, after.ShortForm)
	}
	if after.MessageCount != before.MessageCount || after.UpToSeq != before.UpToSeq {
		t.Errorf("story metadata drifted across restart: before{count=%d seq=%d} after{count=%d seq=%d}",
			before.MessageCount, before.UpToSeq, after.MessageCount, after.UpToSeq)
	}
}

// TestContinuousCollapse_TrimIsNonSummarizing proves a.compact is retired on
// the collapse path (req.9.3): the over-budget window trim (cmTrimWorking) is a
// pure in-memory bound with NO LLM summarization pass. The agent is built with
// a NIL model client — if the trim tried to summarize (the legacy a.compact
// behavior) it would dereference the nil client and panic; a clean bound proves
// it does not.
func TestContinuousCollapse_TrimIsNonSummarizing(t *testing.T) {
	cfg := config.Default()
	cfg.ContinuousMemory = true
	a := New(Options{Config: cfg}) // Main is nil: no model available

	// A long multi-turn working set with several user turns to carve.
	for i := 0; i < 8; i++ {
		a.working = append(a.working,
			llm.UserMessage(fmt.Sprintf("user turn %d with some content to occupy the window", i)),
			llm.AssistantMessage(fmt.Sprintf("assistant reply %d acknowledging the turn", i)),
		)
	}
	beforeLen := len(a.working)

	a.cmTrimWorking() // must not touch the (nil) model

	if a.summary != "" {
		t.Errorf("cmTrimWorking must not produce a summary (a.compact retired), got %q", a.summary)
	}
	if len(a.working) >= beforeLen {
		t.Errorf("cmTrimWorking must bound the window: before=%d after=%d", beforeLen, len(a.working))
	}
	if len(a.working) == 0 {
		t.Error("cmTrimWorking must keep the recent turns, not clear the window")
	}
}

// TestCmActivateCarriesWindowProportionalBudget proves the residency thread
// end to end on REAL components: with the default 1M window, cmActivate's
// bundle admits a transcript far beyond cortex's legacy 3-4K default — the
// derived config.ActivationBudget actually reaches cortex.Activate.
func TestCmActivateCarriesWindowProportionalBudget(t *testing.T) {
	cfg := config.Default()
	cfg.ContinuousMemory = true
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-cm-budget"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	a := New(Options{Config: cfg, Tools: &tools.Manager{}, Pager: pager, ConvID: "conv-cm-budget"})

	line := "step report: verified the build, reran the suite, recorded the outcome for posterity and moved on"
	for i := 0; i < 300; i++ {
		if _, err := pager.AppendMessage(cortex.Message{ConversationID: "conv-cm-budget", Role: cortex.RoleUser, Content: line}); err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
	}

	b := a.cmActivate("what happened so far")
	if b == nil {
		t.Fatal("cmActivate returned nil")
	}
	if b.TotalTokens <= 4000 {
		t.Fatalf("bundle carries only %d tokens at a 1M window — the legacy cortex default is still binding", b.TotalTokens)
	}
	if want := cfg.ActivationBudget(); b.TotalTokens > want {
		t.Fatalf("bundle %d tokens exceeds the derived budget %d", b.TotalTokens, want)
	}
}
