// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"sort"
	"sync"
	"time"
)

// Meter is the lane's measurement. The owner's constraint for routing finance
// through the router was to keep it measured and monitored, and this is the
// thing that makes that true: every call is counted by provider and endpoint,
// with cache effectiveness, upstream health and latency alongside.
//
// It holds counters only — no user content, no symbols-per-user history, no
// vendor payloads.
type Meter struct {
	mu     sync.Mutex
	rows   map[meterKey]*meterRow
	users  map[string]int64
	now    func() time.Time
	since  time.Time
	sample int
}

type meterKey struct {
	Provider Provider
	Endpoint string
}

type meterRow struct {
	Requests    int64
	CacheHits   int64
	Upstream    int64
	Errors      int64
	Throttles   int64
	NotFound    int64
	Fallbacks   int64
	StaleServed int64
	latencies   []time.Duration
}

// NewMeter builds a meter. now is the clock seam.
func NewMeter(now func() time.Time) *Meter {
	if now == nil {
		now = time.Now
	}
	return &Meter{
		rows:   map[meterKey]*meterRow{},
		users:  map[string]int64{},
		now:    now,
		since:  now().UTC(),
		sample: 512,
	}
}

// Record is one served finance call.
type Record struct {
	Provider Provider
	Endpoint string
	// User is the requesting subject. It is counted, never stored per call.
	User     string
	CacheHit bool
	Stale    bool
	Fallback bool
	Latency  time.Duration
	Err      error
}

// Observe folds one served call into the counters.
func (m *Meter) Observe(rec Record) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key := meterKey{Provider: rec.Provider, Endpoint: rec.Endpoint}
	row, ok := m.rows[key]
	if !ok {
		row = &meterRow{}
		m.rows[key] = row
	}
	row.Requests++
	if rec.CacheHit {
		row.CacheHits++
	} else {
		row.Upstream++
	}
	if rec.Stale {
		row.StaleServed++
	}
	if rec.Fallback {
		row.Fallbacks++
	}
	switch KindOf(rec.Err) {
	case "":
	case FailureThrottled:
		row.Throttles++
		row.Errors++
	case FailureNotFound:
		row.NotFound++
	default:
		row.Errors++
	}
	if rec.Latency > 0 {
		row.latencies = append(row.latencies, rec.Latency)
		if len(row.latencies) > m.sample {
			row.latencies = row.latencies[len(row.latencies)-m.sample:]
		}
	}
	if rec.User != "" {
		m.users[rec.User]++
	}
}

// EndpointStats is one provider/endpoint row of the diagnostics snapshot.
type EndpointStats struct {
	Provider     Provider `json:"provider"`
	Endpoint     string   `json:"endpoint"`
	Requests     int64    `json:"requests"`
	CacheHits    int64    `json:"cache_hits"`
	UpstreamCall int64    `json:"upstream_calls"`
	Errors       int64    `json:"errors"`
	Throttles    int64    `json:"throttles"`
	NotFound     int64    `json:"not_found"`
	Fallbacks    int64    `json:"fallbacks"`
	StaleServed  int64    `json:"stale_served"`
	CacheHitRate float64  `json:"cache_hit_rate"`
	P50ms        int64    `json:"p50_ms"`
	P95ms        int64    `json:"p95_ms"`
}

// Stats is the whole diagnostics snapshot.
type Stats struct {
	Since        time.Time       `json:"since"`
	Requests     int64           `json:"requests"`
	UpstreamCall int64           `json:"upstream_calls"`
	CacheHits    int64           `json:"cache_hits"`
	CacheHitRate float64         `json:"cache_hit_rate"`
	Errors       int64           `json:"errors"`
	Throttles    int64           `json:"throttles"`
	Fallbacks    int64           `json:"fallbacks"`
	StaleServed  int64           `json:"stale_served"`
	Users        int             `json:"users"`
	CacheEntries int             `json:"cache_entries"`
	Providers    map[string]bool `json:"providers_configured"`
	Endpoints    []EndpointStats `json:"endpoints"`
}

// Snapshot renders the counters. It is safe to serve without a vendor key
// present — an unconfigured lane still reports what it refused.
func (m *Meter) Snapshot() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := Stats{Since: m.since, Users: len(m.users)}
	for key, row := range m.rows {
		p50, p95 := percentiles(row.latencies)
		stat := EndpointStats{
			Provider: key.Provider, Endpoint: key.Endpoint,
			Requests: row.Requests, CacheHits: row.CacheHits, UpstreamCall: row.Upstream,
			Errors: row.Errors, Throttles: row.Throttles, NotFound: row.NotFound,
			Fallbacks: row.Fallbacks, StaleServed: row.StaleServed,
			P50ms: p50.Milliseconds(), P95ms: p95.Milliseconds(),
		}
		if row.Requests > 0 {
			stat.CacheHitRate = round4(float64(row.CacheHits) / float64(row.Requests))
		}
		out.Endpoints = append(out.Endpoints, stat)
		out.Requests += row.Requests
		out.UpstreamCall += row.Upstream
		out.CacheHits += row.CacheHits
		out.Errors += row.Errors
		out.Throttles += row.Throttles
		out.Fallbacks += row.Fallbacks
		out.StaleServed += row.StaleServed
	}
	if out.Requests > 0 {
		out.CacheHitRate = round4(float64(out.CacheHits) / float64(out.Requests))
	}
	sort.Slice(out.Endpoints, func(i, j int) bool {
		if out.Endpoints[i].Provider != out.Endpoints[j].Provider {
			return out.Endpoints[i].Provider < out.Endpoints[j].Provider
		}
		return out.Endpoints[i].Endpoint < out.Endpoints[j].Endpoint
	})
	return out
}

func percentiles(samples []time.Duration) (p50, p95 time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[idx(len(sorted), 0.50)], sorted[idx(len(sorted), 0.95)]
}

func idx(n int, q float64) int {
	i := int(float64(n) * q)
	if i >= n {
		i = n - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

/* ------------------------------------------------------------ rate limit -- */

// Limiter is the per-user bound on the lane. It protects the shared vendor quota
// from one user's runaway tab, and it is OUR limit — distinct from the vendor's,
// and reported as such.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	rate    float64
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter builds a token-bucket limiter: rate tokens per second with the
// given burst.
func NewLimiter(ratePerSecond, burst float64, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if ratePerSecond <= 0 {
		ratePerSecond = 5
	}
	if burst <= 0 {
		burst = 30
	}
	return &Limiter{buckets: map[string]*bucket{}, now: now, rate: ratePerSecond, burst: burst}
}

// Allow reports whether this user may make one more call now.
func (l *Limiter) Allow(user string) bool {
	if l == nil {
		return true
	}
	if user == "" {
		user = "anonymous"
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[user]
	if !ok {
		l.buckets[user] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
