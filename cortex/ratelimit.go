// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 14: cortex-side rate limiting on the two sub-agent DoS surfaces
// flagged by EXTERNAL_REVIEW_2026_05_22 (R5 and R3b in matrix.ctx
// OPEN_Q_LOCKS_PRE_PHASE_13).
//
// Spec mapping:
//
//   - R5  scope-violation rate limiting — research/04-cortex.md §10
//     ("Any violation [...] is logged as a j/ journal event with severity:
//     high"). A malicious sub-agent looping cortex.ResolveScoped on
//     out-of-scope URIs churns ~50µs/violation of Pebble sync plus the
//     MMR cascade staged by the JournalHook installed in cortex.New
//     (see cortex.go:86). Without a limiter, the parent's OverallRoot
//     moves on every violation, the journal grows unboundedly, and
//     replay cost balloons.
//
//   - R3b cortex.Attest rate limiting — research/04-cortex.md §8.3.
//     cortex.Attest commits TWO journal entries (KindAttest + KindLearnWeights)
//     plus one salience write per cited memory plus one meta/salience_weights
//     write in one atomic batch. A sub-agent looping Attest on the same
//     intent floods journal/salience writes and applies repeated EMA
//     steps modulo renormalisation.
//
// Q-locks (Phase 14):
//
//   Q1 library = golang.org/x/time/rate (Go's de-facto token bucket;
//      AllowN(time, n) accepts a clock argument, so cortex's c.now()
//      override path threads naturally through tests).
//
//   Q2 bucket keys: scope violations keyed by (GrantedTo, GrantedBy)
//      per OPEN_Q_LOCKS line 602; attests keyed by (actor, intent_id)
//      per OPEN_Q_LOCKS line 603. Both are the natural DoS-attribution
//      keys — limiting per-scope and per-intent.
//
//   Q3 default rates: ScopeViolation = 10/sec burst 20 absorbs scope-
//      rebind retry storms (research/06-agents.md §7 scope expiry
//      races) while bounding malicious flooding; Attest = 1/sec burst 5
//      is ~1000× the legitimate cadence of "one attest per intent
//      completion" (research/02-protocol.md kind 12 of 15) so a real
//      multi-step attest burst still fits.
//
//   Q4 bucket lifecycle: in-memory only, lazy creation on first use.
//      Buckets are RUNTIME POLICY ENFORCEMENT state, not memory data —
//      NOT in store/ canonical per D17 ("store/ canonical; indexes/
//      derived"); NOT in OverallRoot. Mirrors the Phase 12
//      meta/salience_weights sidecar posture (matrix.ctx:363) — runtime
//      policy state lives outside the deterministic-replay anchor.
//      At typical workload (≤1000 distinct (scope, intent) pairs over a
//      cortex lifetime) memory bound is ~50 KiB. LRU eviction deferred
//      until profiling shows it matters.
//
//   Q5 error semantics: scope-violation over-rate returns
//      scope.ErrViolation (existing sentinel; caller can't distinguish
//      rate-limited from genuinely out-of-scope — intentional, same
//      retry posture either way) but DOES NOT journal KindScopeViolation.
//      Attest over-rate returns the NEW sentinel ErrAttestRateLimited
//      (callers may want backoff+retry; distinct from
//      AttestResult.SkippedURIs which means "attest succeeded with some
//      cited URIs skipped").
//
//   Q6 tests disable rate limiting via WithRateLimits(UnlimitedRateLimits()).
//      Production callers MUST NOT use UnlimitedRateLimits.
//
//   Q7 clock: rate.Limiter.AllowN(now, 1) accepts the provided wall
//      time; we always pass c.now() so test fakes (WithClock) thread
//      transparently through rate decisions.
//
// Replay invariant (PRESERVED). Rate limiter buckets are pure runtime
// policy — not journaled, not in OverallRoot. Dropping a journal write
// because of rate limiting affects the JOURNAL SHAPE (some
// KindScopeViolation entries that a chatty actor would have produced
// don't appear) but does NOT introduce non-determinism into replay:
// the dropped entries weren't journaled, so the replay walk doesn't
// re-derive them either. The KindAttest path is stricter — if rate
// limiting drops the attest, the salience bumps DON'T fire and the
// KindAttest+KindLearnWeights pair is NOT emitted, so the post-replay
// state matches whatever live state was produced.

