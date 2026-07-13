// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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

// TestProbeStrategyGroupsVariedArgs proves the strategy key ignores the
// varying argument (req.6.2): five differently-guessed paths on one host are
// ONE strategy; a different host is a different strategy; non-probes are not
// classified.
func TestProbeStrategyGroupsVariedArgs(t *testing.T) {
	s1, ok1 := probeStrategy("fetch_url", map[string]interface{}{"url": "http://api.example.com/ohlc"})
	s2, ok2 := probeStrategy("fetch_url", map[string]interface{}{"url": "http://api.example.com/candles"})
	s3, _ := probeStrategy("fetch_url", map[string]interface{}{"url": "http://other.example.com/ohlc"})
	if !ok1 || !ok2 {
		t.Fatal("url fetches must classify as probes")
	}
	if s1 != s2 {
		t.Fatalf("differently-argued probes of one host must share a strategy: %q vs %q", s1, s2)
	}
	if s1 == s3 {
		t.Fatal("different hosts must be different strategies")
	}
	if _, ok := probeStrategy("read_text_file", map[string]interface{}{"path": "/tmp/x"}); ok {
		t.Fatal("a plain file read is not a probe")
	}
	if s, ok := probeStrategy("shell_exec", map[string]interface{}{"command": "curl -s http://x/a"}); !ok || !strings.Contains(s, "curl") {
		t.Fatalf("exec probes key on the command head, got %q ok=%v", s, ok)
	}
}

// TestDetectMismatchDeterministic covers the deterministic detector table
// (req.6.2): HTTP status, exit code, error prefix, empty/nonempty, JSON parse
// — including expectations that PREDICT failure (a hypothesis probing for a
// 404 matches a 404).
func TestDetectMismatchDeterministic(t *testing.T) {
	cases := []struct {
		name             string
		expect, content  string
		isErr            bool
		mismatch, decide bool
	}{
		{"http 404 vs success expectation", "200 with a JSON array", "HTTP 404 Not Found", false, true, true},
		{"http 404 vs failure hypothesis", "a 404 (path does not exist)", "HTTP 404 Not Found", false, false, true},
		{"exit code failure", "exit 0 listing files", "command failed, exit code 2", false, true, true},
		{"error result", "200 OK", "error: connection refused", false, true, true},
		{"dispatch error", "200 OK", "tool unavailable", true, true, true},
		{"empty vs data expectation", "a JSON array of candles", "   ", false, true, true},
		{"valid json matches json expectation", "JSON array of candles", `[{"o":1,"h":2}]`, false, false, true},
		{"prose result undecided", "a list of validators", "here are some validators: alice, bob", false, false, false},
	}
	for _, c := range cases {
		mismatch, decided := detectMismatch(c.expect, c.content, c.isErr)
		if decided != c.decide || (decided && mismatch != c.mismatch) {
			t.Errorf("%s: got (mismatch=%v decided=%v), want (mismatch=%v decided=%v)", c.name, mismatch, decided, c.mismatch, c.decide)
		}
	}
}

// TestPredictionMeterAcrossRealDispatches proves req.6.1/6.2 on the real loop:
// expectations ride real dispatches through the real tools.Manager seam (the
// `expect` argument is lifted off the call — the tool never sees it), each
// failed probe is a detected deterministic mismatch, and the meter aggregates
// the OHLC-shaped strategy across differently-argued probes — two guessed
// paths on one host are one strategy with two mismatches. The mismatch renders
// as structured guidance in the next step's real request bytes, and the
// hypothesis premise on the ledger is refuted.
func TestPredictionMeterAcrossRealDispatches(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	probe := func(path string) string {
		return fmt.Sprintf(`{"index":0,"id":"call_%s","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://api.example.com%s\",\"expect\":\"200 with a JSON array of candles\"}"}}`, strings.Trim(path, "/"), path)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch call {
		case 1:
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", probe("/ohlc"))
		case 2:
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", probe("/candles"))
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"giving up"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
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
	var (
		obsMu   sync.Mutex
		obsArgs []map[string]interface{}
	)
	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{}, // real dispatch seam: fetch_url resolves to a real failure result
		Observer: func(ev ToolEvent) {
			if ev.Phase == ToolEnd {
				obsMu.Lock()
				obsArgs = append(obsArgs, ev.Args)
				obsMu.Unlock()
			}
		},
	})

	if err := a.Chat(context.Background(), "get the OHLC data"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The meter aggregated one strategy across differently-argued probes.
	strategy, _ := probeStrategy("fetch_url", map[string]interface{}{"url": "http://api.example.com/x"})
	if got := a.mismatchMeter[strategy]; got != 2 {
		t.Fatalf("meter for %q = %d, want 2 (two differently-argued probes of one strategy)", strategy, got)
	}

	// The expectation was lifted off the call — the dispatched args never
	// carried it (expectations ride the dispatch seam, not the tool).
	obsMu.Lock()
	for _, args := range obsArgs {
		if _, leaked := args["expect"]; leaked {
			t.Fatal("the expect argument must be stripped before the tool sees the call")
		}
	}
	obsMu.Unlock()

	// The mismatch rode the NEXT step's real request bytes as structured
	// guidance, and the ledger's hypothesis premise took the hit.
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("expected 3 model calls, got %d", len(bodies))
	}
	if !strings.Contains(bodies[1], "Prediction mismatch on strategy") {
		t.Fatal("the first mismatch must render as structured guidance in the next step's window")
	}
	if !strings.Contains(bodies[2], "2 consecutive") {
		t.Fatal("the second mismatch must carry the accumulated per-strategy count")
	}
	if a.ledger == nil || len(a.ledger.items) == 0 {
		t.Fatal("the probe hypotheses must live on the premise ledger")
	}
	refuted := 0
	for _, p := range a.ledger.items {
		if p.Status == premiseRefuted {
			refuted++
		}
	}
	if refuted == 0 {
		t.Fatal("a mismatched probe's hypothesis premise must be refuted on the ledger")
	}
}

