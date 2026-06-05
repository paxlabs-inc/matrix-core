// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// router_metrics.go — per-route latency histograms + router-decision
// audit helpers (Session 31d · P4 observability).
//
// Three observables land in this file:
//
//   1. router.decision — fired on every routed LLM call site (compile,
//      synth, step) with the resolved slot/kind/model + whether the
//      delivery came from cache, was streamed, or fell back to
//      legacy. Single source of truth for "which route was used per
//      intent".
//
//   2. routerMetrics.Observe — accumulator that buckets every routed
//      decode's wall-clock latency into a fixed-grid Prometheus
//      histogram. Aggregation lives in-process; flushed periodically
//      to the transcript JSONL as `router.histogram` events and
//      served wholesale on /metrics in Prometheus exposition format.
//
//   3. routerCounters — cumulative success/error counts per route,
//      compile-cache hit/miss counters, used by both the JSONL flush
//      and the /metrics handler.
//
// Sidecar posture (mirrors meta/salience_weights Phase 12): the
// metrics state lives ENTIRELY in-process. Never journaled, never
// folded into cortex OverallRoot, never persisted. A daemon restart
// resets counters to zero — operators relying on long-window
// aggregation should scrape /metrics into a real Prometheus instance.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------
// Router decision audit
// ---------------------------------------------------------------------

// routerDecision describes one routed LLM call. Fired from compile,
// synth, and step handler at the point the route resolves.
type routerDecision struct {
	Slot     string // "compiler" | "planner" | "executor"
	Kind     string // executor only: "reason" | "code" | ... (empty for compiler/planner)
	Model    string // resolved model id
	IntentID string // optional; populated when known (compile/synth always know it)
	NodeID   string // optional; executor step calls
	LongCtx  bool
	Streamed bool
	CacheHit bool
	Reason   string // free-form audit note ("compiler.slot.resolve", "compile.cache.hit", ...)
}

// recordRouterDecision emits a router.decision audit event into the
// transcript. Idempotent + non-fatal: a nil transcript silently
// drops the event so test code-paths that don't wire a transcript
// stay simple.
func recordRouterDecision(t *transcript, d routerDecision) {
	if t == nil {
		return
	}
	fields := map[string]interface{}{
		"slot":      d.Slot,
		"kind":      d.Kind,
		"model":     d.Model,
		"long_ctx":  d.LongCtx,
		"streamed":  d.Streamed,
		"cache_hit": d.CacheHit,
		"reason":    d.Reason,
	}
	if d.IntentID != "" {
		fields["intent_id"] = d.IntentID
	}
	if d.NodeID != "" {
		fields["node_id"] = d.NodeID
	}
	t.Event("router.decision", "router", fields)
}

// ---------------------------------------------------------------------
// Histogram buckets (Prometheus default-ish, ms-scale)
// ---------------------------------------------------------------------

// histogramBucketsMs is the bucket upper-bound grid for routed-LLM
// latency observations. Same shape as Prometheus's
// prometheus.DefBuckets but rescaled to milliseconds, with the upper
// tail extended to 60s to cover slow tool-augmented executor steps.
// Sorted ascending; the synthetic +Inf bucket is implicit (every
// observation lands at-or-below some bucket OR in the +Inf overflow).
var histogramBucketsMs = []float64{
	1, 5, 10, 25, 50, 100, 250, 500,
	1000, 2500, 5000, 10000, 30000, 60000,
}

// ---------------------------------------------------------------------
// routerMetrics: per-route histogram accumulator
// ---------------------------------------------------------------------

// routeMetricKey identifies one histogram series. Kept value-type so
// it's usable as a map key. NodeID is intentionally NOT here — too
// high cardinality; node-level latency lives in the per-event audit
// stream only.
type routeMetricKey struct {
	Slot     string // "compiler" | "planner" | "executor"
	Kind     string // executor only; "" for compiler/planner
	Model    string
	Streamed bool
}

// routeHistogram tracks one (route, model, streamed) series. Counts +
// sum + the bucket array (len == len(histogramBucketsMs) + 1 for the
// +Inf overflow). Updated under routerMetrics.mu.
type routeHistogram struct {
	Count   uint64
	SumMs   float64
	Buckets []uint64 // cumulative? no — per-bucket counts; aggregator computes le-cumulative on emit
	Errors  uint64
}

