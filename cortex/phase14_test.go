// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 14 integration tests — rate limiting on the two sub-agent DoS
// surfaces flagged by EXTERNAL_REVIEW_2026_05_22 (R5 + R3b). These
// drive cortex.ResolveScoped + cortex.Attest at burst rates with a
// fixed mock clock and assert the gate behaviour observable from
// outside (journal entry counts, returned errors, salience mutation
// absence on rejection).

package cortex

import (
	"errors"
	"testing"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
	"matrix/cortex/store"
)

// TestPhase14ScopeViolationBurstHonoursBurstCap drives a flood of 25
// out-of-scope ResolveScoped calls under a frozen clock. Expect:
//   - all 25 return scope.ErrViolation (the primary error path is
//     preserved regardless of rate limiting),
//   - exactly 20 KindScopeViolation entries journaled (burst cap),
//   - the remaining 5 are silently suppressed.
func TestPhase14ScopeViolationBurstHonoursBurstCap(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	freezeClock(t, c)

	// Memory is a Fact; scope only allows Preference → every Resolve
	// is a violation.
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1,
		Subject:       "x",
		Predicate:     "y",
		Statement:     "z",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	s := buildTestScope(t, c, priv, nil) // Include=[Preference] only

	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	violationsReturned := 0
	for i := 0; i < 25; i++ {
		_, err := c.ResolveScoped(uri, s, time.Time{})
		if errors.Is(err, scope.ErrViolation) {
			violationsReturned++
		}
	}
	postCount := countKindEntries(t, c, journal.KindScopeViolation)

	if violationsReturned != 25 {
		t.Errorf("scope.ErrViolation returns: got %d want 25 (rate limit must not change error semantics)", violationsReturned)
	}
	journaled := postCount - preCount
	if journaled != 20 {
		t.Errorf("KindScopeViolation entries: got %d want 20 (burst cap)", journaled)
	}
}

// TestPhase14ScopeViolationRefillsAfterClockAdvance verifies that after
// the burst is exhausted, advancing the clock by 1 second restores
// 10 tokens (ScopeViolation rate).
func TestPhase14ScopeViolationRefillsAfterClockAdvance(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	clk := advanceableClock(t, c)

	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1,
		Subject:       "a", Predicate: "b", Statement: "c",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	s := buildTestScope(t, c, priv, nil)

	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	// Drain the burst (20 tokens).
	for i := 0; i < 25; i++ {
		_, _ = c.ResolveScoped(uri, s, time.Time{})
	}
	exhaustedCount := countKindEntries(t, c, journal.KindScopeViolation) - preCount
	if exhaustedCount != 20 {
		t.Fatalf("precondition: post-burst count got %d want 20", exhaustedCount)
	}

	// Advance 1 second → bucket gains 10 tokens.
	clk.advance(1 * time.Second)
	for i := 0; i < 12; i++ {
		_, _ = c.ResolveScoped(uri, s, time.Time{})
	}
	postRefillCount := countKindEntries(t, c, journal.KindScopeViolation) - preCount
	if postRefillCount != 30 {
		t.Errorf("post-refill journal count: got %d want 30 (20 burst + 10 refilled)", postRefillCount)
	}
}

// TestPhase14ScopeViolationIndependentBuckets — two different child
// agents share the same parent but maintain independent buckets.
// Flooding one does NOT silence the other's audit trail.
func TestPhase14ScopeViolationIndependentBuckets(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	freezeClock(t, c)

	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1, Subject: "a", Predicate: "b", Statement: "c",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Two scopes from the same parent to two distinct children.
	scopeAlice := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.GrantedTo = "did:pax:0xalice"
	})
	scopeBob := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.GrantedTo = "did:pax:0xbob"
	})

	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	// Flood alice with 30 violations (only 20 should journal).
	for i := 0; i < 30; i++ {
		_, _ = c.ResolveScoped(uri, scopeAlice, time.Time{})
	}
	// Bob's bucket is fresh — 20 more should journal.
	for i := 0; i < 25; i++ {
		_, _ = c.ResolveScoped(uri, scopeBob, time.Time{})
	}
	postCount := countKindEntries(t, c, journal.KindScopeViolation)

	totalJournaled := postCount - preCount
	if totalJournaled != 40 {
		t.Errorf("total journaled: got %d want 40 (20 alice + 20 bob, independent buckets)", totalJournaled)
	}
}

