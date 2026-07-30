// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// LinkSessionProvenance adds a derived_from provenance edge from a
// consolidated memory (memURI) back to the session transcript slice it was
// extracted from — the (conv, seqLo, seqHi) span the consolidator knows at
// write time (DEJA-VU req 1.1). It is a thin delegate to
// cortex.AddSessionProvenance. Best-effort per the fail-open doctrine for the
// write path: an empty URI (a deduped write returns none) or a blank
// conversation is a no-op; a parse/edge error is returned for the caller to
// swallow so a failed edge never fails the memory write.
func (p *Pager) LinkSessionProvenance(memURI, conv string, seqLo, seqHi uint64) error {
	if p == nil || p.cortex == nil {
		return nil
	}
	memURI = strings.TrimSpace(memURI)
	if memURI == "" || strings.TrimSpace(conv) == "" {
		return nil
	}
	_, id, _, err := cortex.ParseURI(memory.URI(memURI))
	if err != nil {
		return err
	}
	return p.cortex.AddSessionProvenance(id, conv, seqLo, seqHi, p.cfg.CortexActor)
}

// ExpandToTranscript resolves a memory URI to the verbatim past exchange it was
// extracted from (DEJA-VU req 1.3), following derived_from provenance edges to
// a session slice (Exact) or falling back to CreatedAt proximity (approximate).
// A thin delegate to cortex.ExpandToTranscript; a pure read. Returns (nil, nil)
// when nothing resolves (fail-open).
func (p *Pager) ExpandToTranscript(memURI string, radius int) (*cortex.TranscriptSlice, error) {
	if p == nil || p.cortex == nil {
		return nil, nil
	}
	return p.cortex.ExpandToTranscript(memory.URI(strings.TrimSpace(memURI)), radius)
}

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

// RecordLoopDeath persists one failure incident in the dedicated derived
// death-journal session lane. It cannot surface through ordinary activation.
func (p *Pager) RecordLoopDeath(ctx context.Context, summary, intentRef string) (string, error) {
	_ = ctx
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", nil
	}
	uri, err := p.cortex.AppendMessage(cortex.Message{
		ConversationID: deathJournalConversation,
		Role:           cortex.RoleToolResult,
		ToolName:       strings.TrimSpace(intentRef),
		Content:        summary,
	})
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
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
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

// opportunityImportance is the declared importance for a captured opportunity
// — moderate (same as a Fact): the proactive queue is durable working state,
// not an inviolable rule.
const opportunityImportance uint8 = 5

// opportunityScanCap bounds how many opportunity records the reader scans
// before ranking. The proactive queue is small by design (per-day cap +
// dedup), so a generous fixed cap surfaces the whole pending set.
const opportunityScanCap = 256

// opportunityDupCosine is the cosine-similarity ceiling above which a new
// opportunity summary is treated as a semantic duplicate of an existing
// pending one. It is the exact mirror of RememberFact's duplicate discipline:
// dupDistanceThreshold is 1-cosine, so the cosine gate is 1 minus it.
const opportunityDupCosine = 1 - dupDistanceThreshold

// RememberOpportunity stores a durable Automatrix opportunity (the proactive
// work queue), deduplicated against existing PENDING opportunities with the
// same discipline as RememberFact: a normalized-statement match (also catches
// case/spacing drift, embedder-independent) OR a semantic-similarity match
// within the duplicate threshold short-circuits the write so a repeatedly
// mentioned need does not accumulate duplicates. Status defaults to pending
// and timestamps are stamped here. Returns the (possibly pre-existing,
// deduped) record URI; an empty summary is a no-op.
func (p *Pager) RememberOpportunity(ctx context.Context, spec OpportunitySpec) (string, error) {
	_ = ctx
	summary := strings.TrimSpace(spec.Summary)
	if summary == "" {
		return "", nil
	}
	spec.Summary = summary
	if !spec.Status.Valid() {
		spec.Status = OpportunityPending
	}
	// Authoritative, fail-closed non-financial re-check (layer 1 of the
	// gating). The extraction model's verdict arrives as eligible_autonomous =
	// !financial; treat its inverse as the model's financial flag and let the
	// deterministic classifier have the final word. This can only tighten the
	// verdict — a model-said-non-financial item that trips a financial keyword,
	// phrase, or symbol is flipped to ineligible — so capture never marks
	// money-touching work eligible for unprompted autonomous execution.
	spec.EligibleAutonomous = EligibleForAutonomy(spec.Summary, !spec.EligibleAutonomous)
	now := time.Now().UTC()
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = now
	}
	spec.UpdatedAt = now

	if dupURI := p.duplicatePendingOpportunity(summary); dupURI != "" {
		return dupURI, nil
	}

	uri, err := p.cortex.Write(p.opportunityHead(opportunityImportance), encodeOpportunityGoal(spec), p.writeMeta())
	return string(uri), err
}

