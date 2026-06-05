// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package salience computes and persists the per-memory salience score
// described in research/04-cortex.md §8.
//
// Formula (§8.1):
//
//	salience(m) = w1·R(m) + w2·A(m) + w3·C(m) + w4·D(m) + w5·V(m, q)
//
//	R(m) = exp(-Δt(now, last_used) / half_life)        // recency, half-life 90d
//	A(m) = log(1 + access_count) / log(1 + 1000)        // normalized access
//	C(m) = log(1 + cite_in_successful_plans) / log(1+1000)
//	D(m) = declared_importance / 10.0                   // 0..1
//	V(m, q) = cos(embedding(m), embedding(q))           // 0 when q.near unset
//
//	Cold weights (§8.2): w = (0.25, 0.15, 0.30, 0.20, 0.10).
//
// Vector-weight gating (§8.2): when q.near is unset, w5 redistributes to
// the other weights proportionally. We bake that in here by dividing the
// non-vector component by the non-vector weight sum (0.90 at cold start),
// which is equivalent to "renormalize the remaining weights so they sum
// to 1".
//
// Pinned floor (§8.2): pinned memories have salience floor of 0.7.
// Tombstoned memories are filtered upstream by the Find planner; this
// package does NOT silently zero them, because we want the cache to remain
// truthful if the memory is later un-tombstoned.
//
// Per-actor weight learning (§8.3, Phase 12): a single per-actor Weights
// record is stored at meta/salience_weights (one Pebble key). The cortex
// is per-actor by construction so no actor suffix is needed in-key.
// EMA-updated by cortex.Attest with alpha=0.05; the BumpFor* helpers in
// THIS package preserve their original signatures and continue to compute
// the per-memory Cached value using the DEFAULT cold weights. Authoritative
// live ranking is computed by ColdScoreWith(score, weights, now) called
// from query.Run (and similar hot paths) using the actor's current
// learned Weights; sc.Cached is a debug/inspection snapshot, not the live
// ranking value (Phase 12 lock — see Q4 in phase12_locked_design).
package salience

import (
	"fmt"
	"math"
	"time"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
)

// SchemaVersion is bumped on any incompatible Score-encoding change.
const SchemaVersion uint8 = 1

// Cold weights from §8.2. They sum to 1.0. These are the DEFAULT weights
// used when meta/salience_weights is absent (i.e. a freshly initialised
// actor) and the floor that BumpFor* helpers use to recompute sc.Cached.
// Authoritative live ranking uses the learned Weights via ColdScoreWith.
const (
	WR float32 = 0.25 // recency
	WA float32 = 0.15 // access
	WC float32 = 0.30 // citations
	WD float32 = 0.20 // declared importance
	WV float32 = 0.10 // vector similarity (gated by q.near)
)

// WeightsSchemaVersion is bumped on any incompatible Weights-encoding
// change. Bump invalidates every actor's learned weights → fallback to
// DefaultWeights at next read.
const WeightsSchemaVersion uint8 = 1

// EMARate is alpha from research/04-cortex.md §8.3 ("EMA rate α = 0.05
// (slow)"). Exported so the journal/cortex layers can stamp it on
// LearnWeightsPayload entries for audit-replay determinism, and so a
// future migration can change it without ambiguity over which value any
// given replay step used.
const EMARate float32 = 0.05

// Weights is the per-actor learned weight set persisted at
// meta/salience_weights. WR+WA+WC+WD+WV must sum to 1.0 (renormalised on
// every UpdateWeightsEMA). UpdatedAt + Updates are advisory audit fields,
// not part of the math — the journal-of-record is KindLearnWeights.
type Weights struct {
	SchemaVersion uint8   `cbor:"0,keyasint"`
	WR            float32 `cbor:"1,keyasint"` // recency
	WA            float32 `cbor:"2,keyasint"` // access
	WC            float32 `cbor:"3,keyasint"` // citations
	WD            float32 `cbor:"4,keyasint"` // declared importance
	WV            float32 `cbor:"5,keyasint"` // vector similarity
	UpdatedAt     int64   `cbor:"6,keyasint,omitempty"`
	Updates       uint64  `cbor:"7,keyasint,omitempty"`
}