// TestPhase14AttestBurstHonoursBurstCap — burst of 6 attests on same
// (actor, intent_id). First 5 succeed; 6th returns ErrAttestRateLimited
// and emits NO journal entries (neither KindAttest nor KindLearnWeights).
func TestPhase14AttestBurstHonoursBurstCap(t *testing.T) {
	c := openCortex(t)
	freezeClock(t, c)
	uri := writePref(t, c, "topic", 5)

	preAttest := countKindEntries(t, c, journal.KindAttest)
	preLearn := countKindEntries(t, c, journal.KindLearnWeights)

	var rateLimited int
	for i := 0; i < 6; i++ {
		_, err := c.Attest(AttestOpts{
			IntentID:  "intent-burst",
			Outcome:   AttestOutcomeSuccess,
			Cited:     []memory.URI{uri},
			CreatedBy: "andrew",
		})
		if errors.Is(err, ErrAttestRateLimited) {
			rateLimited++
		}
	}
	postAttest := countKindEntries(t, c, journal.KindAttest)
	postLearn := countKindEntries(t, c, journal.KindLearnWeights)

	if rateLimited != 1 {
		t.Errorf("ErrAttestRateLimited returns: got %d want 1 (6th call)", rateLimited)
	}
	if attestJournaled := postAttest - preAttest; attestJournaled != 5 {
		t.Errorf("KindAttest journal count: got %d want 5", attestJournaled)
	}
	if learnJournaled := postLearn - preLearn; learnJournaled != 5 {
		t.Errorf("KindLearnWeights journal count: got %d want 5 (paired with each KindAttest)", learnJournaled)
	}
}

// TestPhase14AttestRateLimitedDoesNotMutateSalience — when an attest is
// rate-limited, salience.Citations / salience.AccessCount stay at the
// pre-call values. Verifies that the gate fires BEFORE any salience
// reads or writes.
func TestPhase14AttestRateLimitedDoesNotMutateSalience(t *testing.T) {
	c := openCortex(t)
	freezeClock(t, c)
	uri := writePref(t, c, "topic", 5)
	_, id, _, _ := ParseURI(uri)

	// Drain burst (5 attests succeed).
	for i := 0; i < 5; i++ {
		if _, err := c.Attest(AttestOpts{
			IntentID:  "intent-mute",
			Outcome:   AttestOutcomeSuccess,
			Cited:     []memory.URI{uri},
			CreatedBy: "andrew",
		}); err != nil {
			t.Fatalf("burst attest %d: %v", i, err)
		}
	}
	post, _, _ := salience.Read(c.s, id)
	preCitations, preAccess := post.Citations, post.AccessCount

	// 6th call is rate-limited.
	_, err := c.Attest(AttestOpts{
		IntentID:  "intent-mute",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uri},
		CreatedBy: "andrew",
	})
	if !errors.Is(err, ErrAttestRateLimited) {
		t.Fatalf("got err=%v want ErrAttestRateLimited", err)
	}

	postRL, _, _ := salience.Read(c.s, id)
	if postRL.Citations != preCitations {
		t.Errorf("Citations mutated by rate-limited attest: pre=%d post=%d", preCitations, postRL.Citations)
	}
	if postRL.AccessCount != preAccess {
		t.Errorf("AccessCount mutated by rate-limited attest: pre=%d post=%d", preAccess, postRL.AccessCount)
	}
}