package cortex

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrAttestRateLimited is returned by Cortex.Attest when the per-(actor,
// intent_id) token bucket is empty. Distinct from the existing
// AttestResult.SkippedURIs semantics (which means "attest succeeded but
// some cited URIs were skipped"). Callers may backoff and retry.
var ErrAttestRateLimited = errors.New("cortex.Attest: rate limited (per actor+intent_id)")

// RateLimits configures the Phase 14 token buckets. Zero-value is
// invalid — always populate via DefaultRateLimits() or
// UnlimitedRateLimits().
//
// ScopeViolation / Attest are rate.Limit (events per second). Use
// rate.Inf to disable (tests only).
//
// *Burst is the token-bucket capacity. Bursts above this are dropped.
// Refill is continuous at the rate.Limit.
type RateLimits struct {
	// ScopeViolation caps the per-(GrantedTo, GrantedBy) rate at which
	// cortex.logScopeViolation writes KindScopeViolation journal
	// entries. Excess violations still return scope.ErrViolation to
	// the caller; only the journal write (and its MMR cascade) is
	// suppressed.
	ScopeViolation      rate.Limit
	ScopeViolationBurst int

	// Attest caps the per-(actor, intent_id) rate at which cortex.Attest
	// is allowed to proceed. Excess calls return ErrAttestRateLimited
	// without journaling anything or mutating salience.
	Attest      rate.Limit
	AttestBurst int
}

// DefaultRateLimits returns the Phase 14 production defaults per Q3 lock.
//
//	ScopeViolation: 10 events/sec, burst 20
//	Attest:          1 event/sec,  burst 5
//
// 10/sec scope violations absorbs a 20-violation retry storm during
// scope-rebinding races without dropping audit entries; legitimate steady
// state is ≪1/sec.
//
// 1/sec attests is 1000× the legitimate "one attest per intent completion"
// cadence; bursts above 1/sec on the same (actor, intent_id) are either
// a bug or a flood.
func DefaultRateLimits() RateLimits {
	return RateLimits{
		ScopeViolation:      rate.Limit(10),
		ScopeViolationBurst: 20,
		Attest:              rate.Limit(1),
		AttestBurst:         5,
	}
}

// UnlimitedRateLimits disables both limits. Reserved for tests that
// need to drive scope violations or attests in burst.
//
// Production callers MUST NOT use this; the limiter is the only thing
// bounding Pebble sync churn from a malicious sub-agent.
func UnlimitedRateLimits() RateLimits {
	return RateLimits{
		ScopeViolation:      rate.Inf,
		ScopeViolationBurst: 1,
		Attest:              rate.Inf,
		AttestBurst:         1,
	}
}

// WithRateLimits overrides the Phase 14 token-bucket configuration.
// If multiple WithRateLimits options are passed, the last one wins.
// Clears any existing per-key limiters so subsequent traffic picks up
// the new rates immediately.
func WithRateLimits(rl RateLimits) Option {
	return func(c *Cortex) {
		c.rl.setLimits(rl)
	}
}

// scopeBucketKey identifies one (GrantedTo, GrantedBy) bucket. Both
// fields are part of the key because a single granter might issue
// scopes to many sub-agents (independent buckets) and a single sub-
// agent might hold scopes from multiple parents.
type scopeBucketKey struct {
	grantedTo string
	grantedBy string
}

// attestBucketKey identifies one (actor, intent_id) bucket. Per Q2
// lock; matches matrix.ctx:603. Actor is the local actor (cortex is
// always operating on its store's actor) and intent_id is the MCL
// intent the attest references.
type attestBucketKey struct {
	actor    string
	intentID string
}

// rateLimiter owns the in-memory token buckets for scope-violation
// journaling and cortex.Attest. Concurrent-safe via a single mutex
// guarding the limits + maps. The hot path (allow*) is
// map-lookup + bucket-update; contention is minimal in expected
// workloads (single primary agent + a handful of sub-agents).
type rateLimiter struct {
	mu              sync.Mutex
	limits          RateLimits
	scopeViolations map[scopeBucketKey]*rate.Limiter
	attests         map[attestBucketKey]*rate.Limiter
}

// newRateLimiter constructs a rateLimiter at DefaultRateLimits.
// Called once from cortex.New; WithRateLimits may overwrite the limits
// before any traffic.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		limits:          DefaultRateLimits(),
		scopeViolations: make(map[scopeBucketKey]*rate.Limiter),
		attests:         make(map[attestBucketKey]*rate.Limiter),
	}
}

// setLimits installs a new RateLimits and clears existing per-key
// buckets so the next call recreates them at the new rate. Safe to
// call concurrently with allow* — bucket lookups race lock-free? No,
// all access is mu-guarded; setLimits acquires mu the same as allow*.
func (r *rateLimiter) setLimits(rl RateLimits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limits = rl
	r.scopeViolations = make(map[scopeBucketKey]*rate.Limiter)
	r.attests = make(map[attestBucketKey]*rate.Limiter)
}

// allowScopeViolation returns true iff the (grantedTo, grantedBy) bucket
// has a token to spend at `now`. False means the journal write should
// be suppressed; the caller still returns scope.ErrViolation to its
// own caller.
//
// Lazy bucket creation: first call for a key constructs the limiter at
// the currently-configured rate.
func (r *rateLimiter) allowScopeViolation(grantedTo, grantedBy string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := scopeBucketKey{grantedTo: grantedTo, grantedBy: grantedBy}
	lim, ok := r.scopeViolations[k]
	if !ok {
		lim = rate.NewLimiter(r.limits.ScopeViolation, r.limits.ScopeViolationBurst)
		r.scopeViolations[k] = lim
	}
	return lim.AllowN(now, 1)
}

// allowAttest returns true iff the (actor, intentID) bucket has a
// token to spend at `now`. False means cortex.Attest must return
// ErrAttestRateLimited without any side effects (no journal entries,
// no salience writes, no weight updates).
func (r *rateLimiter) allowAttest(actor, intentID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := attestBucketKey{actor: actor, intentID: intentID}
	lim, ok := r.attests[k]
	if !ok {
		lim = rate.NewLimiter(r.limits.Attest, r.limits.AttestBurst)
		r.attests[k] = lim
	}
	return lim.AllowN(now, 1)
}

// snapshotForTests returns the current count of live buckets per
// surface. Used by tests to assert that buckets are being created
// (and not leaked) across calls.
func (r *rateLimiter) snapshotForTests() (scopeBuckets, attestBuckets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.scopeViolations), len(r.attests)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
