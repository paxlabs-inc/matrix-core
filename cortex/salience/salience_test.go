// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package salience

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// TestColdScoreFreshHighImportance: brand-new pinned memory with maximum
// declared importance should rank near 1.0.
func TestColdScoreFreshHighImportance(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(10, now)
	if s.Cached <= 0.20 {
		t.Fatalf("fresh importance=10 score too low: %f", s.Cached)
	}
	if s.Cached > 1.0 {
		t.Fatalf("score above 1.0: %f", s.Cached)
	}
}

// TestRecencyDecays follows the §8.1 verbatim formula `exp(-Δt / H)`: at
// Δt=0 the score is 1.0, at Δt=H it is 1/e (~0.368), and far past 5·H it
// is effectively zero. The label "half-life" in §8.1 is sloppy English;
// the formula itself is a 1/e time-constant decay.
func TestRecencyDecays(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Score{LastUsed: now.UnixNano()}
	r0 := recency(s.LastUsed, now)
	if math.Abs(float64(r0)-1.0) > 1e-6 {
		t.Fatalf("R at t=now: got %f want ~1.0", r0)
	}
	later := now.Add(time.Duration(HalfLifeNanos))
	rOneTau := recency(s.LastUsed, later)
	if math.Abs(float64(rOneTau)-1/math.E) > 1e-3 {
		t.Fatalf("R at t=now+τ: got %f want ~1/e (%.3f)", rOneTau, 1/math.E)
	}
	farLater := now.Add(time.Duration(HalfLifeNanos) * 5)
	rFar := recency(s.LastUsed, farLater)
	if rFar > 0.01 {
		t.Fatalf("R at t=now+5τ: got %f want < 0.01", rFar)
	}
}

// TestPinnedFloor: a pinned memory with all factors at zero still floors
// at PinnedFloor.
func TestPinnedFloor(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// Older than 5 half-lives so R is near zero.
	old := now.Add(-time.Duration(HalfLifeNanos * 10))
	s := Score{LastUsed: old.UnixNano(), Pinned: true}
	got := ColdScore(&s, now)
	if got < PinnedFloor-1e-6 {
		t.Fatalf("Pinned score below floor: got %f want >= %f", got, PinnedFloor)
	}
}

// TestCitationsDominate: two memories with identical (R, A, D) but different
// citation counts must rank by citations.
func TestCitationsDominate(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	a := Score{LastUsed: now.UnixNano(), Importance: 5, Citations: 0}
	b := Score{LastUsed: now.UnixNano(), Importance: 5, Citations: 100}
	sa := ColdScore(&a, now)
	sb := ColdScore(&b, now)
	if sb <= sa {
		t.Fatalf("citations should raise score: a=%f b=%f", sa, sb)
	}
}

// TestImportanceLinearity: D(m) = importance/10, with weight ratio in score.
// Doubling importance should monotonically raise score.
func TestImportanceMonotone(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	prev := float32(-1)
	for imp := uint8(0); imp <= 10; imp++ {
		s := Score{LastUsed: now.UnixNano(), Importance: imp}
		got := ColdScore(&s, now)
		if got < prev {
			t.Fatalf("importance %d -> score %f decreased from %f", imp, got, prev)
		}
		prev = got
	}
}

