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
	"matrix/cortex/salience"
)

// drain flushes the async embedding worker so HNSW reflects prior writes,
// making semantic-search assertions deterministic.
func drain(t *testing.T, p *Pager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.cortex.DrainEmbedder(ctx)
}

// A learned correction (Constraint, source=learned) and a strong working
// Preference must be PINNED every turn under the learned-guidance block — the
// structural fix for Neo forgetting a behavior it was corrected on.
func TestLearnedGuidancePinned(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const corr = "Always render a Construct surface when performing a task"
	if _, err := p.RememberConstraint(ctx, corr, "do", "firm"); err != nil {
		t.Fatalf("RememberConstraint: %v", err)
	}
	if _, err := p.RememberPreference(ctx, "terse, information-dense replies", "prefer", 0.9, "user dislikes filler"); err != nil {
		t.Fatalf("RememberPreference: %v", err)
	}

	guide := p.LearnedGuidance(ctx)
	if !contains(guide, corr) {
		t.Errorf("learned correction missing from guidance: %v", guide)
	}

	pinned := p.Pinned(ctx, "")
	if !strings.Contains(pinned, "Working guidance you've learned") {
		t.Errorf("pinned block missing learned-guidance header:\n%s", pinned)
	}
	if !strings.Contains(pinned, corr) {
		t.Errorf("pinned block missing the learned correction:\n%s", pinned)
	}
	if !strings.Contains(pinned, "terse, information-dense replies") {
		t.Errorf("pinned block missing the strong learned preference:\n%s", pinned)
	}
}

// A weak preference (below the pin floor) stays in retrieval, not the scarce
// pinned block.
func TestWeakPreferenceNotPinned(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.RememberPreference(ctx, "maybe use emoji sometimes", "prefer", 0.2, ""); err != nil {
		t.Fatalf("RememberPreference: %v", err)
	}
	if g := p.LearnedGuidance(ctx); len(g) != 0 {
		t.Errorf("weak preference should not be pinned guidance, got %v", g)
	}
}

// RememberConstraint must normalize a free-form polarity/strength and stamp
// Source=learned so corrections are auditable as machine-learned, not declared.
func TestRememberConstraintNormalizesAndStampsLearned(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	uri, err := p.RememberConstraint(ctx, "Prefer fenced code blocks", "  DO  ", "garbage")
	if err != nil {
		t.Fatalf("RememberConstraint: %v", err)
	}
	if uri == "" {
		t.Fatal("expected a URI for a fresh constraint")
	}

	ids, err := p.cortex.ListByType(cmem.TypeConstraint, 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ListByType constraints = %d ids (err %v), want 1", len(ids), err)
	}
	m, err := p.cortex.ResolveLatest(ids[0])
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	data, err := cmem.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	cd, ok := data.(cmem.ConstraintData)
	if !ok {
		t.Fatalf("decoded type = %T, want ConstraintData", data)
	}
	if cd.Source != cmem.ConstraintSourceLearned {
		t.Errorf("Source = %q, want learned", cd.Source)
	}
	if cd.Polarity != cmem.PolarityDo {
		t.Errorf("Polarity = %q, want do", cd.Polarity)
	}
	if cd.StrengthVal != cmem.StrengthFirm {
		t.Errorf("Strength = %q, want firm (default for unrecognized)", cd.StrengthVal)
	}
}

// Semantic dedup: re-remembering an identical fact must NOT write a duplicate,
// so a 5-week-old store doesn't bloat with restated truths.
func TestSemanticDedupSkipsIdenticalFact(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if !p.hasEmbedder {
		t.Skip("semantic dedup requires an embedder")
	}
	ctx := context.Background()

	const fact = "the dev box repo is at /root/matrix"
	uri1, err := p.RememberFact(ctx, fact)
	if err != nil || uri1 == "" {
		t.Fatalf("first RememberFact: uri=%q err=%v", uri1, err)
	}
	drain(t, p) // make the first fact visible to HNSW before the dup check

	uri2, err := p.RememberFact(ctx, fact)
	if err != nil {
		t.Fatalf("second RememberFact: %v", err)
	}
	if uri2 != "" {
		t.Errorf("identical fact should have been deduped (empty uri), got %q", uri2)
	}

	ids, err := p.cortex.ListByType(cmem.TypeFact, 10)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("fact count = %d, want 1 (dedup)", len(ids))
	}
}

// AttestUsed is best-effort and must never panic or write on degenerate input
// (empty intent, no citations) so it can sit on the hot completion path.
func TestAttestUsedGuards(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// All of these are no-ops that must return cleanly.
	p.AttestUsed(ctx, "", []string{"matrix://x/Fact/1"}, true)
	p.AttestUsed(ctx, "neo-turn:test:1", nil, true)
	p.AttestUsed(ctx, "neo-turn:test:1", []string{"", "  "}, true)

	// A real surfaced fact attested as used must not error the turn even
	// though the citation bump happens inside cortex.
	uri, err := p.RememberFact(ctx, "neo ships its memory loop")
	if err != nil || uri == "" {
		t.Fatalf("RememberFact: uri=%q err=%v", uri, err)
	}
	drain(t, p)
	p.AttestUsed(ctx, "neo-turn:test:2", []string{uri, uri}, true) // dup URI collapses
}

