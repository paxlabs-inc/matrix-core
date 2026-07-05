// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package cassandra is Matrix's epistemic-completeness faculty: the shared
// cross-rail layer that refuses to read the ABSENCE of an error as the
// PRESENCE of success. It is the generalization of the MCL completeness critic
// (executor/cmd/mcl-execute/critique.go, Phase 11.5) into a first-class module
// consumed by both the MCL pipeline and Neo. Design authority is
// cassandra/cassandra.frozen.kvx — code conforms to the .kvx, never the reverse.
//
// The module is deliberately rail-agnostic and dependency-free: callers build
// the ground-truth evidence digest themselves (the MCL pipeline from its
// plan_tree, Neo from its working transcript) and inject an LLM Decoder, so the
// faculty never imports MCL/llm, the executor, or Neo. The single structured
// output is the Verdict ([verdict].schema).
package cassandra

import "strings"

// Coverage answers "was every explicitly requested deliverable produced?".
type Coverage string

const (
	// CoverageFull — every requested deliverable was produced by a real
	// executed step with a real result.
	CoverageFull Coverage = "full"
	// CoveragePartial — at least one requested deliverable is still
	// unsatisfied (the Missing list enumerates them).
	CoveragePartial Coverage = "partial"
)

// Verdict is the canonical structured judgement every Cassandra adjudication
// returns ([verdict].schema). It generalizes the legacy
// criticVerdict{Complete, Missing, Rationale}: Coverage+Missing carry the old
// deliverable-coverage decision (CoverageComplete), while Grounded /
// UnverifiedClaims / Assumptions / OpenUnknowns add the evidence-grounding
// dimension that Neo's gate consumes (Sound).
type Verdict struct {
	// Grounded is true only when every load-bearing claim in the answer is
	// backed by a real executed step + real result in the evidence digest
	// ([verdict.fields].grounded).
	Grounded bool `json:"grounded"`
	// Coverage is full only if every explicitly requested deliverable was
	// produced; else partial ([verdict.fields].coverage).
	Coverage Coverage `json:"coverage"`
	// Missing enumerates still-unsatisfied requested deliverables, phrased as
	// concrete actionable items a re-planner/continuation can turn into work.
	Missing []string `json:"missing"`
	// UnverifiedClaims are assertions the agent made that NO executed evidence
	// supports — the hallucination surface.
	UnverifiedClaims []string `json:"unverified_claims"`
	// Assumptions are defaults the agent silently chose that materially shape
	// the result.
	Assumptions []string `json:"assumptions"`
	// OpenUnknowns is the absence-as-object field: things NOT confirmed that
	// should have been. An empty list is an explicit, attributable claim of
	// "no unknowns", never silence ([principles].make_absence_present).
	OpenUnknowns []string `json:"open_unknowns"`
	// Certainty is Cassandra's confidence in her OWN verdict (0..1). Low
	// certainty on a reversible turn defers to the agent (fail-open).
	Certainty float64 `json:"certainty"`
	// Rationale is a one-line explanation (audit/transcript only).
	Rationale string `json:"rationale"`
}

// Normalize trims blank list entries and applies the coherence guards
// ([verdict.coherence_guards]) that bias every incoherent or under-signalled
// verdict toward MORE work, never toward a false success
// ([principles].coherence_toward_more_work, invariant i_cass_7):
//
//	g1 — Coverage=full with a non-empty Missing list -> force Coverage=partial.
//	g2 — Grounded=true with non-empty UnverifiedClaims -> force Grounded=false.
//	g4 — Unknown/blank Coverage -> Coverage=partial (a missing signal fails
//	     toward refusal; completeness is only ever asserted by an explicit
//	     coverage=full). The grounded counterpart (g5) is applied at parse
//	     (an omitted grounded field is read as false, never inferred true).
//
// There is no citation-matching guard: completion is judged by whether the
// OUTCOME satisfies the GOAL over the executed transcript (grounded +
// unverified_claims), never by substring-matching refs the agent typed.
func (v *Verdict) Normalize() {
	if v == nil {
		return
	}
	v.Missing = cleanList(v.Missing)
	v.UnverifiedClaims = cleanList(v.UnverifiedClaims)
	v.Assumptions = cleanList(v.Assumptions)
	v.OpenUnknowns = cleanList(v.OpenUnknowns)

	if v.Coverage != CoverageFull && v.Coverage != CoveragePartial {
		// Unknown/blank coverage carries NO affirmative completeness signal, so
		// it fails TOWARD REFUSAL: partial, never full (g4 / req 7.1). An empty
		// missing list is silence, not a claim of completion — completeness is
		// only ever asserted by an explicit coverage=full.
		v.Coverage = CoveragePartial
	}
	// g1: coverage cannot be "full" while items remain missing.
	if v.Coverage == CoverageFull && len(v.Missing) > 0 {
		v.Coverage = CoveragePartial
	}
	// g2: grounded cannot be true while unverified claims remain.
	if v.Grounded && len(v.UnverifiedClaims) > 0 {
		v.Grounded = false
	}
	if v.Certainty < 0 {
		v.Certainty = 0
	} else if v.Certainty > 1 {
		v.Certainty = 1
	}
}

// CoverageComplete reports the legacy completeness-critic decision: every
// requested deliverable was produced ([seams.mcl]). This is the gate the MCL
// re-home consults, so its behaviour is byte-identical to the old
// criticVerdict.Complete (Coverage==full AND no Missing items). Grounding does
// NOT enter this gate — that is reserved for Sound (Neo's gate, Phase 3).
func (v *Verdict) CoverageComplete() bool {
	return v != nil && v.Coverage == CoverageFull && len(v.Missing) == 0
}

// IsGrounded reports the evidence-grounding half of Sound: every load-bearing
// claim in the answer is backed by real executed evidence — grounded AND no
// unverified claims (the hallucination surface is empty). This is the single
// definition of "grounded" every consumer (Neo, Cody, Cassandra) shares; the
// honest-partial gate consults it directly, while Sound composes it with
// coverage for the full-completion decision.
func (v *Verdict) IsGrounded() bool {
	return v != nil && v.Grounded && len(v.UnverifiedClaims) == 0
}

// Sound reports the full grounded verdict Neo's completion gate consults
// (Phase 3): every deliverable produced AND every load-bearing claim grounded
// in real evidence ([coupling].high_stakes). Stricter than CoverageComplete.
// It is the single source of truth for verdict acceptance — Neo's
// verdictAccepts and Cody's gate.Adjudicate both decide through it (or through
// IsGrounded for the honest-partial path) rather than re-implementing the rule.
func (v *Verdict) Sound() bool {
	return v.CoverageComplete() && v.IsGrounded()
}

// cleanList trims whitespace and drops blank entries while preserving order.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:0]
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
