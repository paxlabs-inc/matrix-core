// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package ratelimit implements a per-actor token-bucket rate limiter
// for the gateway's request-rate dimension (the credit_ledger handles
// the spend dimension separately). Buckets are kept in-memory: the
// gateway is a single-process box-side service, so distributed
// coordination is unnecessary; if/when the gateway sharded across
// multiple boxes the buckets would migrate to Redis or Postgres.
//
// Concurrency: every method on *Limiter is safe for concurrent use.
// Internally a single Mutex protects the bucket map; bucket-level
// state is mutated under the same lock to avoid the
// double-locking complexity of a per-bucket mutex (the contention
// envelope is small — one map lookup + a few floating-point ops).
package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// Limiter is a per-key token bucket store.
type Limiter struct {
	mu sync.Mutex

	// rate is the bucket refill rate (tokens per second).
	rate float64

	// burst is the bucket size (max tokens carried at any moment).
	burst float64

	// now overrides time.Now for tests; nil → real wall clock.
	now func() time.Time

	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// New constructs a Limiter with the given refill rate (tokens/second)
// and burst capacity. ratePerSec=0 disables rate limiting (every
// Allow returns true). burst<=0 defaults to ratePerSec (i.e. one
// second of burst headroom).
func New(ratePerSec, burst float64) *Limiter {
	if ratePerSec < 0 {
		ratePerSec = 0
	}
	if burst <= 0 {
		burst = ratePerSec
	}
	return &Limiter{
		rate:    ratePerSec,
		burst:   burst,
		buckets: make(map[string]*bucket),
	}
}

// SetClock injects a clock function for tests so refill arithmetic is
// deterministic. nil resets to wall clock.
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// nowFn returns the active clock, defaulting to time.Now.
func (l *Limiter) nowFn() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

// Allow consumes one token from the bucket identified by key. Returns
// true when the request is allowed; false when the bucket is empty.
// When ratePerSec was 0 at construction, every call returns true
// (no-op limiter).
func (l *Limiter) Allow(key string) bool {
	return l.AllowN(key, 1)
}

// AllowN consumes n tokens from the bucket identified by key.
// Convenience for batched/embedding-window calls. Returns false when
// the bucket can't satisfy the full demand (no partial consumption).
func (l *Limiter) AllowN(key string, n float64) bool {
	if n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rate == 0 {
		return true
	}
	b, ok := l.buckets[key]
	now := l.nowFn()
	if !ok {
		// New bucket starts at burst capacity.
		b = &bucket{tokens: l.burst, lastFill: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.lastFill).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * l.rate
			if b.tokens > l.burst {
				b.tokens = l.burst
			}
			b.lastFill = now
		}
	}
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// Snapshot returns the current token count for key, useful for
// /metrics endpoints or assertions in tests. Missing keys return
// burst (the bucket is virtually full until first use).
func (l *Limiter) Snapshot(key string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return l.burst
	}
	return b.tokens
}

// Reset clears all buckets. Test-only.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = make(map[string]*bucket)
}

// String returns a single-line descriptor for log lines.
func (l *Limiter) String() string {
	return fmt.Sprintf("ratelimit.Limiter(rate=%g/s burst=%g)", l.rate, l.burst)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