// PendingOpportunities returns ONLY eligible-autonomous, pending opportunities
// — the set the autonomous picker may act on — ranked by confidence x salience
// x recency (highest first) and capped at limit (<=0 = all). Financial
// opportunities (eligible_autonomous=false) and any non-pending item are never
// returned, so the picker structurally cannot select work that must not run
// unprompted.
func (p *Pager) PendingOpportunities(ctx context.Context, limit int) ([]OpportunitySpec, error) {
	_ = ctx
	res, err := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(opportunityTag)},
		Limit: opportunityScanCap,
		Form:  query.FormMedium,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	type ranked struct {
		spec  OpportunitySpec
		score float32
	}
	var cands []ranked
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypeGoal || !headHasOpportunityTag(m.Head) {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		gd, ok := asGoalData(data)
		if !ok {
			continue
		}
		uri := string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion))
		spec := decodeOpportunityGoal(gd, uri)
		if spec.Status != OpportunityPending || !spec.EligibleAutonomous {
			continue
		}
		recency := recencyMultiplier(memory.TypeGoal.String(), unixNanoOrZero(spec.UpdatedAt), now)
		score := spec.Confidence * res.Scores[m.Head.ID] * recency
		cands = append(cands, ranked{spec: spec, score: score})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		// Deterministic tie-break: most-recently-updated first.
		return cands[i].spec.UpdatedAt.After(cands[j].spec.UpdatedAt)
	})
	out := make([]OpportunitySpec, 0, len(cands))
	for _, c := range cands {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, c.spec)
	}
	return out, nil
}

// QueuedOpportunities returns ALL pending opportunities — eligible-autonomous
// AND financial alike — ranked by confidence x salience x recency (highest
// first) and capped at limit (<=0 = all). It is the management-UI reader (req
// 7.3): unlike PendingOpportunities (the autonomous picker's set, which filters
// to eligible_autonomous only and must stay that way so a financial item can
// never be auto-run), this surfaces the financial items too so the user can
// dismiss them or approve one for a normal, gated, user-initiated run. Each
// returned spec carries its EligibleAutonomous flag, so the caller knows which
// items are financial (eligible_autonomous=false). Done/dismissed/in-flight
// items are excluded — only the actionable pending queue is shown.
func (p *Pager) QueuedOpportunities(ctx context.Context, limit int) ([]OpportunitySpec, error) {
	_ = ctx
	res, err := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(opportunityTag)},
		Limit: opportunityScanCap,
		Form:  query.FormMedium,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	type ranked struct {
		spec  OpportunitySpec
		score float32
	}
	var cands []ranked
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypeGoal || !headHasOpportunityTag(m.Head) {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		gd, ok := asGoalData(data)
		if !ok {
			continue
		}
		uri := string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion))
		spec := decodeOpportunityGoal(gd, uri)
		// All pending items, regardless of eligibility — the financial ones are
		// surfaced for explicit approval rather than hidden.
		if spec.Status != OpportunityPending {
			continue
		}
		recency := recencyMultiplier(memory.TypeGoal.String(), unixNanoOrZero(spec.UpdatedAt), now)
		score := spec.Confidence * res.Scores[m.Head.ID] * recency
		cands = append(cands, ranked{spec: spec, score: score})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].spec.UpdatedAt.After(cands[j].spec.UpdatedAt)
	})
	out := make([]OpportunitySpec, 0, len(cands))
	for _, c := range cands {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, c.spec)
	}
	return out, nil
}