// DefaultWeights returns the cold-start weights from §8.2.
func DefaultWeights() Weights {
	return Weights{
		SchemaVersion: WeightsSchemaVersion,
		WR:            WR,
		WA:            WA,
		WC:            WC,
		WD:            WD,
		WV:            WV,
	}
}

// EncodeWeights returns canonical deterministic CBOR for w.
func EncodeWeights(w *Weights) ([]byte, error) {
	if w == nil {
		return nil, fmt.Errorf("salience: nil weights")
	}
	return canonicalEnc.Marshal(w)
}

// DecodeWeights parses canonical CBOR into out.
func DecodeWeights(b []byte, out *Weights) error {
	return canonicalDec.Unmarshal(b, out)
}

// ReadWeights fetches the actor's learned weights from meta/salience_weights.
// Returns (DefaultWeights(), false, nil) when the key is absent (cold start).
func ReadWeights(s *store.Store) (Weights, bool, error) {
	raw, ok, err := s.Get(keys.MetaSalienceWeights)
	if err != nil {
		return Weights{}, false, fmt.Errorf("salience: get weights: %w", err)
	}
	if !ok {
		return DefaultWeights(), false, nil
	}
	var w Weights
	if err := DecodeWeights(raw, &w); err != nil {
		return Weights{}, false, fmt.Errorf("salience: decode weights: %w", err)
	}
	return w, true, nil
}

// HalfLifeNanos is the recency time constant from §8.1: 90 days. The spec
// formula is `exp(-Δt / half_life)`; despite the label, this is a 1/e decay
// (R drops to ~0.368 at one constant, not 0.5). Adopting the spec formula
// verbatim keeps cortex_snapshot_hash byte-identical with any independent
// re-implementation. If we ever switch to a strict-half-life decay
// (`exp(-Δt·ln2 / H)`), every salience cache entry needs recomputation —
// that's a journaled migration, not a code change.
const HalfLifeNanos int64 = int64(90 * 24 * time.Hour)

// PinnedFloor is the §8.2 floor for memories with Pinned=true.
const PinnedFloor float32 = 0.7

// AccessSaturation is the denominator constant used to normalize
// access_count and cite_in_successful_plans into [0,1]. We use a fixed
// constant (not per-actor max) because the spec's max-based formula is
// unstable under cold start and creates ranking discontinuities when a
// single memory pulls the max upward. log(1+1000) is the saturation point
// where ranking gain approaches zero in practical loads.
const AccessSaturation = 1000.0

// Score is the cached salience record at salience/<id>.
//
// All factor inputs are stored alongside the cached score so a recompute
// triggered by a single input change (e.g. attestation citation update) is
// O(1) without needing to re-scan the head/version.
type Score struct {
	SchemaVersion uint8   `cbor:"0,keyasint"`
	LastUsed      int64   `cbor:"1,keyasint"`           // unix nano
	AccessCount   uint64  `cbor:"2,keyasint,omitempty"` // bumps on read access (Phase 3+)
	Citations     uint64  `cbor:"3,keyasint,omitempty"` // cite_in_successful_plans
	Importance    uint8   `cbor:"4,keyasint,omitempty"` // mirrors Head.DeclaredImportance
	Pinned        bool    `cbor:"5,keyasint,omitempty"`
	Cached        float32 `cbor:"6,keyasint"` // last computed cold-formula value
	ComputedAt    int64   `cbor:"7,keyasint"` // unix nano
}

// canonicalEnc / canonicalDec ensure Score encodes deterministically so the
// cached bytes hash identically across processes (matters for snapshot
// integrity in Phase 7).
var (
	canonicalEnc cbor.EncMode
	canonicalDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("salience: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("salience: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// Encode returns the canonical CBOR encoding of s.
func Encode(s *Score) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("salience: nil score")
	}
	return canonicalEnc.Marshal(s)
}

