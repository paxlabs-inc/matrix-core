// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_metrics.go — GET /metrics (Prometheus-compatible exposition).
//
// Session 31d · P4 deliverable. Serves the daemon's routerMetrics
// snapshot in the standard text-based Prometheus format, plus a
// couple of process-level gauges (uptime, SSE broker stats, busy
// flag).
//
// Authorisation posture: /metrics is PUBLIC by default — Prometheus
// scrapers usually run as a separate process inside the same trust
// boundary as the daemon (local sidecar / VPN-walled host). When the
// daemon has authToken configured, scrapers must still pass the
// bearer; this mirrors /healthz which is also bearer-protected when
// auth is on.
//
// Content type: text/plain; version=0.0.4 (Prometheus convention).

import (
	"net/http"
	"strings"
	"time"
)

// handleMetrics serves GET /metrics. Method-not-allowed for non-GET.
// Falls back gracefully when metrics aren't attached (early-boot or
// CLI mode) — emits the process-level gauges only.
func (d *daemonState) handleMetrics(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !d.requireAuth(w, r) {
			return
		}

		uptimeSec := int64(time.Since(d.startedAt).Seconds())
		subs, pub, drop := d.broker.Stats()

		var sb strings.Builder
		sb.Grow(4096)

		// Process / SSE broker gauges. These render even without a
		// metrics accumulator attached, so /metrics is useful from
		// the moment the daemon binds the listener.
		writeProcessMetrics(&sb, uptimeSec, subs, pub, drop, d.tryProbeBusy())

		// Router metrics (when attached).
		if m := t.Metrics(); m != nil {
			m.writePrometheus(&sb, uptimeSec)
		} else {
			writeNoRouterMetricsNote(&sb)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sb.String()))
	}
}

// writeProcessMetrics emits the always-on process-level gauges.
// SSE counters mirror /healthz fields so dashboards can graph live
// subscriber count without an extra scrape target.
func writeProcessMetrics(sb *strings.Builder, uptimeSec int64,
	subs int, pub, drop uint64, busy bool) {

	// matrix_daemon_up: 1 when scraped (defensive: the routerMetrics
	// path also emits this when attached; here is the cold fallback).
	sb.WriteString("# HELP matrix_daemon_up Whether the matrix daemon is up (always 1 when scraped).\n")
	sb.WriteString("# TYPE matrix_daemon_up gauge\n")
	sb.WriteString("matrix_daemon_up 1\n")

	sb.WriteString("# HELP matrix_daemon_uptime_seconds Seconds since the daemon booted.\n")
	sb.WriteString("# TYPE matrix_daemon_uptime_seconds gauge\n")
	sb.WriteString("matrix_daemon_uptime_seconds ")
	sb.WriteString(itoa(uptimeSec))
	sb.WriteString("\n")

	sb.WriteString("# HELP matrix_sse_subscribers Current live SSE subscriber count.\n")
	sb.WriteString("# TYPE matrix_sse_subscribers gauge\n")
	sb.WriteString("matrix_sse_subscribers ")
	sb.WriteString(itoa(int64(subs)))
	sb.WriteString("\n")

	sb.WriteString("# HELP matrix_sse_published_total Total SSE events published since daemon start.\n")
	sb.WriteString("# TYPE matrix_sse_published_total counter\n")
	sb.WriteString("matrix_sse_published_total ")
	sb.WriteString(utoa(pub))
	sb.WriteString("\n")

	sb.WriteString("# HELP matrix_sse_dropped_total Total SSE events dropped due to subscriber back-pressure.\n")
	sb.WriteString("# TYPE matrix_sse_dropped_total counter\n")
	sb.WriteString("matrix_sse_dropped_total ")
	sb.WriteString(utoa(drop))
	sb.WriteString("\n")

	sb.WriteString("# HELP matrix_daemon_busy 1 when a /messages call is in flight (single-flight gate held).\n")
	sb.WriteString("# TYPE matrix_daemon_busy gauge\n")
	if busy {
		sb.WriteString("matrix_daemon_busy 1\n")
	} else {
		sb.WriteString("matrix_daemon_busy 0\n")
	}
}

// writeNoRouterMetricsNote emits a single-line marker so an operator
// can tell the difference between "router metrics zero observations"
// and "router metrics not wired". Helpful during P4 rollout.
func writeNoRouterMetricsNote(sb *strings.Builder) {
	sb.WriteString("# HELP matrix_router_metrics_attached 1 when the router metrics accumulator is wired into the transcript.\n")
	sb.WriteString("# TYPE matrix_router_metrics_attached gauge\n")
	sb.WriteString("matrix_router_metrics_attached 0\n")
}

// itoa / utoa: tiny no-alloc int-to-string helpers so /metrics
// doesn't pull strconv into the hot path. The Prometheus exposition
// spec accepts decimal integers + IEEE 754 floats in standard
// notation; we only need decimal here.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
