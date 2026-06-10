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

// RememberFact stores a durable objective fact (semantic memory).
func (p *Pager) RememberFact(ctx context.Context, statement string) (string, error) {
	uri, err := p.cortex.Write(
		p.head(5),
		memory.FactData{
			SchemaVersion: 1,
			Statement:     statement,
			Subject:       factSubject,
			Predicate:     factPredicate,
		},
		p.writeMeta(),
	)
	return string(uri), err
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

// Outcome re-exports the cortex outcome enum so callers in the loop /
// write-back packages don't import matrix/cortex/memory directly.
type Outcome = memory.Outcome

const (
	OutcomeSuccess = memory.OutcomeSuccess
	OutcomeFailure = memory.OutcomeFailure
	OutcomePartial = memory.OutcomePartial
)
