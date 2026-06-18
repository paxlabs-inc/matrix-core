// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// writeMeta is the standard provenance for Neo's auto-consolidated writes.
func (p *Pager) writeMeta() cortex.WriteMeta {
	return cortex.WriteMeta{
		CreatedBy:  p.cfg.CortexActor,
		Provenance: memory.Provenance{Source: memory.SourceObserved},
	}
}

func (p *Pager) head(importance uint8) memory.Head {
	return memory.Head{ActorScope: p.cfg.CortexActor, DeclaredImportance: importance}
}

// factSubject / factPredicate are the default provenance for Neo's
// auto-consolidated facts. cortex requires both a subject and a predicate on a
// Fact (memory.ValidateMemory), so a fact written without them is rejected —
// they are NOT optional. Mirrors the cortex-mem.sh convention.
const (
	factSubject   = "matrix://knowledge/neo"
	factPredicate = "note"

	// userFactSubject scopes facts that describe THE USER (name, role,
	// stable preferences). They get their own subject so the pager can pin
	// them every turn — identity questions must never depend on retrieval
	// luck.
	userFactSubject   = "matrix://knowledge/user"
	userFactPredicate = "profile"
)

// RememberFact stores a durable objective fact (semantic memory). Skips a
// write whose statement is near-identical to an existing fact (semantic
// dedup) so a repeatedly-relevant truth doesn't accumulate duplicates that
// dilute retrieval — its recency/salience is refreshed instead via the
// Attest usage loop when it is surfaced and cited.
func (p *Pager) RememberFact(ctx context.Context, statement string) (string, error) {
	return p.RememberFactRelated(ctx, statement, nil)
}

// RememberFactRelated is RememberFact with conflict-aware linking: when a
// topically-similar (but not duplicate) fact already exists, classify decides
// whether the new fact supersedes / contradicts / merely relates to it, and
// the appropriate cortex edge is linked after the write. A nil classifier is
// exactly RememberFact (dedup-or-write, no edges).
func (p *Pager) RememberFactRelated(ctx context.Context, statement string, classify RelationClassifier) (string, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", nil
	}
	data := memory.FactData{
		SchemaVersion: 1,
		Statement:     statement,
		Subject:       factSubject,
		Predicate:     factPredicate,
	}
	return p.writeWithRelations(ctx, memory.TypeFact, data, 5, classify)
}

// RememberUserFact stores a durable fact about the user themselves (their
// name, role, stable preferences). Pinned every turn via UserProfile, and
// deduped on the normalized statement so repeats don't bloat the profile.
func (p *Pager) RememberUserFact(ctx context.Context, statement string) (string, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", nil
	}
	norm := normalizeStatement(statement)
	for _, existing := range p.UserProfile(ctx) {
		if normalizeStatement(existing) == norm {
			return "", nil
		}
	}
	uri, err := p.cortex.Write(
		p.head(7),
		memory.FactData{
			SchemaVersion: 1,
			Statement:     statement,
			Subject:       userFactSubject,
			Predicate:     userFactPredicate,
		},
		p.writeMeta(),
	)
	return string(uri), err
}

// RememberPreference stores a durable WORKING preference Neo learned about
// how the user wants it to behave (tone, format, which surfaces/tools to use,
// etc.). Preferences are surfaced every turn via the pager's pinned
// learned-guidance block, so a learned working style actually changes
// behavior instead of being lost in salience-ranked retrieval. Deduped
// semantically so repeats don't bloat the pinned block.
func (p *Pager) RememberPreference(ctx context.Context, topic, polarity string, strength float32, rationale string) (string, error) {
	return p.RememberPreferenceRelated(ctx, topic, polarity, strength, rationale, nil)
}

// RememberPreferenceRelated is RememberPreference with conflict-aware linking:
// an updated preference about the same topic supersedes the stale one, an
// opposite-polarity preference contradicts it. A nil classifier is exactly
// RememberPreference (dedup-or-write, no edges).
func (p *Pager) RememberPreferenceRelated(ctx context.Context, topic, polarity string, strength float32, rationale string, classify RelationClassifier) (string, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return "", nil
	}
	data := memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      normalizePolarity(polarity, memory.PolarityPrefer),
		StrengthVal:   clampUnit(strength),
		Rationale:     strings.TrimSpace(rationale),
	}
	return p.writeWithRelations(ctx, memory.TypePreference, data, 7, classify)
}

