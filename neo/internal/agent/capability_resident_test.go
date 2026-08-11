// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
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
	"matrix/neo/internal/tools"
)

// trueSurface mirrors the facts the server derives from its live route table
// (see neo/internal/server SurfaceFacts) — the arena incident's refuting truth.
func trueSurface() *CapabilitySurface {
	return &CapabilitySurface{
		API: []string{
			"POST /chat — send a user message; the reply streams back over the SSE event stream (this is the ONLY way to talk to you)",
			"GET /events — the SSE event stream carrying your replies, tool steps, and workspace events",
		},
		Is: []string{
			"You are the conversational agent behind the Centra AI daemon's HTTP front: clients send POST /chat and read your streamed replies over SSE GET /events.",
		},
		IsNot: []string{
			"You have NO OpenAI-compatible endpoint: /v1/chat/completions, /v1/completions, and /v1/models are NOT served. Nothing can integrate with you as an OpenAI-style API.",
		},
		FailurePatterns: []string{"[failure-mode: probing spiral] guessed endpoints instead of checking the self-model"},
	}
}

// TestCapabilitySurfaceResidentAndByteStable proves epistemic-core req.2.4 on
// the real loop: the rendered capability section carries the true external
// surface, and the system prefix that actually rides each model request is
// byte-identical across a REAL multi-step turn (tool call step + final step)
// against a real httptest SSE endpoint. No fakes: real Agent.Chat, real
// tools.Manager dispatch, real client transport.
func TestCapabilitySurfaceResidentAndByteStable(t *testing.T) {
	var (
		mu      sync.Mutex
		systems []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Capture the leading system prefix of the REAL request bytes. The
		// system message is the first element of "messages"; slice up to the
		// next role marker so we compare the prefix, not the whole window.
		s := string(body)
		if i := strings.Index(s, `"messages"`); i >= 0 {
			if j := strings.Index(s[i:], `"role":"user"`); j >= 0 {
				mu.Lock()
				systems = append(systems, s[i:i+j])
				mu.Unlock()
			}
		}

		mu.Lock()
		step := len(systems)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if step <= 1 {
			// Step 0: one tool call, so the turn is genuinely multi-step.
			tc := `{"index":0,"id":"call_0","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"anything\"}"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
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
	cfg := config.Default()
	cfg.CassandraEnabled = false
	a := New(Options{
		Config:     cfg,
		Main:       client,
		Tools:      &tools.Manager{},
		Capability: trueSurface(),
	})

	if err := a.Chat(context.Background(), "wire yourself up to the other agent"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(systems) < 2 {
		t.Fatalf("expected a multi-step turn (>=2 model calls), got %d", len(systems))
	}

	// The resident truth rides the real request bytes.
	first := systems[0]
	for _, want := range []string{"POST /chat", "SSE", "NO OpenAI-compatible endpoint", "structured schemas", "probing spiral"} {
		if !strings.Contains(first, want) {
			t.Errorf("resident system prefix missing %q", want)
		}
	}
	if strings.Contains(first, "read_overflow") {
		t.Fatal("structured tool schema was duplicated into system prose")
	}
	// Byte-identical across every step of the turn (req.2.4 / prompt-cache).
	for i := 1; i < len(systems); i++ {
		if systems[i] != first {
			t.Fatalf("system prefix mutated between step 0 and step %d", i)
		}
	}
}

// TestCapabilitySurfaceHonestUnknown proves req.2.3: when no capability
// material is resolved (fresh install, no artifact), the rendered section says
// UNKNOWN explicitly instead of fabricating surface facts.
func TestCapabilitySurfaceHonestUnknown(t *testing.T) {
	a := New(Options{Config: config.Default()})
	rendered := a.renderCapabilitySurface()
	if !strings.Contains(rendered, "UNKNOWN — your self-model carries no external API surface facts") {
		t.Fatalf("missing honest unknown for the API surface:\n%s", rendered)
	}
	if strings.Contains(rendered, "POST /chat") {
		t.Fatal("must not fabricate surface facts when the artifact lacks them")
	}
}
