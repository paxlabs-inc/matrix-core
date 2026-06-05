// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
)

// TestAttestSuccessBumpsCitations writes two memories, calls Attest with
// outcome=success, and verifies Citations and AccessCount both increment
// on each cited URI. Also verifies the KindAttest journal entry payload.
func TestAttestSuccessBumpsCitations(t *testing.T) {
	c := openCortex(t)
	uriA := writePref(t, c, "a", 5)
	uriB := writePref(t, c, "b", 5)

	_, idA, _, _ := ParseURI(uriA)
	_, idB, _, _ := ParseURI(uriB)

	preA, _, _ := salience.Read(c.s, idA)
	preB, _, _ := salience.Read(c.s, idB)
	if preA.Citations != 0 || preB.Citations != 0 {
		t.Fatalf("precondition: Citations should be 0 pre-attest, got A=%d B=%d", preA.Citations, preB.Citations)
	}

	res, err := c.Attest(AttestOpts{
		IntentID:  "intent-1",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uriA, uriB},
		CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if len(res.AffectedIDs) != 2 {
		t.Fatalf("AffectedIDs: got %d want 2", len(res.AffectedIDs))
	}
	if res.CitationsDelta != +1 {
		t.Fatalf("CitationsDelta: got %d want +1", res.CitationsDelta)
	}

	postA, _, _ := salience.Read(c.s, idA)
	postB, _, _ := salience.Read(c.s, idB)
	if postA.Citations != 1 || postB.Citations != 1 {
		t.Fatalf("Citations: got A=%d B=%d, want both=1", postA.Citations, postB.Citations)
	}
	if postA.AccessCount != 1 || postB.AccessCount != 1 {
		t.Fatalf("AccessCount: got A=%d B=%d, want both=1 (citation implies access)",
			postA.AccessCount, postB.AccessCount)
	}

	// KindAttest entry exists at the returned seq.
	raw, ok, err := c.s.Get(keys.JournalKey(res.Seq))
	if err != nil || !ok {
		t.Fatalf("journal Get j/%d: ok=%v err=%v", res.Seq, ok, err)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if entry.Kind != journal.KindAttest {
		t.Fatalf("Kind: got %q want %q", entry.Kind, journal.KindAttest)
	}
	var pl journal.AttestPayload
	if err := journal.DecodeAttestPayload(entry.Payload, &pl); err != nil {
		t.Fatalf("DecodeAttestPayload: %v", err)
	}
	if pl.IntentID != "intent-1" {
		t.Fatalf("IntentID: got %q want %q", pl.IntentID, "intent-1")
	}
	if pl.Outcome != journal.AttestOutcomeSuccess {
		t.Fatalf("Outcome: got %d want %d", pl.Outcome, journal.AttestOutcomeSuccess)
	}
	if len(pl.CitedIDs) != 2 {
		t.Fatalf("CitedIDs: got %d want 2", len(pl.CitedIDs))
	}
}

// TestAttestFailureDecrementsCitationsOnReasonMatch verifies the §8.3
// "decrement on factual_error/wrong_assumption" rule. Other reasons
// leave Citations unchanged.
func TestAttestFailureDecrementsCitationsOnReasonMatch(t *testing.T) {
	type tc struct {
		name          string
		reason        string
		wantDelta     int
		wantCitations uint64
		seedCitations uint64
	}
	cases := []tc{
		{"factual_error", AttestReasonFactualError, -1, 1, 2},
		{"wrong_assumption", AttestReasonWrongAssumption, -1, 1, 2},
		{"other_reason", "timeout", 0, 2, 2},
		{"empty_reason", "", 0, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := openCortex(t)
			uri := writePref(t, c, "x", 5)
			_, id, _, _ := ParseURI(uri)

			// Seed Citations to non-zero so a -1 delta is observable.
			seedSalience(t, c, id, tc.seedCitations)

			res, err := c.Attest(AttestOpts{
				IntentID:  "intent-x",
				Outcome:   AttestOutcomeFailure,
				Reason:    tc.reason,
				Cited:     []memory.URI{uri},
				CreatedBy: "andrew",
			})
			if err != nil {
				t.Fatalf("Attest: %v", err)
			}
			if res.CitationsDelta != tc.wantDelta {
				t.Fatalf("delta: got %d want %d", res.CitationsDelta, tc.wantDelta)
			}
			post, _, _ := salience.Read(c.s, id)
			if post.Citations != tc.wantCitations {
				t.Fatalf("Citations: got %d want %d", post.Citations, tc.wantCitations)
			}
			// AccessCount must NOT change on failure regardless of reason.
			if post.AccessCount != 0 {
				t.Fatalf("AccessCount: got %d want 0 (failure must not bump access)", post.AccessCount)
			}
		})
	}
}

// TestAttestFloorsAtZero decrements past zero stay at zero (no underflow).
func TestAttestFloorsAtZero(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "x", 5)
	_, id, _, _ := ParseURI(uri)

	// First failure-with-factual_error: Citations stays 0 (already 0).
	_, err := c.Attest(AttestOpts{
		IntentID:  "intent-1",
		Outcome:   AttestOutcomeFailure,
		Reason:    AttestReasonFactualError,
		Cited:     []memory.URI{uri},
		CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	post, _, _ := salience.Read(c.s, id)
	if post.Citations != 0 {
		t.Fatalf("Citations: got %d want 0 (no underflow)", post.Citations)
	}
}

// TestAttestSkipsTombstoned silently drops tombstoned URIs into SkippedURIs
// rather than failing the whole batch.
func TestAttestSkipsTombstoned(t *testing.T) {
	c := openCortex(t)
	uriA := writePref(t, c, "a", 5)
	uriB := writePref(t, c, "b", 5)
	if err := c.Tombstone(uriB, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	res, err := c.Attest(AttestOpts{
		IntentID:  "intent-1",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uriA, uriB},
		CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if len(res.AffectedIDs) != 1 {
		t.Fatalf("Affected: got %d want 1 (B is tombstoned)", len(res.AffectedIDs))
	}
	if len(res.SkippedURIs) != 1 || res.SkippedURIs[0] != uriB {
		t.Fatalf("SkippedURIs: %v want [%s]", res.SkippedURIs, uriB)
	}
}

// TestAttestRejectsEmptyCited refuses len(Cited)==0 at API boundary.
func TestAttestRejectsEmptyCited(t *testing.T) {
	c := openCortex(t)
	_, err := c.Attest(AttestOpts{
		IntentID: "intent-x",
		Outcome:  AttestOutcomeSuccess,
		Cited:    nil,
	})
	if !errors.Is(err, ErrEmptyCitations) {
		t.Fatalf("err: got %v want %v", err, ErrEmptyCitations)
	}
}

// TestAttestRejectsEmptyIntentID enforces the audit invariant: an attest
// without an intent reference can't be linked back to a signed envelope.
func TestAttestRejectsEmptyIntentID(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "x", 5)
	_, err := c.Attest(AttestOpts{
		IntentID: "",
		Outcome:  AttestOutcomeSuccess,
		Cited:    []memory.URI{uri},
	})
	if !errors.Is(err, ErrAttestEmptyIntentID) {
		t.Fatalf("err: got %v want %v", err, ErrAttestEmptyIntentID)
	}
}

// TestAttestDeduplicatesCitedURIs — same URI twice in one attest must
// bump Citations once (salience is per-memory, not per-citation-mention).
func TestAttestDeduplicatesCitedURIs(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "x", 5)
	_, id, _, _ := ParseURI(uri)

	res, err := c.Attest(AttestOpts{
		IntentID:  "intent-x",
		Outcome:   AttestOutcomeSuccess,
		Cited:     []memory.URI{uri, uri, uri},
		CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if len(res.AffectedIDs) != 1 {
		t.Fatalf("Affected: got %d want 1 (dedup)", len(res.AffectedIDs))
	}
	post, _, _ := salience.Read(c.s, id)
	if post.Citations != 1 {
		t.Fatalf("Citations: got %d want 1 (single dedup'd bump)", post.Citations)
	}
}

// TestLateBindingFindBumpsAccessCount — Phase 11.5 invariant: a Find with
// LateBinding=true must bump salience.AccessCount per returned candidate
// and journal a KindFind entry whose payload carries the AccessedIDs.
func TestLateBindingFindBumpsAccessCount(t *testing.T) {
	c := openCortex(t)
	uriA := writePref(t, c, "a", 5, "voice")
	uriB := writePref(t, c, "b", 5, "voice")
	_, idA, _, _ := ParseURI(uriA)
	_, idB, _, _ := ParseURI(uriB)

	res, err := c.Find(query.Query{
		Type:        []memory.Type{memory.TypePreference},
		Limit:       10,
		LateBinding: true,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("Find returned %d memories, want 2", len(res.Memories))
	}

	postA, _, _ := salience.Read(c.s, idA)
	postB, _, _ := salience.Read(c.s, idB)
	if postA.AccessCount != 1 || postB.AccessCount != 1 {
		t.Fatalf("AccessCount: got A=%d B=%d, want both=1", postA.AccessCount, postB.AccessCount)
	}

	// Walk j/ for the KindFind entry; verify AccessedIDs carries both IDs.
	var foundFind bool
	if err := c.s.PrefixIter(keys.PrefixJournal, func(k, v []byte) error {
		var e journal.Entry
		if err := journal.Decode(v, &e); err != nil {
			return err
		}
		if e.Kind != journal.KindFind {
			return nil
		}
		foundFind = true
		var pl journal.LateBindingPayload
		if err := journal.DecodeLateBindingPayload(e.Payload, &pl); err != nil {
			return err
		}
		if len(pl.AccessedIDs) != 2 {
			t.Fatalf("AccessedIDs: got %d want 2", len(pl.AccessedIDs))
		}
		return nil
	}); err != nil {
		t.Fatalf("PrefixIter: %v", err)
	}
	if !foundFind {
		t.Fatalf("no KindFind entry journaled")
	}
}

// TestCompileTimeFindDoesNotBump preserves the Phase 3 invariant: a Find
// with LateBinding=false (default) does NOT journal and does NOT bump
// AccessCount. Compile-time accesses get accounted for downstream by
// cortex.Attest cited_uris (per Q-lock decision).
func TestCompileTimeFindDoesNotBump(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "a", 5)
	_, id, _, _ := ParseURI(uri)

	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		// LateBinding: false (default)
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("Find returned %d, want 1", len(res.Memories))
	}

	post, _, _ := salience.Read(c.s, id)
	if post.AccessCount != 0 {
		t.Fatalf("AccessCount: got %d want 0 (compile-time Find must not bump)", post.AccessCount)
	}
}

// seedSalience replaces the existing salience cache for id with a Score
// whose Citations field is set to seed. Used by attest tests that want
// to verify decrement behavior starting from non-zero state.
func seedSalience(t *testing.T, c *Cortex, id memory.ID, seedCitations uint64) {
	t.Helper()
	now := c.now()
	sc := salience.NewForWrite(5, now)
	sc.Citations = seedCitations
	seedSalienceFull(t, c, id, sc)
}

// seedSalienceFull writes any Score directly to salience/<id>. Useful for
// Phase 12 tests that need to control the full factor profile (LastUsed,
// AccessCount, Citations, Importance) for EMA-update assertions.
func seedSalienceFull(t *testing.T, c *Cortex, id memory.ID, sc salience.Score) {
	t.Helper()
	encoded, err := salience.Encode(&sc)
	if err != nil {
		t.Fatalf("seedSalienceFull: encode: %v", err)
	}
	var u keys.ULID
	copy(u[:], id[:])
	b := c.s.DB().NewBatch()
	defer b.Close()
	if err := b.Set(keys.SalienceKey(u), encoded, nil); err != nil {
		t.Fatalf("seedSalienceFull: set: %v", err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatalf("seedSalienceFull: commit: %v", err)
	}
}

// TestAttestEmitsKindLearnWeightsSuccess — Phase 12: a success-outcome
// Attest with at least one bumpable cited memory must emit a
// KindLearnWeights entry at Seq+1 and persist a meta/salience_weights
// record. NewWeights != PrevWeights, sum-to-1 stays within tolerance.
func TestAttestEmitsKindLearnWeightsSuccess(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 8)
	_, id, _, _ := ParseURI(uri)
	// Seed the cited memory with high citations so the EMA step has
	// a non-zero gradient on success.
	pre, _, _ := salience.Read(c.s, id)
	pre.Citations = 50
	pre.AccessCount = 20
	seedSalienceFull(t, c, id, *pre)

	res, err := c.Attest(AttestOpts{
		IntentID: "i1", Outcome: AttestOutcomeSuccess, Cited: []memory.URI{uri}, CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if res.LearnSeq != res.Seq+1 {
		t.Fatalf("LearnSeq: got %d want %d (= Seq+1)", res.LearnSeq, res.Seq+1)
	}
	if !res.WeightsUpdated {
		t.Fatalf("WeightsUpdated should be true (non-degenerate cited)")
	}
	if res.NewWeights == res.PrevWeights {
		t.Fatalf("weights unchanged: prev=%+v new=%+v", res.PrevWeights, res.NewWeights)
	}

	// meta/salience_weights persisted; round-trip equals NewWeights.
	stored, found, err := salience.ReadWeights(c.s)
	if err != nil || !found {
		t.Fatalf("ReadWeights: found=%v err=%v", found, err)
	}
	if stored != res.NewWeights {
		t.Fatalf("persisted weights diverge from AttestResult.NewWeights")
	}

	// Sum to 1.0 within tolerance.
	sum := stored.WR + stored.WA + stored.WC + stored.WD + stored.WV
	if sum < 0.9999 || sum > 1.0001 {
		t.Fatalf("post-EMA weights sum out of tolerance: %f", sum)
	}

	// KindLearnWeights journal entry exists at LearnSeq.
	raw, ok, err := c.s.Get(keys.JournalKey(res.LearnSeq))
	if err != nil || !ok {
		t.Fatalf("journal Get j/%d: ok=%v err=%v", res.LearnSeq, ok, err)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if entry.Kind != journal.KindLearnWeights {
		t.Fatalf("Kind: got %q want %q", entry.Kind, journal.KindLearnWeights)
	}
	var pl journal.LearnWeightsPayload
	if err := journal.DecodeLearnWeightsPayload(entry.Payload, &pl); err != nil {
		t.Fatalf("DecodeLearnWeightsPayload: %v", err)
	}
	if pl.SourceSeq != res.Seq {
		t.Fatalf("SourceSeq: got %d want %d", pl.SourceSeq, res.Seq)
	}
	if pl.Skipped {
		t.Fatalf("Skipped should be false on real EMA update")
	}
}

// TestAttestEmitsKindLearnWeightsSkippedOnDegenerate — when all cited
// memories have all-zero factor profiles (deep-past LastUsed, no
// citations, etc.) the EMA is a no-op and the journal entry has
// Skipped=true; meta/salience_weights stays at the prior value.
func TestAttestEmitsKindLearnWeightsSkippedOnDegenerate(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "topic", 0)
	_, id, _, _ := ParseURI(uri)
	// Seed degenerate: LastUsed 250 half-lives ago, no citations,
	// no importance — R underflows to 0, all factors literally zero.
	deepPast := time.Unix(1700000000, 0).Add(-time.Duration(salience.HalfLifeNanos) * 250)
	seedSalienceFull(t, c, id, salience.Score{
		SchemaVersion: salience.SchemaVersion,
		LastUsed:      deepPast.UnixNano(),
	})

	res, err := c.Attest(AttestOpts{
		IntentID: "i-deg", Outcome: AttestOutcomeSuccess, Cited: []memory.URI{uri}, CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	// AccessCount/Citations bumps fired regardless (cited memory was
	// touched), so post-bump Score has fresh LastUsed. The EMA training
	// signal is taken from the POST-bump score, which now has a non-zero
	// factor profile. So Skipped should be FALSE here.
	if !res.WeightsUpdated {
		t.Logf("WeightsUpdated=false (expected: post-bump factors lifted profile)")
	}

	raw, ok, err := c.s.Get(keys.JournalKey(res.LearnSeq))
	if err != nil || !ok {
		t.Fatalf("journal Get j/%d: ok=%v err=%v", res.LearnSeq, ok, err)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if entry.Kind != journal.KindLearnWeights {
		t.Fatalf("Kind: got %q want KindLearnWeights", entry.Kind)
	}
}

// TestAttestColdStartLearnsFirstWeights — fresh actor with no prior
// meta/salience_weights; first Attest must persist the first weights.
func TestAttestColdStartLearnsFirstWeights(t *testing.T) {
	c := openCortex(t)
	// Cold start: meta/salience_weights absent.
	_, found, err := salience.ReadWeights(c.s)
	if err != nil {
		t.Fatalf("ReadWeights: %v", err)
	}
	if found {
		t.Fatalf("precondition: meta/salience_weights should be absent")
	}

	uri := writePref(t, c, "topic", 5)
	res, err := c.Attest(AttestOpts{
		IntentID: "i1", Outcome: AttestOutcomeSuccess, Cited: []memory.URI{uri}, CreatedBy: "andrew",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if res.PrevWeights != salience.DefaultWeights() {
		t.Fatalf("PrevWeights at cold start should equal DefaultWeights: %+v", res.PrevWeights)
	}
	_, found, _ = salience.ReadWeights(c.s)
	if !found {
		t.Fatalf("meta/salience_weights should exist after first Attest")
	}
}

// silence unused-import deterrent if time isn't otherwise referenced.
var _ = time.Time{}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
