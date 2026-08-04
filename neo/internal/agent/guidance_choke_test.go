// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestGuidanceChokePoint_SingleProductionCallSite pins the structural half of
// MORPHEUS req.3.3: llm.GuidanceMessage has exactly ONE production call site
// in the agent package — pushGuidance, the choke point. Every guidance
// producer (nudges, revision directives, mismatch guidance, gate refusal
// directives, re-anchors) must route through it; a second construction site
// would reopen the bypass the reassembly closed.
func TestGuidanceChokePoint_SingleProductionCallSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "llm.GuidanceMessage(") {
				sites = append(sites, fmt.Sprintf("%s:%d", name, i+1))
			}
			// A hand-rolled guidance turn (Guidance: true literal) would bypass
			// both the choke point and the envelope contract.
			if strings.Contains(strings.ReplaceAll(trimmed, " ", ""), "Guidance:true") {
				t.Errorf("%s:%d constructs a guidance message by hand — route it through pushGuidance", name, i+1)
			}
		}
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "agent.go:") {
		t.Fatalf("llm.GuidanceMessage must have exactly ONE production call site (pushGuidance in agent.go), found %v", sites)
	}
}

// chokeReporter records every user-facing string the loop emits, across all
// Reporter channels, so the test can prove no guidance leaked to any of them.
type chokeReporter struct {
	mu    sync.Mutex
	lines []string
}

func (r *chokeReporter) add(t string) {
	r.mu.Lock()
	r.lines = append(r.lines, t)
	r.mu.Unlock()
}

func (r *chokeReporter) Say(t string, _ bool)     { r.add(t) }
func (r *chokeReporter) Status(t string)          { r.add(t) }
func (r *chokeReporter) Progress(t string)        { r.add(t) }
func (r *chokeReporter) Notice(t string)          { r.add(t) }
func (r *chokeReporter) Think(t string)           { r.add(t) }
func (r *chokeReporter) Delta(_ int, _, t string) { r.add(t) }

func (r *chokeReporter) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestGuidanceChokePoint_InvariantsOnRealLoop proves the choke point's two
// invariants on the real loop (req.3.3): a run whose close is blocked by the
// empty-answer guard (guidance genuinely fires and steers the model) ends with
// (a) the steering visible to the MODEL — the follow-up request body carries
// the guidance envelope — while (b) the durable Neocortex transcript records no
// guidance and (c) no user-facing Reporter channel carries it.
func TestGuidanceChokePoint_InvariantsOnRealLoop(t *testing.T) {
	const nudgeMarker = "Continue: either call a tool to make progress"

	var (
		mu     sync.Mutex
		bodies []string
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			// An empty bare answer: the empty_answer close guard must push a
			// guidance nudge and re-enter the loop.
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Here is the real answer."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-guidance-choke"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	rep := &chokeReporter{}
	const conv = "conv-guidance-choke"
	a := New(Options{
		Config:   cfg,
		Main:     revisionTestClient(t, model.URL),
		Tools:    &tools.Manager{},
		Pager:    pager,
		Reporter: rep,
		ConvID:   conv,
	})
	if err := a.Chat(context.Background(), "please answer the question"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	captured := append([]string(nil), bodies...)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("the scenario must take exactly two model calls (empty → nudged retry), got %d", len(captured))
	}

	// (a) The steering genuinely reached the MODEL: the retry window carries
	// the guidance envelope with the nudge text. (json.Marshal escapes the
	// angle brackets, so match the tag name, not the literal tag.)
	if !strings.Contains(captured[1], "system_guidance") || !strings.Contains(captured[1], "Continue: either call a tool") {
		t.Fatal("the retry request must carry the guidance envelope — the nudge never reached the model")
	}
	// And the working transcript holds it as a flagged guidance turn.
	guidanceTurns := 0
	for _, m := range a.working {
		if m.IsGuidance() {
			guidanceTurns++
		}
	}
	if guidanceTurns != 1 {
		t.Fatalf("the working transcript must hold exactly one guidance turn, got %d", guidanceTurns)
	}

	// (b) The durable Neocortex transcript stays guidance-free.
	msgs, err := pager.Transcript(conv, 0, 200)
	if err != nil {
		t.Fatalf("pager.Transcript: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("the durable transcript must have recorded the turn")
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, llm.GuidanceOpen) || strings.Contains(m.Content, nudgeMarker) {
			t.Fatalf("guidance leaked into the durable Neocortex transcript: %q", m.Content)
		}
	}

	// (c) No user-facing Reporter channel carries the steering.
	for _, line := range rep.all() {
		if strings.Contains(line, llm.GuidanceOpen) || strings.Contains(line, llm.GuidanceClose) || strings.Contains(line, nudgeMarker) {
			t.Fatalf("guidance leaked to a user-facing surface: %q", line)
		}
	}
	// The real answer still shipped.
	delivered := false
	for _, line := range rep.all() {
		if strings.Contains(line, "Here is the real answer.") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("the nudged retry's real answer must be delivered")
	}
}