// TestPhase14AttestIndependentBucketsPerIntent — different intent IDs
// from the same actor share NO bucket. Each gets its own burst.
func TestPhase14AttestIndependentBucketsPerIntent(t *testing.T) {
	c := openCortex(t)
	freezeClock(t, c)
	uri := writePref(t, c, "topic", 5)

	preCount := countKindEntries(t, c, journal.KindAttest)

	// Burst on intent-1: 5 succeed.
	for i := 0; i < 5; i++ {
		if _, err := c.Attest(AttestOpts{
			IntentID:  "intent-1",
			Outcome:   AttestOutcomeSuccess,
			Cited:     []memory.URI{uri},
			CreatedBy: "andrew",
		}); err != nil {
			t.Fatalf("intent-1 attest %d: %v", i, err)
		}
	}
	// intent-2: fresh bucket, 5 more succeed.
	for i := 0; i < 5; i++ {
		if _, err := c.Attest(AttestOpts{
			IntentID:  "intent-2",
			Outcome:   AttestOutcomeSuccess,
			Cited:     []memory.URI{uri},
			CreatedBy: "andrew",
		}); err != nil {
			t.Fatalf("intent-2 attest %d: %v", i, err)
		}
	}
	postCount := countKindEntries(t, c, journal.KindAttest)
	if got := postCount - preCount; got != 10 {
		t.Errorf("KindAttest journaled: got %d want 10 (2 intents × 5 burst)", got)
	}
}

// TestPhase14WithRateLimitsUnlimitedDisablesGates — production callers
// must not use this, but the option exists so test suites that hammer
// the API don't get false-positive rate-limit rejections.
func TestPhase14WithRateLimitsUnlimitedDisablesGates(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortexUnlimited(t)
	withResolver(t, c, pub)
	freezeClock(t, c)

	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1, Subject: "a", Predicate: "b", Statement: "c",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	scp := buildTestScope(t, c, priv, nil)

	// 100 violations — all should journal under unlimited.
	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	for i := 0; i < 100; i++ {
		_, _ = c.ResolveScoped(uri, scp, time.Time{})
	}
	got := countKindEntries(t, c, journal.KindScopeViolation) - preCount
	if got != 100 {
		t.Errorf("scope violations journaled under unlimited: got %d want 100", got)
	}

	// Now drive attest beyond default burst (5) on a single intent_id.
	preAttestCount := countKindEntries(t, c, journal.KindAttest)
	pref := writePref(t, c, "topic", 5)
	for i := 0; i < 20; i++ {
		_, err := c.Attest(AttestOpts{
			IntentID:  "intent-flood",
			Outcome:   AttestOutcomeSuccess,
			Cited:     []memory.URI{pref},
			CreatedBy: "andrew",
		})
		if err != nil {
			t.Fatalf("attest %d unexpectedly errored under unlimited: %v", i, err)
		}
	}
	gotAttest := countKindEntries(t, c, journal.KindAttest) - preAttestCount
	if gotAttest != 20 {
		t.Errorf("attest journaled under unlimited: got %d want 20", gotAttest)
	}
}

// freezeClock pins c.now to a fixed time so every rate-limiter
// AllowN(now) sees the same instant (i.e., no automatic refill from
// wall-clock advance during the test).
func freezeClock(t *testing.T, c *Cortex) {
	t.Helper()
	fixed := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { return fixed }
}

// advanceableClock returns a handle that the test can use to advance
// the cortex's clock by a controlled duration. All cortex calls between
// advance() invocations see the same "now".
func advanceableClock(t *testing.T, c *Cortex) *testClock {
	t.Helper()
	tc := &testClock{now: time.Unix(1700000000, 0).UTC()}
	c.now = func() time.Time { return tc.now }
	return tc
}

type testClock struct {
	now time.Time
}

func (c *testClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// openCortexUnlimited is openCortex + WithRateLimits(UnlimitedRateLimits()).
// Used by tests that need to drive the API past production rate caps to
// exercise unrelated behaviour.
func openCortexUnlimited(t *testing.T) *Cortex {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, WithRateLimits(UnlimitedRateLimits()))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
