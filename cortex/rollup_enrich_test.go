// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
)

// templatingEnricher is a REAL, deterministic Enricher (not a canned-return
// fake): it genuinely transforms the deterministic ShortForm and the record's
// tallies/members into prose. Same inputs → same output bytes, so it exercises
// the rebuildable + determinism properties of the enrichment lane honestly.
type templatingEnricher struct{ model string }

func (e templatingEnricher) Enrich(shortForm string, rec *cortex.RollupRecord) (string, string, error) {
	if shortForm == "" {
		return "", "", fmt.Errorf("templatingEnricher: empty short-form")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "During the %s window there were %d recorded events.",
		rec.Window.Tier.String(), rec.EntryCount)

	// Render kind tally with sorted keys (deterministic).
	if len(rec.KindTally) > 0 {
		ks := make([]string, 0, len(rec.KindTally))
		for k := range rec.KindTally {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		b.WriteString(" Activity breakdown:")
		for _, k := range ks {
			fmt.Fprintf(&b, " %d %s;", rec.KindTally[k], k)
		}
	}
	if len(rec.Members) > 0 {
		fmt.Fprintf(&b, " Most salient item: %s.", rec.Members[0].URI)
	}
	b.WriteString(" (derived from: ")
	b.WriteString(shortForm)
	b.WriteString(")")
	return b.String(), e.model, nil
}

// buildEnrichableHourRollup seeds real journal entries in a known hour window
// and builds the hour rollup, returning the window.
func buildEnrichableHourRollup(t *testing.T, c *cortex.Cortex, clk *mutClock) cortex.Window {
	t.Helper()
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	topURI := writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "beta", 5)
	clk.t = baseHour.Add(2 * time.Minute)
	if _, err := c.Attest(cortex.AttestOpts{
		IntentID:  "intent-enrich",
		Outcome:   cortex.AttestOutcomeSuccess,
		Cited:     []memory.URI{topURI},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}
	return w
}

// TestEnrichRollupBasic proves the happy path: EnrichRollup persists an
// EnrichRecord derived from the rollup's ShortForm; LoadEnrichment round-trips
// it with non-empty prose + SourceRecordHash == the rollup's RecordHash; and
// LoadRollupEnriched surfaces the enrichment URI as EnrichRef (req.3.4).
func TestEnrichRollupBasic(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := buildEnrichableHourRollup(t, c, clk)

	enr := templatingEnricher{model: "test-templater@v1"}
	uri, err := c.EnrichRollup(cortex.TierHour, w.Start, enr)
	if err != nil {
		t.Fatalf("EnrichRollup: %v", err)
	}
	wantURI := cortex.BuildEnrichURI(cortex.TierHour, w.Start)
	if uri != wantURI {
		t.Fatalf("EnrichRollup uri = %q, want %q", uri, wantURI)
	}

	er, err := c.LoadEnrichment(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadEnrichment: %v", err)
	}
	if er.Prose == "" {
		t.Fatalf("EnrichRecord.Prose empty")
	}
	if er.Model != "test-templater@v1" {
		t.Fatalf("EnrichRecord.Model = %q, want test-templater@v1", er.Model)
	}
	if er.Window != w {
		t.Fatalf("EnrichRecord.Window = %+v, want %+v", er.Window, w)
	}

	// SourceRecordHash must equal the rollup's current RecordHash.
	rollup, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup: %v", err)
	}
	rb, err := cortex.EncodeRollupRecord(rollup)
	if err != nil {
		t.Fatalf("encode rollup: %v", err)
	}
	if got := sha256.Sum256(rb); got != er.SourceRecordHash {
		t.Fatalf("SourceRecordHash = %x, want rollup RecordHash %x", er.SourceRecordHash, got)
	}

	// LoadRollupEnriched sets EnrichRef in-memory.
	enriched, err := c.LoadRollupEnriched(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollupEnriched: %v", err)
	}
	if enriched.EnrichRef != string(wantURI) {
		t.Fatalf("EnrichRef = %q, want %q", enriched.EnrichRef, wantURI)
	}
}

// TestEnrichRollupDeterministicFloor proves req.4.3 "never load-bearing": the
// STORED RollupRecord is UNCHANGED by enrichment — its bytes + RecordHash are
// byte-identical before and after EnrichRollup — and re-running EnrichRollup
// with the same deterministic enricher yields a byte-identical EnrichRecord.
func TestEnrichRollupDeterministicFloor(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := buildEnrichableHourRollup(t, c, clk)

	before, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup before: %v", err)
	}
	beforeBytes, err := cortex.EncodeRollupRecord(before)
	if err != nil {
		t.Fatalf("encode before: %v", err)
	}

	enr := templatingEnricher{model: "test-templater@v1"}
	if _, err := c.EnrichRollup(cortex.TierHour, w.Start, enr); err != nil {
		t.Fatalf("EnrichRollup #1: %v", err)
	}

	after, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup after: %v", err)
	}
	afterBytes, err := cortex.EncodeRollupRecord(after)
	if err != nil {
		t.Fatalf("encode after: %v", err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("stored RollupRecord bytes changed by enrichment:\n before %x\n after  %x", beforeBytes, afterBytes)
	}
	if after.EnrichRef != "" {
		t.Fatalf("stored RollupRecord.EnrichRef = %q, want empty (never persisted)", after.EnrichRef)
	}

	// First enrichment record bytes.
	er1, err := c.LoadEnrichment(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadEnrichment #1: %v", err)
	}
	e1, err := cortex.EncodeEnrichRecord(er1)
	if err != nil {
		t.Fatalf("encode er1: %v", err)
	}

	// Re-run with the same deterministic enricher (advance wall clock to
	// prove wall-now does not leak into the EnrichRecord bytes).
	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if _, err := c.EnrichRollup(cortex.TierHour, w.Start, enr); err != nil {
		t.Fatalf("EnrichRollup #2: %v", err)
	}
	er2, err := c.LoadEnrichment(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadEnrichment #2: %v", err)
	}
	e2, err := cortex.EncodeEnrichRecord(er2)
	if err != nil {
		t.Fatalf("encode er2: %v", err)
	}
	if !bytes.Equal(e1, e2) {
		t.Fatalf("EnrichRecord not byte-identical across rebuilds:\n #1 %x\n #2 %x", e1, e2)
	}
}