// SetOpportunityStatus transitions an opportunity to a new lifecycle status,
// persisted atomically as a single cortex Update (one Pebble batch commit) so
// a restart never loses or double-runs an item. The opportunity tag and all
// other fields are preserved; updated_at is refreshed.
func (p *Pager) SetOpportunityStatus(ctx context.Context, uri string, status OpportunityStatus) error {
	_ = ctx
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: empty uri")
	}
	if !status.Valid() {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: invalid status %q", status)
	}
	t, id, _, err := cortex.ParseURI(memory.URI(uri))
	if err != nil {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: parse uri: %w", err)
	}
	if t != memory.TypeGoal {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: %s is not an opportunity", uri)
	}
	mem, err := p.cortex.ResolveLatest(id)
	if err != nil {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: resolve: %w", err)
	}
	if !headHasOpportunityTag(mem.Head) {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: %s is not an opportunity", uri)
	}
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: decode: %w", err)
	}
	gd, ok := asGoalData(data)
	if !ok {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: %s is not an opportunity", uri)
	}
	spec := decodeOpportunityGoal(gd, uri)
	spec.Status = status
	spec.UpdatedAt = time.Now().UTC()
	if _, err := p.cortex.Update(memory.URI(uri), encodeOpportunityGoal(spec), p.writeMeta()); err != nil {
		return fmt.Errorf("neo/memory: SetOpportunityStatus: update: %w", err)
	}
	return nil
}

// FailOpportunityAttempt records a failed/partial autonomous run attempt on an
// opportunity: it increments the bounded attempts counter and re-sets the
// lifecycle status, persisted atomically as a single cortex Update (one Pebble
// batch commit). While attempts remain under maxAttempts the opportunity is
// returned to PENDING so a later wake can retry it; once the attempts reach the
// ceiling it is DISMISSED (eventually give up rather than retry forever, req
// 5.5). maxAttempts <= 0 disables the ceiling (always re-pend). Returns the new
// status. No user-facing surface is touched — a failed proactive attempt is
// silent (req 5.5: no failed-surprise spam).
func (p *Pager) FailOpportunityAttempt(ctx context.Context, uri string, maxAttempts int) (OpportunityStatus, error) {
	_ = ctx
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: empty uri")
	}
	t, id, _, err := cortex.ParseURI(memory.URI(uri))
	if err != nil {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: parse uri: %w", err)
	}
	if t != memory.TypeGoal {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: %s is not an opportunity", uri)
	}
	mem, err := p.cortex.ResolveLatest(id)
	if err != nil {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: resolve: %w", err)
	}
	if !headHasOpportunityTag(mem.Head) {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: %s is not an opportunity", uri)
	}
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: decode: %w", err)
	}
	gd, ok := asGoalData(data)
	if !ok {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: %s is not an opportunity", uri)
	}
	spec := decodeOpportunityGoal(gd, uri)
	spec.Attempts++
	if maxAttempts > 0 && spec.Attempts >= maxAttempts {
		spec.Status = OpportunityDismissed
	} else {
		spec.Status = OpportunityPending
	}
	spec.UpdatedAt = time.Now().UTC()
	if _, err := p.cortex.Update(memory.URI(uri), encodeOpportunityGoal(spec), p.writeMeta()); err != nil {
		return "", fmt.Errorf("neo/memory: FailOpportunityAttempt: update: %w", err)
	}
	return spec.Status, nil
}

