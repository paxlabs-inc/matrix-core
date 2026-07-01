// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	cmem "matrix/cortex/memory"
	"matrix/cortex/store"

	"matrix/neo/internal/config"
)

// Continuous-memory task 6.3: the memory_recall tool (pager.Recall) is pointed
// at the cortex RECURSIVE-RECALL surface (cortex.RecallDescend, req.8) when the
// continuous-memory collapse is active, while preserving the as_of parameter
// and never regressing (a flat fallback when the timeline reaches nothing).
//
// Both tests run against a REAL cortex (no stub/mock/fake — req.12.7): one
// exercises the flat fallback through Open()'s real pager, the other builds a
// REAL cascaded ladder and proves the descent renders through the tool.

// TestRecall_ContinuousMemory_FallsBackToFlatWhenNoLadder proves the collapse
// path is never worse than the flat lookup: with the flag on but NO temporal
// ladder built yet (the realistic pre-cascade state), a remembered fact is
// still recalled — recallRecursive descends an empty timeline and falls back to
// recallFlat, which finds the fact via the deterministic type-filtered scan.
func TestRecall_ContinuousMemory_FallsBackToFlatWhenNoLadder(t *testing.T) {
	cfg := testCfg(t)
	cfg.ContinuousMemory = true
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const fact = "the gateway credit ledger lives in postgres"
	if _, err := p.RememberFact(ctx, fact); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	drain(t, p)

	out, err := p.Recall(ctx, "", nil, 0, nil)
	if err != nil {
		t.Fatalf("Recall (continuous, no ladder): %v", err)
	}
	if !strings.Contains(out, "postgres") {
		t.Fatalf("continuous-memory recall must fall back to the flat lookup and surface the fact; got:\n%s", out)
	}
}

// TestRecall_ContinuousMemory_DescendsTimeline proves the tool descends the
// temporal ladder to page in an exact specific: a Preference written in the
// past and cascaded into a closed hour→day→epoch ladder is reached by the
// recursive descent behind pager.Recall, and rendered under the timeline
// header (not the flat "Relevant memories:" header).
func TestRecall_ContinuousMemory_DescendsTimeline(t *testing.T) {
	// A deterministic clock so we can seed the ladder in the past and read it
	// from a "now" outside the recent horizon — the same discipline the
	// cortex-level descent property test uses.
	base := time.Unix(1_700_000_000, 0).UTC()
	var clk time.Time
	clk = base
	nowFn := func() time.Time { return clk }

	dir := t.TempDir()
	s, err := store.Open(dir, "neo-recall-recursive", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	c := cortex.New(s, cortex.WithClock(nowFn))

	cfg := config.Default()
	cfg.ContinuousMemory = true
	// The pager is the thin agent-side shim over this cortex brain; no
	// embedder is started, so recall runs the deterministic path.
	p := &Pager{cfg: cfg, cortex: c, store: s}

	// Seed a real Preference at the base hour.
	const topic = "quantization"
	uri, err := c.Write(
		cmem.Head{ActorScope: "andrew"},
		cmem.PreferenceData{
			SchemaVersion: 1,
			Topic:         topic,
			Polarity:      cmem.PolarityPrefer,
			StrengthVal:   0.9,
			Rationale:     "near-lossless on CPU",
		},
		cortex.WriteMeta{
			CreatedBy:  "andrew",
			Forms:      cmem.Forms{Short: "prefers " + topic, Medium: "prefers " + topic + " (near-lossless on CPU)"},
			Provenance: cmem.Provenance{Source: cmem.SourceUserInput},
		},
	)
	if err != nil {
		t.Fatalf("Write preference: %v", err)
	}

	// Cascade the full ladder as of 10 days later, so every window (hour, day,
	// epoch) is closed. Read from base+10d+1h — well outside the recent tier.
	closeAt := base.Add(10 * 24 * time.Hour)
	clk = closeAt.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	out, err := p.Recall(context.Background(), "", nil, 0, nil)
	if err != nil {
		t.Fatalf("Recall (continuous, cascaded ladder): %v", err)
	}
	if !strings.Contains(out, "paged in from your timeline") {
		t.Fatalf("recall must render the recursive-descent (timeline) header, got:\n%s", out)
	}
	if !strings.Contains(out, topic) {
		t.Fatalf("recall must surface the descended preference %q, got:\n%s", topic, out)
	}
	if !strings.Contains(out, string(uri)) {
		t.Fatalf("recall must cite the memory URI %q for provenance, got:\n%s", uri, out)
	}
}

// TestRecall_ContinuousMemory_AsOfPreserved proves the as_of parameter is
// threaded through the recursive path: a preference closed in valid time is
// reachable at an as_of before the close and filtered at/after it (bi-temporal
// time-travel preserved at the descent leaf — req.8.3).
func TestRecall_ContinuousMemory_AsOfPreserved(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	var clk time.Time
	clk = base
	nowFn := func() time.Time { return clk }

	s, err := store.Open(t.TempDir(), "neo-recall-asof", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	c := cortex.New(s, cortex.WithClock(nowFn))

	cfg := config.Default()
	cfg.ContinuousMemory = true
	p := &Pager{cfg: cfg, cortex: c, store: s}

	const topic = "quantization"
	uri, err := c.Write(
		cmem.Head{ActorScope: "andrew"},
		cmem.PreferenceData{SchemaVersion: 1, Topic: topic, Polarity: cmem.PolarityPrefer, StrengthVal: 0.9, Rationale: "cpu"},
		cortex.WriteMeta{
			CreatedBy:  "andrew",
			Forms:      cmem.Forms{Short: "prefers " + topic, Medium: "prefers " + topic},
			Provenance: cmem.Provenance{Source: cmem.SourceUserInput},
		},
	)
	if err != nil {
		t.Fatalf("Write preference: %v", err)
	}

	// Close the preference's valid time 2 days after it was written.
	closeAt := base.Add(2 * 24 * time.Hour)
	clk = closeAt
	if _, err := c.CloseValidity(uri, time.Time{}, "andrew"); err != nil {
		t.Fatalf("CloseValidity: %v", err)
	}

	// Cascade well after the close so the whole ladder is closed.
	cascadeNow := base.Add(10 * 24 * time.Hour)
	clk = cascadeNow.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, cascadeNow.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	// as_of BEFORE the close → reachable.
	before := base.Add(1 * time.Hour)
	outBefore, err := p.Recall(context.Background(), "", nil, 0, &before)
	if err != nil {
		t.Fatalf("Recall(before close): %v", err)
	}
	if !strings.Contains(outBefore, string(uri)) {
		t.Fatalf("as_of before the close must surface %q, got:\n%s", uri, outBefore)
	}

	// as_of AT/AFTER the close → filtered (the descent reaches no live leaf and
	// falls back to the flat lookup, which also filters the superseded truth).
	after := closeAt.Add(1 * time.Hour)
	outAfter, err := p.Recall(context.Background(), "", nil, 0, &after)
	if err != nil {
		t.Fatalf("Recall(after close): %v", err)
	}
	if strings.Contains(outAfter, string(uri)) {
		t.Fatalf("as_of at/after the close must filter %q (half-open valid interval), got:\n%s", uri, outAfter)
	}
}