// costMetricKey identifies one matrix_daemon_cost_pax_total series
// (sess#32 ambient-architect MatrixGateway · plan §5.11 / §5.15). The
// gateway's response header carries a per-call PAX cost; the daemon
// folds it into a cumulative counter labeled by (slot, kind_route,
// goal). Empty fields collapse cleanly into empty Prometheus labels
// so non-routed call sites still aggregate without proliferating
// series.
type costMetricKey struct {
	Slot      string // "compiler" | "planner" | "executor" | "" (no slot known)
	KindRoute string // executor sub-route: "reason" | "code" | …; "" for non-executor
	GoalID    string // optional: chat-driven runs carry it; CLI one-shots leave empty
}

// routerMetrics aggregates per-route latency histograms + counters.
// Safe for concurrent use under mu; designed for the daemon's
// SSE/HTTP fan-in where every routed decode can fire from any
// goroutine.
type routerMetrics struct {
	mu sync.Mutex

	// routes maps the (Slot, Kind, Model, Streamed) tuple to its
	// histogram + counters.
	routes map[routeMetricKey]*routeHistogram

	// Compile-cache counters (no route key needed — there's only one
	// compile-cache surface per daemon).
	cacheHits   uint64
	cacheMisses uint64

	// costs aggregates cumulative PAX spend per
	// (slot, kind_route, goal) tuple. Stored as a canonical
	// decimal-string with 12 fractional digits (mirrors
	// gateway/internal/rates.AddPax) so the value survives one-time
	// reconciliation against the Postgres credit_ledger NUMERIC(20,12)
	// column. Empty value => "0".
	costs map[costMetricKey]string
}

// newRouterMetrics constructs an empty accumulator.
func newRouterMetrics() *routerMetrics {
	return &routerMetrics{
		routes: make(map[routeMetricKey]*routeHistogram),
		costs:  make(map[costMetricKey]string),
	}
}

// Observe records one routed-decode latency observation. err == nil
// means success; non-nil increments the per-route error counter
// alongside the histogram bucket so /metrics can report a clean
// error-rate. ms < 0 is treated as 0 (defensive against clock skew /
// monotonic-clock weirdness in CI under -race).
func (m *routerMetrics) Observe(key routeMetricKey, ms int64, err error) {
	if m == nil {
		return
	}
	if ms < 0 {
		ms = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.routes[key]
	if !ok {
		h = &routeHistogram{
			Buckets: make([]uint64, len(histogramBucketsMs)+1),
		}
		m.routes[key] = h
	}
	h.Count++
	h.SumMs += float64(ms)
	if err != nil {
		h.Errors++
	}
	// Find first bucket whose upper bound >= ms. The trailing slot
	// is the +Inf overflow.
	idx := len(histogramBucketsMs)
	for i, ub := range histogramBucketsMs {
		if float64(ms) <= ub {
			idx = i
			break
		}
	}
	h.Buckets[idx]++
}

// IncCacheHit bumps the compile-cache hit counter.
func (m *routerMetrics) IncCacheHit() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cacheHits++
	m.mu.Unlock()
}

// IncCacheMiss bumps the compile-cache miss counter.
func (m *routerMetrics) IncCacheMiss() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cacheMisses++
	m.mu.Unlock()
}

// AddCost folds one PAX-denominated cost (decimal-string, e.g.
// "0.000123000000") into the cumulative counter keyed on key. Empty
// or "0" inputs are no-ops so /metrics never carries phantom rows
// for BYO calls that bypass the gateway. Concurrency-safe via mu.
//
// Sess#32 ambient-architect cost telemetry (plan §5.11 / §5.15).
func (m *routerMetrics) AddCost(key costMetricKey, costPax string) {
	if m == nil {
		return
	}
	costPax = strings.TrimSpace(costPax)
	if costPax == "" || costPax == "0" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.costs[key]
	if !ok || cur == "" {
		m.costs[key] = costPax
		return
	}
	if sum, err := paxAdd(cur, costPax); err == nil {
		m.costs[key] = sum
	}
}