// RememberConstraint stores a durable behavioral rule Neo learned — typically
// from a USER CORRECTION ("you did X, do Y instead"). Source=learned. It is
// pinned every turn (hard-strength rules verbatim via hardConstraints, softer
// ones via the learned-guidance block) so the correction sticks structurally
// rather than depending on retrieval luck. Deduped semantically.
func (p *Pager) RememberConstraint(ctx context.Context, statement, polarity, strength string) (string, error) {
	return p.RememberConstraintRelated(ctx, statement, polarity, strength, nil)
}

// RememberConstraintRelated is RememberConstraint with conflict-aware linking:
// a refined behavioral rule supersedes the stale one it corrects; an opposite
// rule contradicts it. A nil classifier is exactly RememberConstraint
// (dedup-or-write, no edges).
func (p *Pager) RememberConstraintRelated(ctx context.Context, statement, polarity, strength string, classify RelationClassifier) (string, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", nil
	}
	data := memory.ConstraintData{
		SchemaVersion: 1,
		Statement:     statement,
		Polarity:      normalizePolarity(polarity, memory.PolarityDo),
		StrengthVal:   normalizeStrength(strength),
		Source:        memory.ConstraintSourceLearned,
	}
	return p.writeWithRelations(ctx, memory.TypeConstraint, data, 8, classify)
}

// RecordOutcome stores an episodic outcome (success/failure/partial). The
// background write-back pass and the loop's termination both call this.
func (p *Pager) RecordOutcome(ctx context.Context, summary string, outcome memory.Outcome, intentRef string) (string, error) {
	uri, err := p.cortex.Write(
		p.head(4),
		memory.EventData{
			SchemaVersion: 1,
			Kind:          memory.EventObservation,
			OutcomeVal:    outcome,
			Summary:       summary,
			IntentRef:     intentRef,
		},
		p.writeMeta(),
	)
	return string(uri), err
}

// WritePattern stores a candidate procedural pattern (the nursery for MCL
// skills). The structured spec is encoded onto cortex's flat Statement field.
// Coverage starts low and is reinforced on each repeat success; retrieval gates
// injection on cfg.MinPatternSuccesses (anti-overfit).
func (p *Pager) WritePattern(ctx context.Context, spec PatternSpec, strength float32, coverage int, derivedFrom []string) (string, error) {
	uri, err := p.cortex.Write(
		p.head(6),
		memory.PatternData{
			SchemaVersion: 1,
			Statement:     spec.Encode(),
			Strength:      strength,
			Coverage:      coverage,
			DerivedFrom:   derivedFrom,
		},
		p.writeMeta(),
	)
	return string(uri), err
}

// ReinforcePattern implements the procedural lifecycle's distill+reinforce
// stages: if a pattern with the same dedup identity (name → trigger → steps)
// already exists it is reinforced (coverage++ and strength nudged up) so it can
// graduate past the anti-overfit gate; otherwise a fresh low-confidence
// candidate is written. Dedup is deliberately simple for v1 (semantic dedup is
// a follow-up).
func (p *Pager) ReinforcePattern(ctx context.Context, spec PatternSpec, derivedFrom []string) (string, error) {
	key := spec.dedupKey()
	if key == "" {
		return "", nil
	}
	res, err := p.cortex.Find(query.Query{Type: []memory.Type{memory.TypePattern}, Limit: 100})
	if err == nil && res != nil {
		for _, m := range res.Memories {
			data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
			if derr != nil {
				continue
			}
			var pd memory.PatternData
			switch x := data.(type) {
			case memory.PatternData:
				pd = x
			case *memory.PatternData:
				pd = *x
			default:
				continue
			}
			if DecodePatternSpec(pd.Statement).dedupKey() != key {
				continue
			}
			pd.Coverage++
			pd.Strength = clampUnit(pd.Strength + 0.1)
			pd.DerivedFrom = mergeUnique(pd.DerivedFrom, derivedFrom)
			uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
			u, uerr := p.cortex.Update(uri, pd, p.writeMeta())
			return string(u), uerr
		}
	}
	return p.WritePattern(ctx, spec, 0.5, 1, derivedFrom)
}

