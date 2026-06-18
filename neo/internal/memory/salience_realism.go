// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Salience realism (memory capabilities v2, Phase 2). Two Neo-side additions
// to the usage-learning loop, both of which leave cortex's replay-critical
// bytes untouched:
//
//   - Per-type half-life RE-RANK: cortex stores one global 90-day recency
//     half-life (a journaled, replay-invariant constant). Different memory
//     types should not live on one timescale, so AFTER cortex returns its
//     ranked candidates the pager scales each candidate's score by a recency
//     multiplier keyed by the memory's type. This only re-orders Neo's working
//     set; cortex's stored Score, sc.Cached, and the 90d formula never change.
//
//   - Rejection (negative) ATTESTATION: v1 only reinforced memories it USED.
//     This adds the negative half — a surfaced-but-ignored memory gets a
//     decrement signal through the existing cortex.Attest decrement path
//     (DecrementCitation + EMA-away), so noise that crowds the budget without
//     ever being used decays out over time. Conservative by construction:
//     only off-topic, non-pinned, rejectable types are penalized, capped per
//     turn (risk R2).
package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
)

// typeHalfLife returns the Neo-side recency half-life for a memory type (the
// v2 "salience realism" table). A zero duration means "no decay": Identity is
// permanent, and any type the table does not cover (Event, Pattern,
// Capability, Unknown) is deliberately left undecayed so the re-rank never
// penalizes a type the plan did not account for.
func typeHalfLife(typ string) time.Duration {
	switch typ {
	case memory.TypeIdentity.String():
		return 0 // ∞ — identity never decays
	case memory.TypeConstraint.String():
		return 365 * 24 * time.Hour // includes user corrections
	case memory.TypePreference.String():
		return 90 * 24 * time.Hour
	case memory.TypeFact.String(), memory.TypeGoal.String():
		return 30 * 24 * time.Hour
	case memory.TypeBelief.String():
		return 7 * 24 * time.Hour // inferred / least durable
	default:
		return 0 // Event, Pattern, Capability, Unknown: no Neo-side decay
	}
}

// recencyMultiplier returns the per-type half-life decay multiplier in (0,1]
// for a memory of type typ last touched at lastUsedNano (unix nano), evaluated
// at now. It is 1.0 (no decay) when the type has no half-life, when the
// timestamp is unknown (<= 0), or when the timestamp is in the future (clock
// skew) — never a penalty for missing data. Exact half-life decay:
// multiplier = 2^(-Δt/H) = exp(-Δt·ln2/H). Pure read-time math; nothing
// is persisted.
func recencyMultiplier(typ string, lastUsedNano int64, now time.Time) float32 {
	h := typeHalfLife(typ)
	if h <= 0 || lastUsedNano <= 0 {
		return 1.0
	}
	dt := now.UnixNano() - lastUsedNano
	if dt <= 0 {
		return 1.0
	}
	exponent := -float64(dt) * math.Ln2 / float64(h.Nanoseconds())
	return float32(math.Exp(exponent))
}

// lastUsedNano returns the salience LastUsed timestamp (unix nano) for id, or
// 0 when no salience record exists yet — so the per-type recency re-rank
// treats an un-scored memory as fresh (multiplier 1.0) rather than maximally
// stale. Best-effort: a read error reads as 0 too.
func (p *Pager) lastUsedNano(id memory.ID) int64 {
	if id.IsZero() {
		return 0
	}
	sc, ok, err := salience.Read(p.store, id)
	if err != nil || !ok || sc == nil {
		return 0
	}
	return sc.LastUsed
}

// rejectableType reports whether a surfaced memory of this type may be
// penalized by the rejection (negative-attest) path. Only ephemeral knowledge
// memories — Fact, Event, Belief — are eligible. Identity, Constraint,
// Preference, Goal, Pattern and Capability are NEVER penalized: they are
// pinned every turn or structurally protected, so demoting their salience
// would fight the pinned floor (guardrail c — never demote Identity / Hard
// Constraint / Active Goal).
func rejectableType(typ string) bool {
	switch typ {
	case memory.TypeFact.String(), memory.TypeEvent.String(), memory.TypeBelief.String():
		return true
	default:
		return false
	}
}

