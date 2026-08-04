// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/o1"
)

// TestEpistemicMismatchDoesNotInterceptDispatch proves the ordinary-execution
// boundary on a REAL non-converging run —
// real httptest SSE model, the real spawned MCP fetch tool probing a real
// 404-only API, every production bound at its default. Prediction mismatch
// remains observable, Cassandra may still annotate the working turn, and the
// hard no-progress failsafe remains authoritative, but no tools-stripped
// forced-revision call may intercept dispatch.
func TestEpistemicMismatchDoesNotInterceptDispatch(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)

	probe := func(id, path, expect string) string {
		args, _ := json.Marshal(map[string]string{"url": api.URL + path, "expect": expect})
		return fmt.Sprintf(`{"index":0,"id":%q,"type":"function","function":{"name":"fetch__fetch","arguments":%q}}`, id, string(args))
	}
	// Distinct wording per probe so the probing phase carries NO repeat signal
	// (the epistemic meter, not the repeat read, must be what fires first).
	probes := []string{
		probe("p1", "/ohlc", "json array of ohlc candles"),
		probe("p2", "/candles", "rows shaped like market history entries"),
		probe("p3", "/klines", "kline series payload grouped per interval"),
	}
	spiral := func(n int) string { return probe(fmt.Sprintf("s%d", n), "/ohlc", "json array of ohlc candles") }

	var (
		mu       sync.Mutex
		timeline []string
		mainCall int
	)
	record := func(ev string) {
		mu.Lock()
		timeline = append(timeline, ev)
		mu.Unlock()
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.Contains(body, "load-bearing FACTUAL PREMISES"):
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		case strings.Contains(body, "stated expectation"):
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"mismatch"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		case !strings.Contains(body, `"tools"`):
			// The forced revision step is tools-stripped — the epistemic layer
			// firing IS this request's shape.
			record("revision")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Revised plan: the probes keep missing; I will keep investigating the endpoint."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		mu.Lock()
		mainCall++
		call := mainCall
		mu.Unlock()
		var tc string
		if call <= len(probes) {
			tc = probes[call-1]
		} else {
			tc = spiral(call)
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	client := revisionTestClient(t, model.URL)
	cfg := config.Default() // every bound at its production default, Cassandra ON
	a := New(Options{
		Config: cfg,
		Main:   client,
		Cheap:  client,
		Tools:  replayFetchManager(t),
		AuditObserver: func(ev AuditEvent) {
			if ev.Type == auditEventMod {
				record("casmod")
			}
		},
	})

	err := a.Chat(context.Background(), "find the current OHLC candles on this undocumented API")
	if err == nil || !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "repeating the same step") {
		t.Fatalf("the non-converging run must end on the hard stall failsafe, got: %v", err)
	}

	mu.Lock()
	events := append([]string(nil), timeline...)
	mu.Unlock()

	firstRevision, firstMod := -1, -1
	for i, ev := range events {
		if ev == "revision" && firstRevision < 0 {
			firstRevision = i
		}
		if ev == "casmod" && firstMod < 0 {
			firstMod = i
		}
	}
	if firstRevision >= 0 {
		t.Fatalf("prediction mismatch injected a tools-stripped revision call; timeline: %v", events)
	}
	if firstMod < 0 {
		t.Fatalf("the Cassandra voice never fired; timeline: %v", events)
	}

	// req.5.4 — the death taxonomy is the governor's terminal verdict and the
	// failure-context round-trip holds: the structured record carries the real
	// stall state, and the supervisor-facing StateLine renders it.
	death, ok := a.LastDeath()
	if !ok {
		t.Fatal("the governor's terminal verdict must record the structured death")
	}
	if death.Reason != DeathReasonStall {
		t.Fatalf("death reason = %q, want %q", death.Reason, DeathReasonStall)
	}
	if death.RepeatCount < cfg.NoProgressStall {
		t.Fatalf("death must capture the real stall state: repeats=%d, want >= %d", death.RepeatCount, cfg.NoProgressStall)
	}
	line := death.StateLine()
	for _, want := range []string{"reason=no_progress_stall", "last_tool=fetch__fetch", "faculty=conversation"} {
		if !strings.Contains(line, want) {
			t.Fatalf("failure-context StateLine missing %q:\n%s", want, line)
		}
	}
}

// TestGovernorDeath_OneTerminalVerdictSite pins the structural half of
// req.5.4: recordDeath (the death record) and consolidateWorking on death
// paths are reached ONLY through governDeath — one place decides and records
// why a turn ended. recordDeath's only other production caller would be
// duplicate death bookkeeping.
func TestGovernorDeath_OneTerminalVerdictSite(t *testing.T) {
	sites := productionCallSites(t, "a.recordDeath(")
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "governor.go:") {
		t.Fatalf("recordDeath must be called only from governDeath (governor.go), found %v", sites)
	}
	deathSites := productionCallSites(t, "a.governDeath(")
	if len(deathSites) != 4 {
		t.Fatalf("stall, step budget, close-retry cap, and synthesis retry must all route through governDeath, found %v", deathSites)
	}
}

func TestGovernDeathPreservesSuccessfulOperationStanding(t *testing.T) {
	a := New(Options{Config: config.Default()})
	success := o1.NewOutcome("write_project").Success().
		Evidence("project files were written and verified").
		PostSatisfied("requested files exist").MustBuild()
	a.turn.commitOutcome(success)
	a.turn.runLedger = o1.NewRunLedger("run-success", "contract", 1)

	err := a.governDeath(DeathReasonSynthesis, "final synthesis retry exhausted. Progress so far:")
	if err == nil || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("governDeath = %v, want resumable ErrIncomplete", err)
	}
	standing := a.turn.outcome()
	if standing == nil || !standing.Success || standing.Operation != "write_project" {
		t.Fatalf("successful standing was overwritten: %+v", standing)
	}
	if attempts := a.turn.runLedger.Attempts; len(attempts) != 0 {
		t.Fatalf("turn death fabricated an operation attempt: %+v", attempts)
	}
}