// Decode parses canonical CBOR into out.
func Decode(b []byte, out *Score) error {
	return canonicalDec.Unmarshal(b, out)
}

// recency returns R(m) at the given clock.
func recency(lastUsedUnixNano int64, now time.Time) float32 {
	dt := now.UnixNano() - lastUsedUnixNano
	if dt < 0 {
		dt = 0
	}
	r := math.Exp(-float64(dt) / float64(HalfLifeNanos))
	return float32(r)
}

// logSat returns log(1+x) / log(1+saturation), clamped to [0,1].
func logSat(x uint64) float32 {
	v := math.Log(1+float64(x)) / math.Log(1+AccessSaturation)
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return float32(v)
}

// ColdScore computes salience(m) using the cold (default) weights with V
// skipped (no q.near). Preserved for source-compat with Phase 3+ callers;
// internally delegates to ColdScoreWith.
//
// Live ranking SHOULD prefer ColdScoreWith with the actor's learned
// Weights (loaded via ReadWeights) so EMA updates are honored. Bumpers
// (BumpForAccess/BumpForCitation/etc.) intentionally use this default-
// weights path for sc.Cached: per Phase 12 Q4, sc.Cached is a debug
// snapshot, not the live ranking value.
func ColdScore(s *Score, now time.Time) float32 {
	w := DefaultWeights()
	return ColdScoreWith(s, w, now)
}

