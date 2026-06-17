// Package ratelimit is an in-house, dependency-free, per-key token-bucket rate
// limiter. layerxd uses it for application-level rate limiting (defense in depth
// behind the nginx edge limiter): even if a client reaches the loopback
// listener directly, or the edge is misconfigured, the sequencer still bounds
// per-client request rates. Matches the repo's "no external deps for the
// trusted surface" ethos (see layerx/contracts/README.md).
package ratelimit

import (
	"sync"
	"time"
)

// bucket is a single token bucket: a continuous fill at `rate` tokens/sec capped
// at `burst`, decremented one token per allowed request.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a keyed set of token buckets (one per client key, e.g. IP). It is
// safe for concurrent use.
type Limiter struct {
	mu    sync.Mutex
	rate  float64 // tokens added per second
	burst float64 // bucket capacity (max burst)
	keys  map[string]*bucket
}

// New builds a Limiter allowing `ratePerSec` sustained requests with up to
// `burst` in a spike. Non-positive inputs are clamped to 1.
func New(ratePerSec float64, burst int) *Limiter {
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{
		rate:  ratePerSec,
		burst: float64(burst),
		keys:  make(map[string]*bucket),
	}
}

// Allow reports whether a request for key may proceed now. When denied it also
// returns the duration the caller should wait before the next token is available
// (for a Retry-After header).
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	return l.allowAt(key, time.Now())
}

// allowAt is Allow with an injectable clock for deterministic tests.
func (l *Limiter) allowAt(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.keys[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.keys[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := (1 - b.tokens) / l.rate
	return false, time.Duration(wait * float64(time.Second))
}

// Purge drops buckets idle longer than maxIdle, bounding memory under a churn of
// distinct keys. A key idle past maxIdle has, by construction, refilled to full
// burst (set maxIdle >> burst/rate), so dropping it is equivalent to resetting
// it to its default full-burst state — it never relaxes an active limit.
func (l *Limiter) Purge(maxIdle time.Duration) {
	l.purgeAt(time.Now(), maxIdle)
}

func (l *Limiter) purgeAt(now time.Time, maxIdle time.Duration) {
	cutoff := now.Add(-maxIdle)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.keys {
		if b.last.Before(cutoff) {
			delete(l.keys, k)
		}
	}
}

// Len returns the number of tracked keys (for tests / metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}
