// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"sort"
	"sync"
	"time"
)

type MeterRecord struct {
	Endpoint string
	User     string
	CacheHit bool
	Latency  time.Duration
	Cost     float64
	Err      error
}

type EndpointStats struct {
	Endpoint  string  `json:"endpoint"`
	Requests  int64   `json:"requests"`
	CacheHits int64   `json:"cache_hits"`
	Errors    int64   `json:"errors"`
	Cost      float64 `json:"cost_dollars"`
	P50ms     int64   `json:"p50_ms"`
	P95ms     int64   `json:"p95_ms"`
}

type Stats struct {
	Since     time.Time       `json:"since"`
	Requests  int64           `json:"requests"`
	CacheHits int64           `json:"cache_hits"`
	Errors    int64           `json:"errors"`
	Cost      float64         `json:"cost_dollars"`
	Users     int             `json:"users"`
	Endpoints []EndpointStats `json:"endpoints"`
}

type meterRow struct {
	requests, cacheHits, errors int64
	cost                        float64
	latencies                   []time.Duration
}

type Meter struct {
	mu    sync.Mutex
	since time.Time
	rows  map[string]*meterRow
	users map[string]bool
}

func NewMeter(now time.Time) *Meter {
	return &Meter{since: now.UTC(), rows: map[string]*meterRow{}, users: map[string]bool{}}
}

func (m *Meter) Observe(record MeterRecord) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[record.Endpoint]
	if row == nil {
		row = &meterRow{}
		m.rows[record.Endpoint] = row
	}
	row.requests++
	if record.CacheHit {
		row.cacheHits++
	}
	if record.Err != nil {
		row.errors++
	}
	row.cost += record.Cost
	if record.Latency > 0 {
		row.latencies = append(row.latencies, record.Latency)
		if len(row.latencies) > 512 {
			row.latencies = row.latencies[len(row.latencies)-512:]
		}
	}
	if record.User != "" {
		m.users[record.User] = true
	}
}

func (m *Meter) Snapshot() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Stats{Since: m.since, Users: len(m.users)}
	for endpoint, row := range m.rows {
		latencies := append([]time.Duration(nil), row.latencies...)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p := func(q float64) int64 {
			if len(latencies) == 0 {
				return 0
			}
			index := int(float64(len(latencies)-1) * q)
			return latencies[index].Milliseconds()
		}
		out.Endpoints = append(out.Endpoints, EndpointStats{Endpoint: endpoint, Requests: row.requests, CacheHits: row.cacheHits, Errors: row.errors, Cost: row.cost, P50ms: p(.5), P95ms: p(.95)})
		out.Requests += row.requests
		out.CacheHits += row.cacheHits
		out.Errors += row.errors
		out.Cost += row.cost
	}
	sort.Slice(out.Endpoints, func(i, j int) bool { return out.Endpoints[i].Endpoint < out.Endpoints[j].Endpoint })
	return out
}