// CostSnapshot returns a copy of the (key → totalPax) map suitable
// for /metrics emission. Stable iteration is the caller's
// responsibility.
func (m *routerMetrics) CostSnapshot() map[costMetricKey]string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[costMetricKey]string, len(m.costs))
	for k, v := range m.costs {
		out[k] = v
	}
	return out
}

// CacheStats returns a (hits, misses) snapshot.
func (m *routerMetrics) CacheStats() (hits, misses uint64) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cacheHits, m.cacheMisses
}

// snapshot returns a deep-copied per-route summary suitable for
// JSONL emission or /metrics exposition. The returned slice is
// sorted by (Slot, Kind, Model, Streamed) for deterministic output.
type histogramSnapshot struct {
	Slot      string   `json:"slot"`
	Kind      string   `json:"kind,omitempty"`
	Model     string   `json:"model"`
	Streamed  bool     `json:"streamed"`
	Count     uint64   `json:"count"`
	SumMs     float64  `json:"sum_ms"`
	Errors    uint64   `json:"errors"`
	BucketsMs []uint64 `json:"buckets"` // per-bucket counts; aligns to histogramBucketsMs + 1 (+Inf)
}

func (m *routerMetrics) snapshot() []histogramSnapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]histogramSnapshot, 0, len(m.routes))
	for k, h := range m.routes {
		buckets := make([]uint64, len(h.Buckets))
		copy(buckets, h.Buckets)
		out = append(out, histogramSnapshot{
			Slot:      k.Slot,
			Kind:      k.Kind,
			Model:     k.Model,
			Streamed:  k.Streamed,
			Count:     h.Count,
			SumMs:     h.SumMs,
			Errors:    h.Errors,
			BucketsMs: buckets,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		// Stable order on Streamed: false before true.
		return !out[i].Streamed && out[j].Streamed
	})
	return out
}

// Flush emits a single router.histogram audit event into the
// transcript carrying the current snapshot. Safe to call repeatedly;
// counters are NOT reset (Prometheus convention: counters are
// monotonic; resets only happen on process restart). Returns the
// total number of routes flushed for caller-side smoke logging.
func (m *routerMetrics) Flush(t *transcript) int {
	if m == nil || t == nil {
		return 0
	}
	snap := m.snapshot()
	hits, misses := m.CacheStats()
	t.Event("router.histogram", "router", map[string]interface{}{
		"buckets_ms":         histogramBucketsMs,
		"routes":             snap,
		"compile_cache_hits": hits,
		"compile_cache_miss": misses,
	})
	return len(snap)
}

// ---------------------------------------------------------------------
// Prometheus exposition
// ---------------------------------------------------------------------