// InProgressOpportunities returns the eligible-autonomous opportunities left in
// the IN_PROGRESS state — the set a process restart must pick back up so a
// proactive task in flight when the daemon died is driven to completion (the
// Task Durability Rule, req 5.1). Only eligible_autonomous items are returned,
// so a restart can never resume work on anything but the restricted (non-
// financial) surface. Unranked (order is irrelevant for resume); capped at
// limit (<=0 = all).
func (p *Pager) InProgressOpportunities(ctx context.Context, limit int) ([]OpportunitySpec, error) {
	_ = ctx
	res, err := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(opportunityTag)},
		Limit: opportunityScanCap,
		Form:  query.FormMedium,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	out := make([]OpportunitySpec, 0)
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypeGoal || !headHasOpportunityTag(m.Head) {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		gd, ok := asGoalData(data)
		if !ok {
			continue
		}
		uri := string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion))
		spec := decodeOpportunityGoal(gd, uri)
		if spec.Status != OpportunityInProgress || !spec.EligibleAutonomous {
			continue
		}
		out = append(out, spec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// OpportunityByURI resolves a single opportunity record by its cortex URI. ok
// is false (with a nil error) when the URI does not resolve to an opportunity
// record. It is the point lookup the management/queue surfaces and the
// autonomous run's lifecycle bookkeeping read through.
func (p *Pager) OpportunityByURI(ctx context.Context, uri string) (OpportunitySpec, bool, error) {
	_ = ctx
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return OpportunitySpec{}, false, nil
	}
	t, id, _, err := cortex.ParseURI(memory.URI(uri))
	if err != nil || t != memory.TypeGoal {
		return OpportunitySpec{}, false, nil
	}
	mem, err := p.cortex.ResolveLatest(id)
	if err != nil {
		return OpportunitySpec{}, false, nil
	}
	if !headHasOpportunityTag(mem.Head) {
		return OpportunitySpec{}, false, nil
	}
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return OpportunitySpec{}, false, fmt.Errorf("neo/memory: OpportunityByURI: decode: %w", err)
	}
	gd, ok := asGoalData(data)
	if !ok {
		return OpportunitySpec{}, false, nil
	}
	return decodeOpportunityGoal(gd, uri), true, nil
}

// opportunityHead is p.head with the opportunity tag attached, so the record
// is discoverable by the tag query and excludable from the ambient window.
func (p *Pager) opportunityHead(importance uint8) memory.Head {
	h := p.head(importance)
	h.Tags = []memory.Tag{opportunityTag}
	return h
}

// pendingOpportunities returns the decoded pending (status=pending)
// opportunity records regardless of eligibility — the dedup target set.
func (p *Pager) pendingOpportunities() []OpportunitySpec {
	res, err := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(opportunityTag)},
		Limit: opportunityScanCap,
		Form:  query.FormMedium,
	})
	if err != nil || res == nil {
		return nil
	}
	out := make([]OpportunitySpec, 0, len(res.Memories))
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypeGoal || !headHasOpportunityTag(m.Head) {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		gd, ok := asGoalData(data)
		if !ok {
			continue
		}
		uri := string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion))
		spec := decodeOpportunityGoal(gd, uri)
		if spec.Status == OpportunityPending {
			out = append(out, spec)
		}
	}
	return out
}

// duplicatePendingOpportunity returns the URI of an existing PENDING
// opportunity that duplicates summary — by normalized-statement equality, or
// (when an embedder is available) by semantic similarity within the duplicate
// threshold. Empty string means no duplicate (write a fresh record). Mirrors
// the normalize + cosine dedup discipline of RememberFact.
func (p *Pager) duplicatePendingOpportunity(summary string) string {
	pending := p.pendingOpportunities()
	if len(pending) == 0 {
		return ""
	}
	norm := normalizeStatement(summary)
	for _, op := range pending {
		if normalizeStatement(op.Summary) == norm {
			return op.URI
		}
	}
	if !p.hasEmbedder || p.embedder == nil {
		return ""
	}
	nv, err := p.embedder.Embed(summary)
	if err != nil {
		return ""
	}
	for _, op := range pending {
		ev, eerr := p.embedder.Embed(op.Summary)
		if eerr != nil {
			continue
		}
		if embed.Cosine(nv, ev) >= opportunityDupCosine {
			return op.URI
		}
	}
	return ""
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