// RejectionCandidates enforces the negative-attest guardrails: only off-topic,
// rejectable (non-pinned) types are selected; an on-topic memory and every
// pinned/protected type (Identity / Constraint / Preference / Goal / Pattern)
// are never penalized.
func TestRejectionCandidatesGuardrails(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if !p.hasEmbedder {
		t.Skip("rejection cosine gate requires an embedder")
	}

	const turn = "deploy an erc20 token to base mainnet"
	surfaced := []Snippet{
		{URI: "matrix://x/Fact/1", Type: "Fact", Text: "the user's cat is named mittens"}, // off-topic + rejectable → SELECTED
		{URI: "matrix://x/Fact/2", Type: "Fact", Text: turn},                              // on-topic (identical) → not selected
		{URI: "matrix://x/Constraint/3", Type: "Constraint", Text: "never wipe prod state"}, // pinned type → never
		{URI: "matrix://x/Preference/4", Type: "Preference", Text: "use tabs not spaces"},    // pinned type → never
		{URI: "matrix://x/Goal/5", Type: "Goal", Text: "learn to play the cello"},            // pinned type → never
		{URI: "matrix://x/Pattern/6", Type: "Pattern", Text: "some unrelated recipe"},        // not rejectable → never
	}

	got := p.RejectionCandidates(turn, surfaced)
	if len(got) != 1 || got[0] != "matrix://x/Fact/1" {
		t.Fatalf("rejection candidates = %v, want exactly [matrix://x/Fact/1]", got)
	}

	// A concrete-output guardrail: an empty turn produces no rejections.
	if r := p.RejectionCandidates("", surfaced); r != nil {
		t.Errorf("empty turn must yield no rejections, got %v", r)
	}
}

// The per-turn cap bounds the negative signal even when many surfaced memories
// are off-topic.
func TestRejectionCandidatesCapped(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	if !p.hasEmbedder {
		t.Skip("rejection cosine gate requires an embedder")
	}

	const turn = "summarize the credit ledger migration"
	surfaced := []Snippet{
		{URI: "matrix://x/Fact/a", Type: "Fact", Text: "alpha unrelated trivia one"},
		{URI: "matrix://x/Fact/b", Type: "Fact", Text: "beta unrelated trivia two"},
		{URI: "matrix://x/Fact/c", Type: "Fact", Text: "gamma unrelated trivia three"},
		{URI: "matrix://x/Event/d", Type: "Event", Text: "delta unrelated trivia four"},
		{URI: "matrix://x/Belief/e", Type: "Belief", Text: "epsilon unrelated trivia five"},
		{URI: "matrix://x/Fact/f", Type: "Fact", Text: "zeta unrelated trivia six"},
	}
	got := p.RejectionCandidates(turn, surfaced)
	if len(got) != maxRejectionsPerTurn {
		t.Fatalf("rejection count = %d, want cap %d", len(got), maxRejectionsPerTurn)
	}
}

// AttestRejected is best-effort on degenerate input and sends the §8.3
// decrement signal on a real memory: a fact bumped to one citation by a USED
// attest drops back to zero after a rejection attest.
func TestAttestRejectedDecrements(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// Degenerate input is a clean no-op.
	p.AttestRejected(ctx, "", []string{"matrix://x/Fact/1"})
	p.AttestRejected(ctx, "neo-turn:test:1", nil)
	p.AttestRejected(ctx, "neo-turn:test:1", []string{"", "  "})

	uri, err := p.RememberFact(ctx, "the gateway credit ledger lives in postgres")
	if err != nil || uri == "" {
		t.Fatalf("RememberFact: uri=%q err=%v", uri, err)
	}
	drain(t, p)

	_, id, _, err := cortex.ParseURI(cmem.URI(uri))
	if err != nil {
		t.Fatalf("ParseURI(%q): %v", uri, err)
	}

	// USED bumps citations to 1.
	p.AttestUsed(ctx, "neo-turn:test:used", []string{uri}, true)
	sc, ok, err := salience.Read(p.store, id)
	if err != nil || !ok {
		t.Fatalf("salience.Read after use: ok=%v err=%v", ok, err)
	}
	if sc.Citations != 1 {
		t.Fatalf("citations after USED attest = %d, want 1", sc.Citations)
	}

	// REJECTED decrements citations back to 0 (the negative half of the loop).
	p.AttestRejected(ctx, "neo-turn:test:reject", []string{uri})
	sc, ok, err = salience.Read(p.store, id)
	if err != nil || !ok {
		t.Fatalf("salience.Read after reject: ok=%v err=%v", ok, err)
	}
	if sc.Citations != 0 {
		t.Errorf("citations after REJECTED attest = %d, want 0 (decremented)", sc.Citations)
	}
}
