// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/cassandra"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

// These tests exercise the REAL verdict→action mapping against REAL
// cassandra.Verdict values and the REAL digest/contract builders — no fake
// decoders, no canned LLM output. A faked auditor would only prove the fake
// works; the behavior that matters is how a genuine verdict maps to finish vs.
// loop, which is pure and fully testable on real types.

func TestVerdictAcceptsGroundedFull(t *testing.T) {
	// A grounded, full-coverage verdict on a claimed-full turn → finish.
	v := &cassandra.Verdict{Grounded: true, Coverage: cassandra.CoverageFull}
	if !verdictAccepts("full", v, nil) {
		t.Error("grounded + full coverage must accept")
	}
}

func TestVerdictRejectsUngrounded(t *testing.T) {
	// The hallucination case: the model declares done but a load-bearing claim
	// has no supporting evidence → loop, never finish.
	v := &cassandra.Verdict{Grounded: false, Coverage: cassandra.CoverageFull}
	if verdictAccepts("full", v, nil) {
		t.Error("ungrounded verdict must reject (loop)")
	}
}

func TestVerdictRejectsUnverifiedClaims(t *testing.T) {
	// grounded=true is overridden by the presence of unverified claims — the
	// gate must not finish on assertions no executed result supports.
	v := &cassandra.Verdict{
		Grounded:         true,
		Coverage:         cassandra.CoverageFull,
		UnverifiedClaims: []string{"the deploy succeeded"},
	}
	if verdictAccepts("full", v, nil) {
		t.Error("unverified claims must reject even when grounded=true")
	}
}

func TestVerdictRejectsPhantomCitation(t *testing.T) {
	// A cited evidence ref absent from the transcript (g3) forces a reject.
	v := &cassandra.Verdict{Grounded: true, Coverage: cassandra.CoverageFull}
	if verdictAccepts("full", v, []string{"tx 0xdeadbeef confirmed"}) {
		t.Error("phantom citation must reject")
	}
}

func TestVerdictAcceptsHonestPartial(t *testing.T) {
	// An honest partial (the agent itself declares coverage=partial) finishes
	// AS a partial as long as its claims are grounded — Cassandra blocks false
	// SUCCESS, not honest incompleteness (i_cass_1).
	v := &cassandra.Verdict{
		Grounded: true,
		Coverage: cassandra.CoveragePartial,
		Missing:  []string{"the second contract is still unverified"},
	}
	if !verdictAccepts("partial", v, nil) {
		t.Error("honest, grounded partial must accept")
	}
}

func TestVerdictRejectsClaimedFullButIncomplete(t *testing.T) {
	// The agent claims full, but Cassandra finds a still-missing deliverable →
	// the false-success attempt loops with the missing item.
	v := &cassandra.Verdict{
		Grounded: true,
		Coverage: cassandra.CoveragePartial,
		Missing:  []string{"the refund was never sent"},
	}
	if verdictAccepts("full", v, nil) {
		t.Error("claimed-full but Cassandra-incomplete must reject")
	}
}

// cappedUnknowns is the current contract (F4): flagged unknowns ride the
// cassandra.* side-channel and are NEVER folded into the delivered answer.
func TestCappedUnknownsReturnsFlagged(t *testing.T) {
	v := &cassandra.Verdict{OpenUnknowns: []string{"whether the index finished rebuilding"}}
	got := cappedUnknowns(v)
	if len(got) != 1 || !strings.Contains(got[0], "index finished rebuilding") {
		t.Errorf("must surface the flagged unknown on the side-channel, got %v", got)
	}
}

func TestCappedUnknownsCapsAtTwo(t *testing.T) {
	v := &cassandra.Verdict{OpenUnknowns: []string{"a thing", "another thing", "a third thing"}}
	if got := cappedUnknowns(v); len(got) != 2 {
		t.Errorf("flagged unknowns must be capped at 2, got %d (%v)", len(got), got)
	}
}

func TestCappedUnknownsNilWhenClean(t *testing.T) {
	if got := cappedUnknowns(&cassandra.Verdict{}); got != nil {
		t.Errorf("no unknowns must return nil, got %v", got)
	}
}

func TestContinueFeedbackEnumeratesNegativeSpace(t *testing.T) {
	v := &cassandra.Verdict{
		Missing:          []string{"send the receipt"},
		UnverifiedClaims: []string{"balance is 5 PAX"},
		OpenUnknowns:     []string{"did the swap settle"},
	}
	fb := continueFeedback(v, []string{"tx 0xabc"})
	for _, want := range []string{"send the receipt", "balance is 5 PAX", "tx 0xabc", "did the swap settle", "partial"} {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback missing %q: %s", want, fb)
		}
	}
}

func TestBuildEvidenceDigestOnlyToolResults(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("what's the block height?"),
		llm.AssistantMessage("let me check"),
		llm.ToolResult("chain", "chain_info", `{"blockNumber":20531991}`),
	}
	d := a.buildEvidenceDigest()
	if !strings.Contains(d, "chain_info") || !strings.Contains(d, "20531991") {
		t.Errorf("digest must carry the real tool result, got %q", d)
	}
	if strings.Contains(d, "let me check") || strings.Contains(d, "what's the block height") {
		t.Errorf("digest is ground truth only — no user/assistant narration, got %q", d)
	}
}

func TestBuildAuditContractFoldsTheClaim(t *testing.T) {
	c := buildAuditContract(
		"send 1 PAX to alice",
		"Sent 1 PAX to alice.",
		"full",
		[]string{"tx 0xabc confirmed"},
		nil,
		nil,
	)
	for _, want := range []string{"send 1 PAX to alice", "Sent 1 PAX to alice.", "full", "tx 0xabc confirmed"} {
		if !strings.Contains(c, want) {
			t.Errorf("contract missing %q: %s", want, c)
		}
	}
}
