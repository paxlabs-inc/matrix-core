// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// intent_cost.go — cost telemetry helpers for sess#32 ambient-architect.
//
// The MatrixGateway (gateway/) responds with X-Matrix-Cost-Pax +
// X-Matrix-Daily-Spent-Pax + X-Matrix-Daily-Remaining-Pax on every
// metered LLM call. Daemons capture those via the
// llm.Config.OnResponseHeaders hook (MCL/llm/llm.go) and route them
// here.
//
// Two surfaces are produced per call:
//
//  1. A `transcript.intent.cost` audit event. Plan §5.11.
//  2. A Prometheus counter increment on
//     matrix_daemon_cost_pax_total{slot,kind_route,goal}.
//
// At intent terminal, callers may also call `aggregateIntentCost` to
// roll up into a single cortex Event memory tagged "cost". v1 keeps
// the cortex aggregation as a TODO (cortex.Add API surface is
// daemon-internal and threaded through too many layers to wire
// cleanly here without crossing into cortex/). The transcript +
// Prometheus surfaces are what /metrics + the Inbox UI consume in v1.
//
// Concurrency: every helper is safe for concurrent use. The cost
// accumulator (used by aggregate) takes a sync.Mutex around updates.

import (
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header names mirror gateway/internal/types verbatim. The executor
// module deliberately does NOT import the gateway module (cross-module
// dep would force a `replace` directive); we re-state the small set of
// constants the daemon needs. If the gateway header surface changes,
// this and gateway/internal/types/types.go MUST land in lockstep.
const (
	costHeaderCostPax           = "X-Matrix-Cost-Pax"
	costHeaderDailySpentPax     = "X-Matrix-Daily-Spent-Pax"
	costHeaderDailyRemainingPax = "X-Matrix-Daily-Remaining-Pax"
	costHeaderRateTableVersion  = "X-Matrix-Rate-Table-Version"
)

// intentCostEvent is the structured payload of an `intent.cost`
// transcript event. Fields mirror plan §5.11 verbatim plus the
// daily-spend trailer that the gateway response carries.
type intentCostEvent struct {
	Kind             string `json:"kind"`
	Ts               string `json:"ts"`
	IntentID         string `json:"intent_id,omitempty"`
	GoalID           string `json:"goal_id,omitempty"`
	Model            string `json:"model"`
	Slot             string `json:"slot,omitempty"`
	KindRoute        string `json:"kind_route,omitempty"`
	TokensInput      int    `json:"tokens_input,omitempty"`
	TokensOutput     int    `json:"tokens_output,omitempty"`
	CostPax          string `json:"cost_pax,omitempty"`
	DailySpentPax    string `json:"daily_spent_pax,omitempty"`
	DailyRemainPax   string `json:"daily_remaining_pax,omitempty"`
	RateTableVersion int    `json:"rate_table_version,omitempty"`
}

// intentCostMeta packages the call-site context the cost recorder
// needs. Stays a value-type so daemon goroutines pass it cheaply.
type intentCostMeta struct {
	IntentID  string
	GoalID    string
	Model     string
	Slot      string
	KindRoute string
	Tokens    intentCostTokens
}

type intentCostTokens struct {
	Input  int
	Output int
}

// recordIntentCost is the daemon-side wrapper invoked from llm.Config
// .OnResponseHeaders. Reads the X-Matrix-Cost-Pax + spent/remain
// headers off the upstream response, stamps an `intent.cost` event,
// and increments the Prometheus counter on routerMetrics.
//
// Safe to call when h is empty (BYO calls don't carry cost headers);
// the function then no-ops without touching the transcript or the
// metrics counter so the audit stream stays clean of zero-cost rows.
func recordIntentCost(t *transcript, m *routerMetrics, h http.Header, meta intentCostMeta) {
	if t == nil || h == nil {
		return
	}
	cost := strings.TrimSpace(h.Get(costHeaderCostPax))
	if cost == "" {
		return
	}
	rateV := 0
	if rv := h.Get(costHeaderRateTableVersion); rv != "" {
		if n, err := strconv.Atoi(rv); err == nil {
			rateV = n
		}
	}
	ev := intentCostEvent{
		Kind:             "intent.cost",
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		IntentID:         meta.IntentID,
		GoalID:           meta.GoalID,
		Model:            meta.Model,
		Slot:             meta.Slot,
		KindRoute:        meta.KindRoute,
		TokensInput:      meta.Tokens.Input,
		TokensOutput:     meta.Tokens.Output,
		CostPax:          cost,
		DailySpentPax:    h.Get(costHeaderDailySpentPax),
		DailyRemainPax:   h.Get(costHeaderDailyRemainingPax),
		RateTableVersion: rateV,
	}
	t.Event("intent.cost", "cost", map[string]interface{}{
		"intent_id":           ev.IntentID,
		"goal_id":             ev.GoalID,
		"model":               ev.Model,
		"slot":                ev.Slot,
		"kind_route":          ev.KindRoute,
		"tokens_input":        ev.TokensInput,
		"tokens_output":       ev.TokensOutput,
		"cost_pax":            ev.CostPax,
		"daily_spent_pax":     ev.DailySpentPax,
		"daily_remaining_pax": ev.DailyRemainPax,
		"rate_table_version":  ev.RateTableVersion,
	})
	if m != nil {
		m.AddCost(costMetricKey{
			Slot:      meta.Slot,
			KindRoute: meta.KindRoute,
			GoalID:    meta.GoalID,
		}, cost)
	}
}

// intentCostAccumulator is a per-intent rollup. The lifecycle driver
// owns one per active intent + flushes a single aggregate event on
// terminal (`intent.cost.summary` style — actual aggregation into
// cortex Event memories is plan §5.11 deferred work).
//
// v1 surface: callers track the per-intent total in the daemon
// process; UI reads it via the transcript stream. v2 will fold the
// total into a cortex Event memory tagged "cost".
type intentCostAccumulator struct {
	mu       sync.Mutex
	intentID string
	goalID   string
	totalPax string // canonical decimal-string sum; "" before first record
	calls    int
}

// newIntentCostAccumulator constructs an accumulator for one intent.
func newIntentCostAccumulator(intentID, goalID string) *intentCostAccumulator {
	return &intentCostAccumulator{intentID: intentID, goalID: goalID}
}

// Add folds one cost-pax value into the running total. costPax is the
// canonical decimal-string form ("0.000123000000"); empty / "0" inputs
// are no-ops.
func (a *intentCostAccumulator) Add(costPax string) {
	if a == nil || costPax == "" || costPax == "0" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.totalPax == "" {
		a.totalPax = costPax
		return
	}
	// We import gateway/internal/rates lazily via the callerless path
	// to keep this file's import set tight. The actual sum is
	// performed by the helper below to avoid threading rates through
	// every daemon code path.
	if sum, err := paxAdd(a.totalPax, costPax); err == nil {
		a.totalPax = sum
	}
}

// Snapshot returns (totalPax, callCount). Empty totalPax means
// no cost was recorded.
func (a *intentCostAccumulator) Snapshot() (string, int) {
	if a == nil {
		return "", 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totalPax, a.calls
}

// EmitTerminal writes a single `intent.cost.summary` audit event with
// the aggregated total. Called from the lifecycle driver on
// successful terminal envelopes. Plan §5.11 also calls for a cortex
// Event memory tagged "cost"; v1 emits the transcript event only,
// the cortex write is staged for the next pass (cortex.Add wiring
// crosses module boundaries that require additional plumbing).
func (a *intentCostAccumulator) EmitTerminal(t *transcript) {
	if a == nil || t == nil {
		return
	}
	total, calls := a.Snapshot()
	if total == "" {
		return
	}
	t.Event("intent.cost.summary", "cost", map[string]interface{}{
		"intent_id": a.intentID,
		"goal_id":   a.goalID,
		"total_pax": total,
		"calls":     calls,
	})
}

// makeCostHook returns an llm.Config.OnResponseHeaders compatible
// closure that pipes the response headers into recordIntentCost +
// the supplied accumulator. Used by compile.go / synthesize.go /
// step_handler.go so each routed-LLM call site shares the same
// cost-capture surface.
//
// The closure is safe for concurrent use (recordIntentCost guards
// transcript + metrics writes; accumulator has its own mutex).
func makeCostHook(t *transcript, m *routerMetrics, acc *intentCostAccumulator, meta intentCostMeta) func(http.Header) {
	return func(h http.Header) {
		recordIntentCost(t, m, h, meta)
		if acc != nil {
			if cost := strings.TrimSpace(h.Get(costHeaderCostPax)); cost != "" {
				acc.Add(cost)
			}
		}
	}
}

// paxAdd sums two PAX-denominated decimal strings using big.Float math
// (mirrors gateway/internal/rates.AddPax). 256-bit precision keeps
// rounding error well below NUMERIC(20,12) least-significant digit.
// Output is fixed-12 decimal so the daemon-side accumulator's
// snapshot is stable across calls.
func paxAdd(a, b string) (string, error) {
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	const prec = uint(256)
	af, _, err := big.ParseFloat(strings.TrimSpace(a), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("intent_cost: parse %q: %w", a, err)
	}
	bf, _, err := big.ParseFloat(strings.TrimSpace(b), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("intent_cost: parse %q: %w", b, err)
	}
	sum := new(big.Float).SetPrec(prec).Add(af, bf)
	return sum.Text('f', 12), nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
