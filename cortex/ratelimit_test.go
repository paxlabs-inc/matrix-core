// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 14 rate-limiter unit tests. These exercise the rateLimiter
// struct in isolation (no cortex / no Pebble) so the bucket-key,
// burst-capacity, and replenishment semantics are pinned independently
// of the gate-call-sites in scope_enforce.go and attest.go.
//
// Clock is fully mocked via the now parameter to allow* methods, so
// no test sleeps.

package cortex

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fixedTime returns a time.Time that's deterministic and easy to
// advance by sub-second units in tests. Tests must use Add() on the
// returned value rather than re-calling time.Now() to keep clock
// monotonic per rate.Limiter expectations.
func fixedTime() time.Time {
	return time.Unix(1700000000, 0).UTC()
}

// TestRateLimiterDefaultsAreProductionValues verifies that
// DefaultRateLimits() returns the Phase 14 Q3 locks verbatim. A drift
// here either (a) silently slackens the DoS bound or (b) silently
// tightens it below legitimate workload — both regressions get caught
// at CI rather than ops.
func TestRateLimiterDefaultsAreProductionValues(t *testing.T) {
	rl := DefaultRateLimits()
	if rl.ScopeViolation != rate.Limit(10) {
		t.Errorf("ScopeViolation: got %v want 10/sec", rl.ScopeViolation)
	}
	if rl.ScopeViolationBurst != 20 {
		t.Errorf("ScopeViolationBurst: got %d want 20", rl.ScopeViolationBurst)
	}
	if rl.Attest != rate.Limit(1) {
		t.Errorf("Attest: got %v want 1/sec", rl.Attest)
	}
	if rl.AttestBurst != 5 {
		t.Errorf("AttestBurst: got %d want 5", rl.AttestBurst)
	}
}

// TestRateLimiterScopeViolationConsumesBurst verifies that the first
// ScopeViolationBurst calls succeed and the (burst+1)th is rejected
// when the clock doesn't advance.
func TestRateLimiterScopeViolationConsumesBurst(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.ScopeViolationBurst

	// First `burst` calls all at the same instant must succeed.
	for i := 0; i < burst; i++ {
		if !r.allowScopeViolation("alice", "andrew", t0) {
			t.Fatalf("call %d/%d should be allowed under burst", i+1, burst)
		}
	}
	// burst+1 at the same instant must be rejected.
	if r.allowScopeViolation("alice", "andrew", t0) {
		t.Errorf("call %d should be rejected (burst exhausted)", burst+1)
	}
}

// TestRateLimiterScopeViolationReplenishes verifies tokens drip back
// into the bucket at the configured rate. After exhausting burst, a
// clock advance of 1 second at 10/sec should restore exactly 10
// tokens.
func TestRateLimiterScopeViolationReplenishes(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.ScopeViolationBurst

	for i := 0; i < burst; i++ {
		r.allowScopeViolation("a", "b", t0)
	}
	// Confirm exhausted.
	if r.allowScopeViolation("a", "b", t0) {
		t.Fatalf("precondition: bucket should be exhausted at t0")
	}

	// Advance 1 second at 10/sec ⇒ +10 tokens (cap at burst=20 but
	// we exhausted everything so we only get 10 back).
	t1 := t0.Add(1 * time.Second)
	for i := 0; i < 10; i++ {
		if !r.allowScopeViolation("a", "b", t1) {
			t.Fatalf("post-refill call %d/10 should be allowed", i+1)
		}
	}
	// 11th post-refill at same t1 must reject again.
	if r.allowScopeViolation("a", "b", t1) {
		t.Errorf("post-refill call 11 should be rejected (10 tokens consumed)")
	}
}

// TestRateLimiterScopeViolationKeysIndependent verifies that two
// (GrantedTo, GrantedBy) pairs have separate buckets. A flooded
// (alice, andrew) does not affect (bob, andrew) or (alice, eve).
func TestRateLimiterScopeViolationKeysIndependent(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.ScopeViolationBurst

	// Exhaust (alice, andrew) bucket.
	for i := 0; i < burst+5; i++ {
		r.allowScopeViolation("alice", "andrew", t0)
	}
	// (bob, andrew) must still have a full burst available.
	for i := 0; i < burst; i++ {
		if !r.allowScopeViolation("bob", "andrew", t0) {
			t.Fatalf("(bob, andrew) call %d should be allowed (independent bucket)", i+1)
		}
	}
	// (alice, eve) must still have a full burst — different granter.
	for i := 0; i < burst; i++ {
		if !r.allowScopeViolation("alice", "eve", t0) {
			t.Fatalf("(alice, eve) call %d should be allowed (independent bucket)", i+1)
		}
	}
	// Bucket count: 3 distinct keys.
	scopes, _ := r.snapshotForTests()
	if scopes != 3 {
		t.Errorf("bucket count: got %d want 3", scopes)
	}
}