// offTopicCosine is the cosine ceiling below which a surfaced memory is treated
// as demonstrably off-topic to the produced turn (guardrail b). 0.30 mirrors
// the topic-pivot threshold; deliberately low so only memories with little
// bearing on the turn are penalized, keeping false negatives rare (risk R2).
const offTopicCosine float32 = 0.30

// maxRejectionsPerTurn caps how many surfaced-but-ignored memories a single
// turn may penalize, bounding journal churn from the negative-attest path
// (the per-turn cap guardrail, risk R2).
const maxRejectionsPerTurn = 4

// RejectionCandidates returns the URIs of memories that were SURFACED into a
// turn but were demonstrably ignored. A surfaced memory is a candidate when
// ALL guardrails hold:
//
//	(a) the turn produced concrete output  — turnText is non-empty;
//	(b) the memory is clearly off-topic    — cosine(turn, memory) < offTopicCosine;
//	(c) the memory is not pinned/protected — rejectableType(type) is true.
//
// The result is ordered farthest-first (lowest cosine) and capped at
// maxRejectionsPerTurn. It requires an embedder for the cosine gate; without
// one it returns nil so the rejection path stays conservative.
func (p *Pager) RejectionCandidates(turnText string, surfaced []Snippet) []string {
	if p == nil || !p.hasEmbedder || p.embedder == nil {
		return nil
	}
	turnText = strings.TrimSpace(turnText)
	if turnText == "" || len(surfaced) == 0 {
		return nil
	}
	qv, err := p.embedder.Embed(turnText)
	if err != nil {
		return nil
	}
	type scored struct {
		uri    string
		cosine float32
	}
	var off []scored
	seen := map[string]bool{}
	for _, s := range surfaced {
		if s.URI == "" || seen[s.URI] || !rejectableType(s.Type) {
			continue
		}
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		mv, eerr := p.embedder.Embed(text)
		if eerr != nil {
			continue
		}
		if cos := embed.Cosine(qv, mv); cos < offTopicCosine {
			seen[s.URI] = true
			off = append(off, scored{uri: s.URI, cosine: cos})
		}
	}
	sort.SliceStable(off, func(i, j int) bool { return off[i].cosine < off[j].cosine })
	out := make([]string, 0, maxRejectionsPerTurn)
	for _, s := range off {
		out = append(out, s.uri)
		if len(out) >= maxRejectionsPerTurn {
			break
		}
	}
	return out
}

// AttestRejected records that the given memories were surfaced into a turn but
// demonstrably ignored, sending the negative salience signal (DecrementCitation
// + EMA-away via cortex's decrementOnFailure path) so memories that crowd the
// budget without being used decay out of the working set over time.
//
// The attest is Outcome=Failure, Reason=wrong_assumption: surfacing the memory
// assumed a relevance the produced turn did not bear out, and wrong_assumption
// is one of the two §8.3 reasons that trigger the citation decrement. The
// caller is responsible for passing only memories that cleared the
// RejectionCandidates guardrails (off-topic, non-pinned, capped). Best-effort
// and never blocks the turn — a failed attest just skips the signal.
func (p *Pager) AttestRejected(ctx context.Context, intentID string, ignoredURIs []string) {
	_ = ctx
	if p == nil || p.cortex == nil {
		return
	}
	intentID = strings.TrimSpace(intentID)
	if intentID == "" || len(ignoredURIs) == 0 {
		return
	}
	seen := map[string]bool{}
	cited := make([]memory.URI, 0, len(ignoredURIs))
	for _, u := range ignoredURIs {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		cited = append(cited, memory.URI(u))
		if len(cited) >= maxRejectionsPerTurn {
			break
		}
	}
	if len(cited) == 0 {
		return
	}
	_, _ = p.cortex.Attest(cortex.AttestOpts{
		IntentID:  intentID,
		Outcome:   cortex.AttestOutcomeFailure,
		Reason:    cortex.AttestReasonWrongAssumption,
		Cited:     cited,
		CreatedBy: p.cfg.CortexActor,
	})
}