func normalizeStatement(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func clampUnit(f float32) float32 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// dupDistanceThreshold is the HNSW distance below which a candidate write is
// treated as a semantic duplicate of an existing same-type memory. Distance
// is 1-cosine under unit norm (0=identical, 2=opposite); 0.08 ≈ cosine ~0.92,
// deliberately tight so genuinely distinct statements are never merged. The
// conflict-aware write core (writeWithRelations, relation.go) reads it to
// short-circuit a duplicate before spending a classifier call.
const dupDistanceThreshold float32 = 0.08

// AttestUsed records that the given cortex memories were surfaced into a
// completed turn, feeding the usage-based salience signals (access +
// citation) and the per-actor EMA weight learner via cortex.Attest. This is
// what turns the cortex LEARNING LOOP ON for Neo: Neo's page-fault Find calls
// are compile-time (no journal, no access bump), so without this the actor's
// salience would stay frozen at the cold recency×importance formula and the
// store could never learn what is actually useful. Best-effort and never
// blocks the turn — a failed attest just skips the signal for that turn.
//
// success=false attests a failed turn for audit only; per cortex §8.3 a
// generic failure (no factual-error/wrong-assumption reason) leaves citations
// untouched, so we do not punish surfaced memories for an unrelated stall.
func (p *Pager) AttestUsed(ctx context.Context, intentID string, citedURIs []string, success bool) {
	_ = ctx
	if p == nil || p.cortex == nil {
		return
	}
	intentID = strings.TrimSpace(intentID)
	if intentID == "" || len(citedURIs) == 0 {
		return
	}
	seen := map[string]bool{}
	cited := make([]memory.URI, 0, len(citedURIs))
	for _, u := range citedURIs {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		cited = append(cited, memory.URI(u))
		if len(cited) >= cortex.MaxCitedURIsPerAttest {
			break
		}
	}
	if len(cited) == 0 {
		return
	}
	outcome := cortex.AttestOutcomeSuccess
	if !success {
		outcome = cortex.AttestOutcomeFailure
	}
	_, _ = p.cortex.Attest(cortex.AttestOpts{
		IntentID:  intentID,
		Outcome:   outcome,
		Cited:     cited,
		CreatedBy: p.cfg.CortexActor,
	})
}

// normalizePolarity coerces a free-form polarity string to the closed cortex
// enum, falling back to def when unrecognized.
func normalizePolarity(s string, def memory.Polarity) memory.Polarity {
	switch memory.Polarity(normalizeStatement(s)) {
	case memory.PolarityPrefer:
		return memory.PolarityPrefer
	case memory.PolarityAvoid:
		return memory.PolarityAvoid
	case memory.PolarityDo:
		return memory.PolarityDo
	case memory.PolarityDont:
		return memory.PolarityDont
	case memory.PolarityNeutral:
		return memory.PolarityNeutral
	default:
		return def
	}
}

// normalizeStrength coerces a free-form strength string to the closed cortex
// enum, defaulting to firm (learned corrections are strong defaults, not
// inviolable hard rules).
func normalizeStrength(s string) memory.Strength {
	switch memory.Strength(normalizeStatement(s)) {
	case memory.StrengthHard:
		return memory.StrengthHard
	case memory.StrengthSoft:
		return memory.StrengthSoft
	default:
		return memory.StrengthFirm
	}
}

// Outcome re-exports the cortex outcome enum so callers in the loop /
// write-back packages don't import matrix/cortex/memory directly.
type Outcome = memory.Outcome

const (
	OutcomeSuccess = memory.OutcomeSuccess
	OutcomeFailure = memory.OutcomeFailure
	OutcomePartial = memory.OutcomePartial
)