// TestRateLimiterAttestConsumesBurst is the Attest analogue of the
// scope-violation burst test. Smaller burst (5 by default).
func TestRateLimiterAttestConsumesBurst(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.AttestBurst

	for i := 0; i < burst; i++ {
		if !r.allowAttest("andrew", "intent-1", t0) {
			t.Fatalf("call %d/%d should be allowed under burst", i+1, burst)
		}
	}
	if r.allowAttest("andrew", "intent-1", t0) {
		t.Errorf("call %d should be rejected (burst exhausted)", burst+1)
	}
}

// TestRateLimiterAttestReplenishes — 1/sec means a 1-second advance
// returns exactly 1 token.
func TestRateLimiterAttestReplenishes(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.AttestBurst

	for i := 0; i < burst; i++ {
		r.allowAttest("a", "i1", t0)
	}
	if r.allowAttest("a", "i1", t0) {
		t.Fatalf("precondition: bucket should be exhausted")
	}

	// +1 second at 1/sec ⇒ exactly 1 token back.
	t1 := t0.Add(1 * time.Second)
	if !r.allowAttest("a", "i1", t1) {
		t.Errorf("post-refill call should be allowed")
	}
	if r.allowAttest("a", "i1", t1) {
		t.Errorf("second post-refill call at same instant should be rejected")
	}
}

// TestRateLimiterAttestKeysIndependent — different intent_id values
// from the same actor share NO bucket state. Same actor + different
// intent = independent floods.
func TestRateLimiterAttestKeysIndependent(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	burst := r.limits.AttestBurst

	// Drain intent-1.
	for i := 0; i < burst+3; i++ {
		r.allowAttest("andrew", "intent-1", t0)
	}
	// intent-2 must have full burst.
	for i := 0; i < burst; i++ {
		if !r.allowAttest("andrew", "intent-2", t0) {
			t.Fatalf("intent-2 call %d should be allowed", i+1)
		}
	}
	// Different actor + same intent = also independent.
	for i := 0; i < burst; i++ {
		if !r.allowAttest("eve", "intent-1", t0) {
			t.Fatalf("(eve, intent-1) call %d should be allowed", i+1)
		}
	}
	_, attests := r.snapshotForTests()
	if attests != 3 {
		t.Errorf("attest bucket count: got %d want 3", attests)
	}
}

// TestRateLimiterUnlimitedDisablesBothGates — when WithRateLimits
// installs UnlimitedRateLimits(), arbitrarily many calls in zero
// elapsed time all succeed.
func TestRateLimiterUnlimitedDisablesBothGates(t *testing.T) {
	r := newRateLimiter()
	r.setLimits(UnlimitedRateLimits())
	t0 := fixedTime()

	for i := 0; i < 10000; i++ {
		if !r.allowScopeViolation("a", "b", t0) {
			t.Fatalf("scope call %d should be allowed (rate.Inf)", i)
		}
		if !r.allowAttest("a", "i", t0) {
			t.Fatalf("attest call %d should be allowed (rate.Inf)", i)
		}
	}
}

// TestRateLimiterSetLimitsClearsBuckets — after WithRateLimits is
// called mid-flight, existing buckets are dropped so the new rate
// takes effect on the next call (rather than the old bucket's
// stale token count carrying over).
func TestRateLimiterSetLimitsClearsBuckets(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	// Populate a few buckets at default rates.
	r.allowScopeViolation("a", "b", t0)
	r.allowAttest("a", "i", t0)
	scopes, attests := r.snapshotForTests()
	if scopes != 1 || attests != 1 {
		t.Fatalf("precondition: 1 bucket each, got scope=%d attest=%d", scopes, attests)
	}

	// Tighten rate; buckets cleared.
	r.setLimits(RateLimits{
		ScopeViolation:      rate.Limit(1),
		ScopeViolationBurst: 2,
		Attest:              rate.Limit(0.1),
		AttestBurst:         1,
	})
	scopes, attests = r.snapshotForTests()
	if scopes != 0 || attests != 0 {
		t.Errorf("post-setLimits: buckets should be cleared, got scope=%d attest=%d", scopes, attests)
	}

	// New bucket honors the new limits.
	if !r.allowScopeViolation("a", "b", t0) {
		t.Errorf("first call under new rate should succeed (burst 2)")
	}
	if !r.allowScopeViolation("a", "b", t0) {
		t.Errorf("second call under new rate should succeed (burst 2)")
	}
	if r.allowScopeViolation("a", "b", t0) {
		t.Errorf("third call under new rate (burst 2) should fail")
	}
}

// TestRateLimiterBucketsLeakOnHighKeyCardinality — sanity check the
// memory bound. 1000 distinct sub-agents each producing one violation
// generates 1000 buckets; ~50 bytes each is ~50 KiB total. Just verify
// the map fills correctly (no key collisions, no eviction).
func TestRateLimiterBucketsLeakOnHighKeyCardinality(t *testing.T) {
	r := newRateLimiter()
	t0 := fixedTime()
	for i := 0; i < 1000; i++ {
		grantedTo := "sub-agent-" + intToStr(i)
		r.allowScopeViolation(grantedTo, "andrew", t0)
	}
	scopes, _ := r.snapshotForTests()
	if scopes != 1000 {
		t.Errorf("buckets: got %d want 1000 (no eviction yet)", scopes)
	}
}

// intToStr is a tiny strconv shim to keep this test file dep-light.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