// writePrometheus writes a Prometheus-compat text exposition of the
// current metrics snapshot to w. Stable bucket-label set (le=...),
// stable label ordering, and HELP/TYPE preambles per series.
//
// See https://prometheus.io/docs/instrumenting/exposition_formats/#text-based-format
// for the wire format. Each metric:
//
//	# HELP <name> <description>
//	# TYPE <name> <kind>
//	<name>{labels} <value>
//
// Counters are monotonic uint64; histograms expose _bucket / _sum /
// _count families with stable label sets.
func (m *routerMetrics) writePrometheus(w *strings.Builder, uptimeSec int64) {
	if m == nil {
		return
	}
	snap := m.snapshot()
	hits, misses := m.CacheStats()

	fmt.Fprintf(w, "# HELP matrix_daemon_up Whether the matrix daemon is up (always 1 when scraped).\n")
	fmt.Fprintf(w, "# TYPE matrix_daemon_up gauge\n")
	fmt.Fprintf(w, "matrix_daemon_up 1\n")

	fmt.Fprintf(w, "# HELP matrix_daemon_uptime_seconds Seconds since the daemon booted.\n")
	fmt.Fprintf(w, "# TYPE matrix_daemon_uptime_seconds gauge\n")
	fmt.Fprintf(w, "matrix_daemon_uptime_seconds %d\n", uptimeSec)

	fmt.Fprintf(w, "# HELP matrix_compile_cache_hits_total Total compile-cache hits since daemon start.\n")
	fmt.Fprintf(w, "# TYPE matrix_compile_cache_hits_total counter\n")
	fmt.Fprintf(w, "matrix_compile_cache_hits_total %d\n", hits)

	fmt.Fprintf(w, "# HELP matrix_compile_cache_misses_total Total compile-cache misses since daemon start.\n")
	fmt.Fprintf(w, "# TYPE matrix_compile_cache_misses_total counter\n")
	fmt.Fprintf(w, "matrix_compile_cache_misses_total %d\n", misses)

	// Single histogram family: matrix_router_request_duration_ms.
	// One series per (slot, kind, model, streamed) label tuple.
	fmt.Fprintf(w, "# HELP matrix_router_request_duration_ms Routed LLM decode latency in ms, by route key.\n")
	fmt.Fprintf(w, "# TYPE matrix_router_request_duration_ms histogram\n")
	for _, s := range snap {
		labels := promLabelTuple(s.Slot, s.Kind, s.Model, s.Streamed)
		// Bucket cumulative emission: Prometheus convention is
		// le="<upper>" with CUMULATIVE counts ending at +Inf.
		var cum uint64
		for i, ub := range histogramBucketsMs {
			cum += s.BucketsMs[i]
			fmt.Fprintf(w, "matrix_router_request_duration_ms_bucket{%s,le=\"%s\"} %d\n",
				labels, formatFloat(ub), cum)
		}
		cum += s.BucketsMs[len(histogramBucketsMs)] // +Inf overflow bucket
		fmt.Fprintf(w, "matrix_router_request_duration_ms_bucket{%s,le=\"+Inf\"} %d\n",
			labels, cum)
		fmt.Fprintf(w, "matrix_router_request_duration_ms_sum{%s} %s\n",
			labels, formatFloat(s.SumMs))
		fmt.Fprintf(w, "matrix_router_request_duration_ms_count{%s} %d\n",
			labels, s.Count)
	}

	// Per-route error counter (independent series so dashboards can
	// alert on errors_total without parsing histogram internals).
	fmt.Fprintf(w, "# HELP matrix_router_request_errors_total Total LLM decode errors, by route key.\n")
	fmt.Fprintf(w, "# TYPE matrix_router_request_errors_total counter\n")
	for _, s := range snap {
		labels := promLabelTuple(s.Slot, s.Kind, s.Model, s.Streamed)
		fmt.Fprintf(w, "matrix_router_request_errors_total{%s} %d\n",
			labels, s.Errors)
	}

	// Cumulative PAX spend per (slot, kind_route, goal) tuple
	// (sess#32 ambient-architect · plan §5.11 / §5.15). Counter
	// monotonic; resets only on daemon restart. Emitted in stable
	// label order so /metrics scrapes are diff-clean.
	fmt.Fprintf(w, "# HELP matrix_daemon_cost_pax_total Cumulative PAX spent on routed LLM calls, by slot/kind_route/goal.\n")
	fmt.Fprintf(w, "# TYPE matrix_daemon_cost_pax_total counter\n")
	costSnap := m.CostSnapshot()
	keys := make([]costMetricKey, 0, len(costSnap))
	for k := range costSnap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Slot != keys[j].Slot {
			return keys[i].Slot < keys[j].Slot
		}
		if keys[i].KindRoute != keys[j].KindRoute {
			return keys[i].KindRoute < keys[j].KindRoute
		}
		return keys[i].GoalID < keys[j].GoalID
	})
	for _, k := range keys {
		fmt.Fprintf(w, "matrix_daemon_cost_pax_total{slot=%q,kind_route=%q,goal=%q} %s\n",
			k.Slot, k.KindRoute, k.GoalID, costSnap[k])
	}
}

// promLabelTuple returns a stable label set for one route.
// kind="" is preserved as kind="" in the output (Prometheus
// considers empty labels equivalent to absent, which collapses the
// compiler/planner series into one bucket — desired).
func promLabelTuple(slot, kind, model string, streamed bool) string {
	streamedStr := "false"
	if streamed {
		streamedStr = "true"
	}
	return fmt.Sprintf(`slot=%q,kind=%q,model=%q,streamed=%q`,
		slot, kind, model, streamedStr)
}

// formatFloat strips trailing zeros from a float for the le="..."
// label so bucket upper bounds render as "1", "5", "10" instead of
// "1.000000" — matches Prometheus default histogram output.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