// TestLoadRollupEnrichedAbsent proves req.3.4: with NO enrichment, the rollup
// is returned with EnrichRef == "" and its deterministic ShortForm intact.
func TestLoadRollupEnrichedAbsent(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := buildEnrichableHourRollup(t, c, clk)

	if _, err := c.LoadEnrichment(cortex.TierHour, w.Start); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("LoadEnrichment(absent) err = %v, want memory.ErrNotFound", err)
	}

	rec, err := c.LoadRollupEnriched(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollupEnriched: %v", err)
	}
	if rec.EnrichRef != "" {
		t.Fatalf("EnrichRef = %q, want empty (no enrichment)", rec.EnrichRef)
	}
	if rec.ShortForm == "" {
		t.Fatalf("deterministic ShortForm empty")
	}
}

// TestEnrichmentStaleness proves req.4.3 (enrichment never load-bearing): after
// enriching, rebuilding the SAME window with DIFFERENT content changes the
// rollup's RecordHash, so the enrichment's SourceRecordHash no longer matches
// and LoadRollupEnriched treats it as stale (EnrichRef == "") — the
// deterministic short-form stands.
func TestEnrichmentStaleness(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := buildEnrichableHourRollup(t, c, clk)

	enr := templatingEnricher{model: "test-templater@v1"}
	if _, err := c.EnrichRollup(cortex.TierHour, w.Start, enr); err != nil {
		t.Fatalf("EnrichRollup: %v", err)
	}

	// Sanity: enrichment is live before the rollup content changes.
	pre, err := c.LoadRollupEnriched(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollupEnriched pre: %v", err)
	}
	if pre.EnrichRef == "" {
		t.Fatalf("EnrichRef empty before content change, want set")
	}

	// Add a NEW in-window entry, then rebuild the SAME window. BuildRollup is
	// idempotent per store state, so the extra entry changes EntryCount /
	// tallies / members → a different RollupRecord + RecordHash.
	clk.t = baseHour.Add(5 * time.Minute)
	writePrefAt(t, c, "gamma", 7)
	clk.t = baseHour.Add(120 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup rebuild: %v", err)
	}

	rebuilt, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup rebuilt: %v", err)
	}
	if rebuilt.EntryCount == pre.EntryCount {
		t.Fatalf("rebuild did not change EntryCount (%d); staleness test is vacuous", rebuilt.EntryCount)
	}

	// The enrichment is now stale: SourceRecordHash mismatches → EnrichRef "".
	post, err := c.LoadRollupEnriched(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollupEnriched post: %v", err)
	}
	if post.EnrichRef != "" {
		t.Fatalf("EnrichRef = %q, want empty (stale enrichment)", post.EnrichRef)
	}
	if post.ShortForm == "" {
		t.Fatalf("deterministic ShortForm empty after staleness")
	}

	// Re-enriching over the rebuilt rollup makes it live again.
	if _, err := c.EnrichRollup(cortex.TierHour, w.Start, enr); err != nil {
		t.Fatalf("EnrichRollup re-enrich: %v", err)
	}
	again, err := c.LoadRollupEnriched(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollupEnriched again: %v", err)
	}
	if again.EnrichRef != string(cortex.BuildEnrichURI(cortex.TierHour, w.Start)) {
		t.Fatalf("EnrichRef = %q, want re-enriched URI", again.EnrichRef)
	}
}

// TestEnrichRollupNotFound proves EnrichRollup errors when there is no rollup
// to enrich.
func TestEnrichRollupNotFound(t *testing.T) {
	c, _ := openRollupCortex(t)
	empty := cortex.HourWindow(baseHour.UnixNano())
	enr := templatingEnricher{model: "test-templater@v1"}
	if _, err := c.EnrichRollup(cortex.TierHour, empty.Start, enr); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("EnrichRollup(no rollup) err = %v, want memory.ErrNotFound", err)
	}
}

// TestEnrichRollupDerivedLaneSafety proves the derived-lane posture: EnrichRollup
// causes NO anchored-namespace SMT drift (req.4.3), and the FULL OverallRoot
// (incl the KindRollupEnrich journal-MMR leaves) rebuilds byte-identically
// after a replay drop+rebuild.
func TestEnrichRollupDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := buildEnrichableHourRollup(t, c, clk)
	enr := templatingEnricher{model: "test-templater@v1"}

	clk.t = baseHour.Add(90 * time.Minute)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, e := c.EnrichRollup(cortex.TierHour, w.Start, enr)
		return e
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across EnrichRollup: %v", err)
	}

	res, err := cmharness.ReplayPreservesRoot(c, nil)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with enrich entries present: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}