// ColdScoreWith computes salience(m) using the supplied weights with V
// skipped (q.near unset). The non-vector weights are renormalised over
// their own sum so the final score lives in [0,1] regardless of what
// EMA has done to WV.
//
// §8.2 "vector weight gating" rule when q.near is unset: w5 is
// redistributed across the remaining weights proportionally. Equivalent
// to dividing the non-vector component by (WR+WA+WC+WD). Done here so
// the formula is symmetric for both default and learned weights.
//
// Pinned memories floor at PinnedFloor.
func ColdScoreWith(s *Score, w Weights, now time.Time) float32 {
	if s == nil {
		return 0
	}
	r := recency(s.LastUsed, now)
	a := logSat(s.AccessCount)
	c := logSat(s.Citations)
	d := float32(s.Importance) / 10.0
	if d > 1 {
		d = 1
	}
	denom := w.WR + w.WA + w.WC + w.WD // V redistributes; never 0 if Weights valid
	if denom <= 0 {
		denom = 1 // defensive — should never happen for renormalised weights
	}
	score := (w.WR*r + w.WA*a + w.WC*c + w.WD*d) / denom
	if s.Pinned && score < PinnedFloor {
		score = PinnedFloor
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}

// factorProfile returns the (R, A, C, D) factor contributions for one
// Score at the given clock, normalised against the cold-weight denominator.
// V is omitted (Phase 12 trains four-factor only; vector signal lands when
// V-aware Find ranking lands in a later phase). Returned slice always has
// length 4 in order WR, WA, WC, WD.
func factorProfile(s *Score, now time.Time) [4]float32 {
	r := recency(s.LastUsed, now)
	a := logSat(s.AccessCount)
	c := logSat(s.Citations)
	d := float32(s.Importance) / 10.0
	if d > 1 {
		d = 1
	}
	return [4]float32{r, a, c, d}
}

// UpdateWeightsEMA applies one EMA step to w in place, training toward (on
// success) or away from (on failure with reason ∈ §8.3 decrement set) the
// average per-factor profile of citedScores at the given clock.
//
// Spec: research/04-cortex.md §8.3 ("EMA-update the actor's weights
// toward the high-performing weighting" / "away from the weighting that
// ranked the bad memory highly"). alpha is the EMA rate (typically
// EMARate = 0.05). decrementOnFailure indicates whether the (outcome,
// reason) pair triggers the away-pull (i.e. matches §8.3 decrement set);
// when false (success or any other failure reason) the toward-pull is
// applied.
//
// Algorithm:
//  1. profile_i = average over citedScores of factor i (i ∈ {R,A,C,D})
//  2. normalise profile so sum_i profile_i = 1; if sum_i == 0 (all
//     cited memories have zero factor inputs — degenerate cold-start
//     case) skip the update entirely and return false.
//  3. delta_i = profile_i - w_i
//  4. w_i_new = w_i + α·delta_i           (toward)
//     or       = w_i - α·delta_i          (away)
//  5. Clamp w_i_new ≥ 0; if any clamp fired we still proceed to renormalise.
//  6. Renormalise: w_i_new /= Σ_j w_j_new (V preserved exactly).
//
// The V (vector) weight is left at its current value through this entire
// step — Phase 12 trains four-factor only. Renormalisation step (6) does
// include w.WV in the denominator so the full 5-weight sum stays at 1.0.
//
// Returns true iff an update was applied; false on degenerate input
// (empty citedScores or all-zero profile). UpdatedAt is advanced and
// Updates incremented when an update IS applied.
func UpdateWeightsEMA(w *Weights, citedScores []Score, alpha float32, decrementOnFailure bool, now time.Time) bool {
	if w == nil || len(citedScores) == 0 || alpha <= 0 {
		return false
	}

	// 1. Average per-factor profile across cited memories.
	var sum [4]float32
	for i := range citedScores {
		p := factorProfile(&citedScores[i], now)
		sum[0] += p[0]
		sum[1] += p[1]
		sum[2] += p[2]
		sum[3] += p[3]
	}
	n := float32(len(citedScores))
	avg := [4]float32{sum[0] / n, sum[1] / n, sum[2] / n, sum[3] / n}

	// 2. Normalise profile so sum = 1.
	avgSum := avg[0] + avg[1] + avg[2] + avg[3]
	if avgSum <= 0 {
		return false
	}
	profile := [4]float32{
		avg[0] / avgSum,
		avg[1] / avgSum,
		avg[2] / avgSum,
		avg[3] / avgSum,
	}

	// 3+4. Pull weights toward (or away from) profile.
	cur := [4]float32{w.WR, w.WA, w.WC, w.WD}
	next := [4]float32{}
	sign := float32(1)
	if decrementOnFailure {
		sign = -1
	}
	for i := 0; i < 4; i++ {
		delta := profile[i] - cur[i]
		v := cur[i] + sign*alpha*delta
		if v < 0 {
			v = 0
		}
		next[i] = v
	}

	// 6. Renormalise the full 5-weight sum (R+A+C+D+V) to 1.
	newSum := next[0] + next[1] + next[2] + next[3] + w.WV
	if newSum <= 0 {
		// Pathological — every learned weight collapsed to zero. Refuse
		// the update rather than divide-by-zero; falls back to current.
		return false
	}
	w.WR = next[0] / newSum
	w.WA = next[1] / newSum
	w.WC = next[2] / newSum
	w.WD = next[3] / newSum
	w.WV = w.WV / newSum
	w.UpdatedAt = now.UnixNano()
	w.Updates++
	return true
}

// NewForWrite builds an initial Score for a freshly-written memory at the
// given clock. AccessCount and Citations are zero; LastUsed = now.
func NewForWrite(importance uint8, now time.Time) Score {
	s := Score{
		SchemaVersion: SchemaVersion,
		LastUsed:      now.UnixNano(),
		Importance:    importance,
		ComputedAt:    now.UnixNano(),
	}
	s.Cached = ColdScore(&s, now)
	return s
}

// BumpForUpdate refreshes a Score after a memory's Data was updated.
// LastUsed advances to now and the cached value is recomputed.
func BumpForUpdate(s *Score, importance uint8, now time.Time) {
	s.LastUsed = now.UnixNano()
	s.Importance = importance
	s.Cached = ColdScore(s, now)
	s.ComputedAt = now.UnixNano()
}

// BumpForAccess increments AccessCount, advances LastUsed, and recomputes
// the cached cold score. Fired per returned candidate from a LateBinding
// Find (research/04-cortex.md §8.1 input `access_count` + §8.4 cache
// invalidation trigger). AccessCount saturates at math.MaxUint64; the
// logSat normalizer floors the score contribution at 1.0 well before
// then, so we just refuse to wrap rather than journal an overflow.
//
// Phase 11.5: this is the only path that feeds salience.AccessCount.
// Compile-time Find does NOT call this (research/04 §12 + Phase 3
// invariant: compile-time Find does not journal); compile-time access
// gets accounted for downstream via cortex.Attest cited_uris.
func BumpForAccess(s *Score, now time.Time) {
	if s.AccessCount != ^uint64(0) {
		s.AccessCount++
	}
	s.LastUsed = now.UnixNano()
	s.Cached = ColdScore(s, now)
	s.ComputedAt = now.UnixNano()
}

// BumpForCitation increments Citations AND AccessCount, advances LastUsed,
// and recomputes the cached cold score. Fired per cited URI from a
// successful cortex.Attest (research/04-cortex.md §8.3 verbatim: "For
// each `cited` memory in the plan, increment `cite_in_successful_plans`").
//
// AccessCount also goes up because a citation implies an access — the
// agent had to read the memory to cite it. Bundling the two bumps in a
// single primitive keeps the salience cache consistent across the
// Find→Attest path: a memory found via late-binding Find and then cited
// in a successful attest has both signals reflected.
func BumpForCitation(s *Score, now time.Time) {
	if s.Citations != ^uint64(0) {
		s.Citations++
	}
	if s.AccessCount != ^uint64(0) {
		s.AccessCount++
	}
	s.LastUsed = now.UnixNano()
	s.Cached = ColdScore(s, now)
	s.ComputedAt = now.UnixNano()
}

// DecrementCitation decrements Citations (floored at zero), advances
// LastUsed, and recomputes the cached cold score. Fired per cited URI
// from a failed cortex.Attest whose reason ∈ {factual_error,
// wrong_assumption} (research/04-cortex.md §8.3: "For memories cited
// at the failed step, *decrement* `cite_in_successful_plans`").
//
// AccessCount is NOT decremented — the access happened, even if the
// downstream use was wrong. Only the success-citation signal flips.
//
// LastUsed advances because the attest action is itself a touch of
// these memories (the agent had them in context at attest time); this
// matches BumpForCitation symmetry and avoids the "decrement zeros R"
// surprise where a recently-cited-but-failed memory would otherwise
// stay at its pre-attest recency.
func DecrementCitation(s *Score, now time.Time) {
	if s.Citations > 0 {
		s.Citations--
	}
	s.LastUsed = now.UnixNano()
	s.Cached = ColdScore(s, now)
	s.ComputedAt = now.UnixNano()
}

// ZeroForTombstone collapses a Score to a tombstoned-equivalent state. We
// keep the record (as opposed to deleting it) so that operational replay
// from journal+m/+mv/ produces a byte-identical salience namespace; deleting
// would require an explicit tombstone marker in the salience namespace too,
// which is unnecessary complexity.
func ZeroForTombstone(s *Score, now time.Time) {
	s.Cached = 0
	s.ComputedAt = now.UnixNano()
}

// Read fetches the salience record for id. Returns (nil, false, nil) if
// absent (e.g. memories written before Phase 3 wiring; planner falls back
// to a zero score).
func Read(s *store.Store, id memory.ID) (*Score, bool, error) {
	var u keys.ULID
	copy(u[:], id[:])
	raw, ok, err := s.Get(keys.SalienceKey(u))
	if err != nil {
		return nil, false, fmt.Errorf("salience: get: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	var sc Score
	if err := Decode(raw, &sc); err != nil {
		return nil, false, fmt.Errorf("salience: decode: %w", err)
	}
	return &sc, true, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