// TestEncodeDeterministic: two structurally identical Scores must encode
// to byte-identical CBOR.
func TestEncodeDeterministic(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(7, now)
	b1, err := Encode(&s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b2, err := Encode(&s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("non-deterministic encode")
	}
	var back Score
	if err := Decode(b1, &back); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if back.Cached != s.Cached || back.LastUsed != s.LastUsed || back.Importance != s.Importance {
		t.Fatalf("round trip mismatch: got %+v want %+v", back, s)
	}
}

// TestZeroForTombstone collapses Cached but leaves factor inputs intact so
// a future un-tombstone (hypothetical) recomputes correctly.
func TestZeroForTombstone(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(8, now)
	preCached := s.Cached
	if preCached == 0 {
		t.Fatalf("precondition: fresh score should be > 0")
	}
	ZeroForTombstone(&s, now.Add(time.Hour))
	if s.Cached != 0 {
		t.Fatalf("Cached should be 0 post-tombstone, got %f", s.Cached)
	}
	if s.Importance != 8 {
		t.Fatalf("Importance should be preserved")
	}
	if s.LastUsed == 0 {
		t.Fatalf("LastUsed should be preserved")
	}
}

// TestBumpForUpdate moves LastUsed forward and recomputes.
func TestBumpForUpdate(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(3, now)
	old := s.LastUsed
	BumpForUpdate(&s, 7, now.Add(time.Hour))
	if s.LastUsed <= old {
		t.Fatalf("LastUsed not advanced")
	}
	if s.Importance != 7 {
		t.Fatalf("Importance not updated: %d", s.Importance)
	}
}

// TestColdScoreClamps: even pathological inputs (huge access count, future
// last_used) must produce a score in [0,1].
func TestColdScoreClamps(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Score{
		LastUsed:    now.Add(time.Hour).UnixNano(), // future
		AccessCount: ^uint64(0),
		Citations:   ^uint64(0),
		Importance:  255, // wildly out of spec
		Pinned:      true,
	}
	got := ColdScore(&s, now)
	if got < 0 || got > 1.0 {
		t.Fatalf("score out of [0,1]: %f", got)
	}
}

// TestBumpForAccess increments AccessCount, advances LastUsed, and
// recomputes Cached. Phase 11.5: this is the helper called by every
// late-binding Find candidate.
func TestBumpForAccess(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(5, now)
	preAC := s.AccessCount
	preCached := s.Cached
	BumpForAccess(&s, now.Add(time.Hour))
	if s.AccessCount != preAC+1 {
		t.Fatalf("AccessCount: got %d want %d", s.AccessCount, preAC+1)
	}
	if s.LastUsed != now.Add(time.Hour).UnixNano() {
		t.Fatalf("LastUsed not advanced: %d", s.LastUsed)
	}
	if s.Citations != 0 {
		t.Fatalf("BumpForAccess must not touch Citations: %d", s.Citations)
	}
	// Cached should differ — both AccessCount factor and LastUsed
	// (recency) changed.
	if s.Cached == preCached {
		t.Fatalf("Cached unchanged after bump (want recompute)")
	}
}

// TestBumpForAccessSaturates refuses to wrap at math.MaxUint64.
func TestBumpForAccessSaturates(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Score{AccessCount: ^uint64(0)}
	BumpForAccess(&s, now)
	if s.AccessCount != ^uint64(0) {
		t.Fatalf("AccessCount wrapped: %d", s.AccessCount)
	}
}

// TestBumpForCitation bumps both Citations and AccessCount. Recency
// advances too — a citation IS a touch.
func TestBumpForCitation(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(5, now)
	preAC := s.AccessCount
	preC := s.Citations
	BumpForCitation(&s, now.Add(time.Hour))
	if s.Citations != preC+1 {
		t.Fatalf("Citations: got %d want %d", s.Citations, preC+1)
	}
	if s.AccessCount != preAC+1 {
		t.Fatalf("AccessCount: got %d want %d (citation implies access)", s.AccessCount, preAC+1)
	}
}

// TestDecrementCitation floors at zero (no underflow) and never touches
// AccessCount.
func TestDecrementCitation(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Score{Citations: 1, AccessCount: 4, LastUsed: now.UnixNano()}
	DecrementCitation(&s, now.Add(time.Hour))
	if s.Citations != 0 {
		t.Fatalf("Citations: got %d want 0", s.Citations)
	}
	if s.AccessCount != 4 {
		t.Fatalf("AccessCount should NOT change on decrement: got %d", s.AccessCount)
	}
	// Decrementing from zero must not underflow.
	DecrementCitation(&s, now.Add(2*time.Hour))
	if s.Citations != 0 {
		t.Fatalf("Citations underflowed: %d", s.Citations)
	}
}

// TestCitationsBumpDominatesCachedScore — after enough bumps, the
// Cached score should monotonically increase to match the §8.1
// formula's C(m) contribution. Demonstrates the bumps actually feed
// the score and aren't just incrementing counters in a vacuum.
func TestCitationsBumpDominatesCachedScore(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := NewForWrite(0, now)
	baseline := s.Cached
	for i := 0; i < 50; i++ {
		BumpForCitation(&s, now)
	}
	if s.Cached <= baseline {
		t.Fatalf("expected Cached to grow with citations: baseline=%f post=%f", baseline, s.Cached)
	}
	if s.Citations != 50 {
		t.Fatalf("Citations: got %d want 50", s.Citations)
	}
}

// TestDefaultWeightsSum: §8.2 cold weights sum to exactly 1.0.
func TestDefaultWeightsSum(t *testing.T) {
	w := DefaultWeights()
	sum := w.WR + w.WA + w.WC + w.WD + w.WV
	if math.Abs(float64(sum)-1.0) > 1e-6 {
		t.Fatalf("DefaultWeights sum: got %f want 1.0 (within 1e-6)", sum)
	}
	if w.SchemaVersion != WeightsSchemaVersion {
		t.Fatalf("DefaultWeights schema version: got %d want %d", w.SchemaVersion, WeightsSchemaVersion)
	}
}

// TestEncodeWeightsDeterministic: round-trip CBOR with byte equality.
func TestEncodeWeightsDeterministic(t *testing.T) {
	w := DefaultWeights()
	w.UpdatedAt = 1700000000_000000000
	w.Updates = 7
	b1, err := EncodeWeights(&w)
	if err != nil {
		t.Fatalf("EncodeWeights: %v", err)
	}
	b2, err := EncodeWeights(&w)
	if err != nil {
		t.Fatalf("EncodeWeights: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("non-deterministic encode: %x vs %x", b1, b2)
	}
	var back Weights
	if err := DecodeWeights(b1, &back); err != nil {
		t.Fatalf("DecodeWeights: %v", err)
	}
	if back != w {
		t.Fatalf("round trip mismatch: got %+v want %+v", back, w)
	}
}

// TestColdScoreWithDefaultEqualsColdScore: ColdScoreWith with default
// weights must equal ColdScore exactly (the delegate contract).
func TestColdScoreWithDefaultEqualsColdScore(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := Score{LastUsed: now.UnixNano(), AccessCount: 13, Citations: 5, Importance: 7}
	a := ColdScore(&s, now)
	b := ColdScoreWith(&s, DefaultWeights(), now)
	if a != b {
		t.Fatalf("ColdScore vs ColdScoreWith(Default): %f vs %f", a, b)
	}
}

// TestColdScoreWithLearnedWeightsRanks — learned weights that favour
// citations heavily must rank a high-citation memory above a high-
// recency one, even when default weights would have ranked them in
// the opposite order.
func TestColdScoreWithLearnedWeightsRanks(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// recencyMem: fresh, no citations.
	recencyMem := Score{LastUsed: now.UnixNano(), Importance: 3}
	// citeMem: stale (1 half-life ago) but 100 citations.
	citeMem := Score{LastUsed: now.Add(-time.Duration(HalfLifeNanos)).UnixNano(), Importance: 3, Citations: 100}

	def := DefaultWeights()
	defRecency := ColdScoreWith(&recencyMem, def, now)
	defCite := ColdScoreWith(&citeMem, def, now)
	// Under default cold weights the recency-only memory is roughly in
	// the same ballpark; we don't assert ordering here (formula-dependent).

	// Learned weights: shift heavily toward citations.
	learned := Weights{
		SchemaVersion: WeightsSchemaVersion,
		WR:            0.05, WA: 0.05, WC: 0.75, WD: 0.10, WV: 0.05,
	}
	lRecency := ColdScoreWith(&recencyMem, learned, now)
	lCite := ColdScoreWith(&citeMem, learned, now)

	if !(lCite > lRecency) {
		t.Fatalf("learned weights should rank cited > recent: cite=%f recent=%f", lCite, lRecency)
	}
	// And the learned cite score should be HIGHER than the default cite
	// score (because we biased toward C).
	if !(lCite > defCite) {
		t.Fatalf("learned cite (%f) should exceed default cite (%f)", lCite, defCite)
	}
	_ = defRecency
}

// TestUpdateWeightsEMA_Success: a single attest with one cited memory
// (high citations) pulls WC up and WR down by the spec rule. Sum stays
// at 1.0 within float32 tolerance.
func TestUpdateWeightsEMA_Success(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	w := DefaultWeights()
	pre := w
	// Citation-heavy memory: high C, low R/A/D.
	cited := []Score{{LastUsed: now.Add(-time.Duration(HalfLifeNanos)).UnixNano(), Citations: 100}}
	ok := UpdateWeightsEMA(&w, cited, EMARate, false, now)
	if !ok {
		t.Fatalf("UpdateWeightsEMA should report update applied")
	}
	if w == pre {
		t.Fatalf("weights unchanged after UpdateWeightsEMA")
	}
	// WC should have moved up (we pulled toward a high-C profile).
	if !(w.WC > pre.WC) {
		t.Fatalf("WC did not increase: pre=%f post=%f", pre.WC, w.WC)
	}
	// Sum-to-1.0 invariant.
	sum := w.WR + w.WA + w.WC + w.WD + w.WV
	if math.Abs(float64(sum)-1.0) > 1e-5 {
		t.Fatalf("post-EMA weights sum: %f", sum)
	}
	if w.Updates != 1 || w.UpdatedAt != now.UnixNano() {
		t.Fatalf("EMA audit fields: Updates=%d UpdatedAt=%d", w.Updates, w.UpdatedAt)
	}
}

// TestUpdateWeightsEMA_Failure: decrement-on-failure pulls weights AWAY
// from the cited profile (opposite sign), so a citation-heavy cited
// memory drives WC DOWN.
func TestUpdateWeightsEMA_Failure(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	w := DefaultWeights()
	pre := w
	cited := []Score{{LastUsed: now.UnixNano(), Citations: 100}}
	ok := UpdateWeightsEMA(&w, cited, EMARate, true, now)
	if !ok {
		t.Fatalf("UpdateWeightsEMA failure-decrement should apply")
	}
	if !(w.WC < pre.WC) {
		t.Fatalf("WC did not decrease on failure-decrement: pre=%f post=%f", pre.WC, w.WC)
	}
	sum := w.WR + w.WA + w.WC + w.WD + w.WV
	if math.Abs(float64(sum)-1.0) > 1e-5 {
		t.Fatalf("post-EMA-failure weights sum: %f", sum)
	}
}

// TestUpdateWeightsEMA_Degenerate: cited list with all-zero factor profile
// (no LastUsed touch, no citations, no importance) is a no-op. We push
// LastUsed deep enough into the past that R underflows float32 to 0.0
// (exp(-200) ≈ 1.4e-87, below the float32 denormal floor).
func TestUpdateWeightsEMA_Degenerate(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	w := DefaultWeights()
	pre := w
	cited := []Score{{LastUsed: now.Add(-time.Duration(HalfLifeNanos) * 250).UnixNano()}}
	// Sanity-check the precondition: factor profile MUST be all-zero.
	prof := factorProfile(&cited[0], now)
	if prof[0]+prof[1]+prof[2]+prof[3] != 0 {
		t.Fatalf("precondition: factor profile non-zero: %v", prof)
	}
	ok := UpdateWeightsEMA(&w, cited, EMARate, false, now)
	if ok {
		t.Fatalf("UpdateWeightsEMA should be no-op for all-zero profile")
	}
	if w != pre {
		t.Fatalf("weights mutated on degenerate update: pre=%+v post=%+v", pre, w)
	}
}

// TestUpdateWeightsEMA_EmptyCited: nil/empty cited list is a no-op.
func TestUpdateWeightsEMA_EmptyCited(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	w := DefaultWeights()
	pre := w
	if UpdateWeightsEMA(&w, nil, EMARate, false, now) {
		t.Fatalf("nil cited should no-op")
	}
	if UpdateWeightsEMA(&w, []Score{}, EMARate, false, now) {
		t.Fatalf("empty cited should no-op")
	}
	if w != pre {
		t.Fatalf("weights mutated on empty cited")
	}
}

// TestUpdateWeightsEMA_Renormalize: after many updates the sum stays at
// 1.0 within float tolerance.
func TestUpdateWeightsEMA_Renormalize(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	w := DefaultWeights()
	cited := []Score{{LastUsed: now.UnixNano(), Citations: 100, AccessCount: 200, Importance: 8}}
	for i := 0; i < 100; i++ {
		UpdateWeightsEMA(&w, cited, EMARate, false, now)
	}
	sum := w.WR + w.WA + w.WC + w.WD + w.WV
	if math.Abs(float64(sum)-1.0) > 1e-4 {
		t.Fatalf("renormalize drift after 100 updates: sum=%f", sum)
	}
	if w.Updates != 100 {
		t.Fatalf("Updates counter: got %d want 100", w.Updates)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