// TestInjectExpectParamUniversal proves the universal `expect` advertisement:
// every advertised schema (native, synthetic, read_overflow) carries the
// expect property, and the injection is copy-on-write — the Manager's own
// parameter maps are untouched, so a second consumer sees pristine schemas.
func TestInjectExpectParamUniversal(t *testing.T) {
	cfg := config.Default()
	tm := &tools.Manager{}
	a := New(Options{Config: cfg, Tools: tm})

	if len(a.schemas) == 0 {
		t.Fatal("agent advertises no schemas")
	}
	for _, s := range a.schemas {
		props, ok := s.Function.Parameters["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: parameters carry no properties map", s.Function.Name)
		}
		exp, ok := props["expect"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: schema does not advertise the expect param", s.Function.Name)
		}
		if exp["type"] != "string" {
			t.Fatalf("%s: expect param is not a string", s.Function.Name)
		}
		// Enforcement lives at the dispatch seam, not provider-side JSON
		// schema strictness: expect must NOT be in required.
		if req, ok := s.Function.Parameters["required"].([]string); ok {
			for _, r := range req {
				if r == "expect" {
					t.Fatalf("%s: expect must not be schema-required", s.Function.Name)
				}
			}
		}
	}

	// Copy-on-write: the Manager's schemas stay pristine.
	for _, s := range tm.Schemas() {
		if props, ok := s.Function.Parameters["properties"].(map[string]interface{}); ok {
			if _, leaked := props["expect"]; leaked {
				t.Fatalf("%s: injection mutated the Manager's shared schema map", s.Function.Name)
			}
		}
	}

	// Disabled predictions ⇒ no injection.
	off := config.Default()
	off.EpistemicPredictions = false
	b := New(Options{Config: off, Tools: &tools.Manager{}})
	for _, s := range b.schemas {
		if props, ok := s.Function.Parameters["properties"].(map[string]interface{}); ok {
			if _, has := props["expect"]; has {
				t.Fatalf("%s: expect injected with EpistemicPredictions off", s.Function.Name)
			}
		}
	}
}

// TestPredictionObserveNonProbe proves the universal belief update: a NON-probe
// call that states an expectation gets the same mismatch treatment (ledger
// premise, meter, structured guidance), while an expectation-less non-probe
// stays outside the discipline (refusal is probe-only).
func TestPredictionObserveNonProbe(t *testing.T) {
	cfg := config.Default()
	a := New(Options{Config: cfg, Tools: &tools.Manager{}})

	before := len(a.working)
	missed := a.predictionObserve(context.Background(), "fs_write", map[string]interface{}{"path": "a.txt"},
		"file written cleanly", "error: permission denied", true)
	if !missed {
		t.Fatal("non-probe mismatch with a stated expectation must count as missed")
	}
	if a.mismatchMeter["fs_write"] != 1 {
		t.Fatalf("meter = %d, want 1 keyed by tool name", a.mismatchMeter["fs_write"])
	}
	guided := false
	for _, m := range a.working[before:] {
		if strings.Contains(m.Content, "Prediction mismatch") && strings.Contains(m.Content, "fs_write") {
			guided = true
		}
	}
	if !guided {
		t.Fatal("mismatch must render structured guidance naming the strategy")
	}

	// Match resets the meter and discharges the hypothesis.
	if missed := a.predictionObserve(context.Background(), "fs_write", map[string]interface{}{"path": "a.txt"},
		"file written cleanly", "wrote 42 bytes to a.txt", false); missed {
		t.Fatal("matching outcome must not count as missed")
	}
	if a.mismatchMeter["fs_write"] != 0 {
		t.Fatalf("meter = %d after match, want 0", a.mismatchMeter["fs_write"])
	}

	// Expectation-less non-probe: outside the discipline entirely.
	if missed := a.predictionObserve(context.Background(), "todo", nil, "", "error: boom", true); missed {
		t.Fatal("expectation-less non-probe must not be treated as a missed probe")
	}
}
