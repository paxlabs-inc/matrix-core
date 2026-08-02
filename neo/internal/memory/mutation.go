// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/cortex"
	cmemory "matrix/cortex/memory"
	cquery "matrix/cortex/query"
)

const maxMutationItems = 20
const semanticTargetLimit = 5

type MutationOperation string

const (
	MutationCreate    MutationOperation = "create"
	MutationUpdate    MutationOperation = "update"
	MutationSupersede MutationOperation = "supersede"
	MutationDelete    MutationOperation = "delete"
)

// MutationTarget identifies one existing record either exactly or by a
// bounded semantic lookup. Exactly one field is required for non-create ops.
type MutationTarget struct {
	URI   string   `json:"uri,omitempty"`
	Query string   `json:"query,omitempty"`
	Types []string `json:"types,omitempty"`
}

// MutationValue is the typed user-memory payload. Content is the statement for
// facts, beliefs, goals, and constraints, and the topic for a preference.
type MutationValue struct {
	Type      string  `json:"type,omitempty"`
	Content   string  `json:"content,omitempty"`
	Subject   string  `json:"subject,omitempty"`
	Predicate string  `json:"predicate,omitempty"`
	Polarity  string  `json:"polarity,omitempty"`
	Strength  float32 `json:"strength,omitempty"`
	Rationale string  `json:"rationale,omitempty"`
}

type MutationItem struct {
	Operation      MutationOperation `json:"operation"`
	Target         *MutationTarget   `json:"target,omitempty"`
	ReplacementURI string            `json:"replacement_uri,omitempty"`
	Value          *MutationValue    `json:"value,omitempty"`
	Reason         string            `json:"reason,omitempty"`
}

type MutationRequest struct {
	Items              []MutationItem `json:"items"`
	IncludeInternalIDs bool           `json:"include_internal_ids,omitempty"`
}

type MutationResult struct {
	Operation   MutationOperation `json:"operation"`
	Description string            `json:"description"`
	URI         string            `json:"uri,omitempty"`
}

type MutationBatchResult struct {
	Results []MutationResult `json:"results"`
}

type preparedMutation struct {
	item      MutationItem
	targetURI cmemory.URI
	target    *cmemory.Memory
	data      cmemory.TypedData
	head      cmemory.Head
}

// Mutate performs a bounded batch of typed user-memory mutations. Every item
// is fully resolved and decoded before the first write, so ambiguity or an
// invalid payload cannot leave a partially-applied correction batch.
func (p *Pager) Mutate(ctx context.Context, req MutationRequest) (MutationBatchResult, error) {
	if p == nil || p.cortex == nil {
		return MutationBatchResult{}, errors.New("neo/memory: pager unavailable")
	}
	if len(req.Items) == 0 {
		return MutationBatchResult{}, errors.New("memory mutation requires at least one item")
	}
	if len(req.Items) > maxMutationItems {
		return MutationBatchResult{}, fmt.Errorf("memory mutation accepts at most %d items", maxMutationItems)
	}
	// Do not let an in-flight embedder head rewrite race a versioned mutation.
	// Draining here also makes delete/supersede cache effects observable before
	// the typed operation begins its bounded target resolution.
	if p.hasEmbedder {
		if err := p.cortex.DrainEmbedder(ctx); err != nil {
			return MutationBatchResult{}, fmt.Errorf("prepare memory indexes: %w", err)
		}
	}
	prepared := make([]preparedMutation, 0, len(req.Items))
	for i, item := range req.Items {
		if strings.EqualFold(strings.TrimSpace(string(item.Operation)), string(MutationCreate)) && !p.MemoryConsentEnabled() {
			return MutationBatchResult{}, fmt.Errorf("item %d: durable memory is off; enable it after reviewing what will be stored before creating a record", i+1)
		}
		pm, err := p.prepareMutation(ctx, item)
		if err != nil {
			return MutationBatchResult{}, fmt.Errorf("item %d: %w", i+1, err)
		}
		prepared = append(prepared, pm)
	}

	out := MutationBatchResult{Results: make([]MutationResult, 0, len(prepared))}
	for i, pm := range prepared {
		result, err := p.applyMutation(pm, req.IncludeInternalIDs)
		if err != nil {
			return MutationBatchResult{}, fmt.Errorf("item %d: %w", i+1, err)
		}
		out.Results = append(out.Results, result)
	}
	if p.hasEmbedder {
		_ = p.cortex.DrainEmbedder(ctx)
	}
	return out, nil
}

func (p *Pager) prepareMutation(ctx context.Context, item MutationItem) (preparedMutation, error) {
	item.Operation = MutationOperation(strings.ToLower(strings.TrimSpace(string(item.Operation))))
	switch item.Operation {
	case MutationCreate:
		if item.Target != nil && (strings.TrimSpace(item.Target.URI) != "" || strings.TrimSpace(item.Target.Query) != "") {
			return preparedMutation{}, errors.New("create does not accept a target")
		}
		data, err := mutationData(nil, item.Value)
		if err != nil {
			return preparedMutation{}, err
		}
		return preparedMutation{item: item, data: data, head: p.head(defaultMutationImportance(data))}, nil
	case MutationUpdate, MutationSupersede, MutationDelete:
		uri, mem, err := p.resolveMutationTarget(ctx, item.Target)
		if err != nil {
			return preparedMutation{}, err
		}
		pm := preparedMutation{item: item, targetURI: uri, target: mem}
		if item.Operation != MutationDelete {
			pm.data, err = mutationData(mem, item.Value)
			if err != nil {
				return preparedMutation{}, err
			}
			pm.head = replacementHead(mem.Head)
		}
		return pm, nil
	default:
		return preparedMutation{}, fmt.Errorf("unsupported operation %q", item.Operation)
	}
}

func (p *Pager) applyMutation(pm preparedMutation, includeURI bool) (MutationResult, error) {
	meta := cortex.WriteMeta{
		CreatedBy:  p.cfg.CortexActor,
		Provenance: cmemory.Provenance{Source: cmemory.SourceUserInput},
	}
	var uri cmemory.URI
	var err error
	switch pm.item.Operation {
	case MutationCreate:
		uri, err = p.cortex.Write(pm.head, pm.data, meta)
	case MutationUpdate:
		uri, err = p.cortex.Update(pm.targetURI, pm.data, meta)
	case MutationSupersede:
		uri, err = p.cortex.Supersede(pm.targetURI, pm.data, cortex.SupersedeOptions{
			ReplacementURI: cmemory.URI(strings.TrimSpace(pm.item.ReplacementURI)),
			Head:           pm.head,
			WriteMeta:      meta,
			EdgeMeta: cortex.AddEdgeMeta{
				CreatedBy: p.cfg.CortexActor,
				Data:      []byte(strings.TrimSpace(pm.item.Reason)),
			},
		})
	case MutationDelete:
		reason := strings.TrimSpace(pm.item.Reason)
		if reason == "" {
			reason = "deleted by user"
		}
		err = p.cortex.Tombstone(pm.targetURI, reason, p.cfg.CortexActor)
	}
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{
		Operation:   pm.item.Operation,
		Description: mutationDescription(pm.item.Operation, pm.data, pm.target),
	}
	if includeURI && uri != "" {
		result.URI = string(uri)
	}
	return result, nil
}

func (p *Pager) resolveMutationTarget(ctx context.Context, target *MutationTarget) (cmemory.URI, *cmemory.Memory, error) {
	if target == nil {
		return "", nil, errors.New("target is required")
	}
	rawURI := strings.TrimSpace(target.URI)
	queryText := strings.TrimSpace(target.Query)
	if (rawURI == "") == (queryText == "") {
		return "", nil, errors.New("target requires exactly one of uri or query")
	}
	if rawURI != "" {
		_, id, _, err := cortex.ParseURI(cmemory.URI(rawURI))
		if err != nil {
			return "", nil, fmt.Errorf("invalid target uri: %w", err)
		}
		mem, err := p.cortex.ResolveLatest(id)
		if err != nil {
			return "", nil, fmt.Errorf("resolve target: %w", err)
		}
		return cortex.BuildURI(mem.Head.Type, id, mem.Head.CurrentVersion), mem, nil
	}

	hits, err := p.RecallHits(ctx, queryText, target.Types, semanticTargetLimit, nil)
	if err != nil {
		if p.cfg.InteractionPosture {
			if uri, mem, found, exactErr := p.resolveExactCurrentMutationTarget(queryText, target.Types); exactErr != nil {
				return "", nil, exactErr
			} else if found {
				return uri, mem, nil
			}
			return "", nil, errors.New("semantic target matched no current memory")
		}
		return "", nil, fmt.Errorf("semantic target lookup: %w", err)
	}
	if len(hits) == 0 {
		if p.cfg.InteractionPosture {
			if uri, mem, found, exactErr := p.resolveExactCurrentMutationTarget(queryText, target.Types); exactErr != nil {
				return "", nil, exactErr
			} else if found {
				return uri, mem, nil
			}
		}
		return "", nil, errors.New("semantic target matched no current memory")
	}
	type candidate struct {
		uri cmemory.URI
		mem *cmemory.Memory
	}
	candidates := make([]candidate, 0, len(hits))
	exact := make([]candidate, 0, 1)
	normQuery := normalizeMutationText(queryText)
	for _, hit := range hits {
		_, id, _, err := cortex.ParseURI(cmemory.URI(hit.URI))
		if err != nil {
			continue
		}
		mem, err := p.cortex.ResolveLatest(id)
		if err != nil || mem.Head.Tombstoned != nil || mem.Version.ValidUntil != nil {
			continue
		}
		cand := candidate{uri: cortex.BuildURI(mem.Head.Type, id, mem.Head.CurrentVersion), mem: mem}
		candidates = append(candidates, cand)
		primary := normalizeMutationText(primaryMutationText(mem))
		if primary == normQuery || (len(normQuery) >= 8 && strings.Contains(primary, normQuery)) {
			exact = append(exact, cand)
		}
	}
	if len(exact) == 1 {
		return exact[0].uri, exact[0].mem, nil
	}
	if len(candidates) == 1 {
		return candidates[0].uri, candidates[0].mem, nil
	}
	if len(candidates) == 0 {
		if p.cfg.InteractionPosture {
			if uri, mem, found, exactErr := p.resolveExactCurrentMutationTarget(queryText, target.Types); exactErr != nil {
				return "", nil, exactErr
			} else if found {
				return uri, mem, nil
			}
		}
		return "", nil, errors.New("semantic target matched no live memory")
	}
	return "", nil, fmt.Errorf("semantic target is ambiguous across %d current memories; narrow the query or use an explicit uri", len(candidates))
}

func (p *Pager) resolveExactCurrentMutationTarget(queryText string, typeNames []string) (cmemory.URI, *cmemory.Memory, bool, error) {
	types := parseRecallTypes(typeNames)
	if len(types) == 0 {
		types = []cmemory.Type{
			cmemory.TypeFact,
			cmemory.TypePreference,
			cmemory.TypeBelief,
			cmemory.TypeGoal,
			cmemory.TypeConstraint,
		}
	}
	res, err := p.cortex.Find(cquery.Query{Type: types, Limit: 512})
	if err != nil {
		return "", nil, false, fmt.Errorf("exact current target lookup: %w", err)
	}
	normQuery := normalizeMutationText(queryText)
	type candidate struct {
		uri cmemory.URI
		mem *cmemory.Memory
	}
	var matches []candidate
	if res != nil {
		for index := range res.Memories {
			mem := res.Memories[index]
			primary := normalizeMutationText(primaryMutationText(mem))
			if primary == "" || (primary != normQuery && !strings.Contains(primary, normQuery) && !strings.Contains(normQuery, primary)) {
				continue
			}
			matches = append(matches, candidate{
				uri: cortex.BuildURI(mem.Head.Type, mem.Head.ID, mem.Head.CurrentVersion),
				mem: mem,
			})
		}
	}
	if len(matches) == 0 {
		return "", nil, false, nil
	}
	if len(matches) > 1 {
		return "", nil, false, fmt.Errorf("semantic target is ambiguous across %d exact current memories; narrow the query or use an explicit uri", len(matches))
	}
	return matches[0].uri, matches[0].mem, true, nil
}

func mutationData(existing *cmemory.Memory, value *MutationValue) (cmemory.TypedData, error) {
	if value == nil {
		return nil, errors.New("value is required")
	}
	typeName := strings.ToLower(strings.TrimSpace(value.Type))
	if existing != nil {
		typeName = strings.ToLower(existing.Head.Type.String())
		if strings.TrimSpace(value.Type) != "" && typeName != strings.ToLower(strings.TrimSpace(value.Type)) {
			return nil, errors.New("value type does not match target type")
		}
	}
	content := strings.TrimSpace(value.Content)
	if content == "" {
		return nil, errors.New("value.content is required")
	}
	var current cmemory.TypedData
	if existing != nil {
		var err error
		current, err = cmemory.DecodeData(existing.Version.Type, existing.Version.Data)
		if err != nil {
			return nil, err
		}
	}
	switch typeName {
	case "fact":
		v := cmemory.FactData{SchemaVersion: 1, Subject: userFactSubject, Predicate: userFactPredicate}
		if old, ok := current.(cmemory.FactData); ok {
			v = old
		}
		v.Statement = content
		if strings.TrimSpace(value.Subject) != "" {
			v.Subject = strings.TrimSpace(value.Subject)
		}
		if strings.TrimSpace(value.Predicate) != "" {
			v.Predicate = strings.TrimSpace(value.Predicate)
		}
		return v, nil
	case "preference":
		v := cmemory.PreferenceData{SchemaVersion: 1, Polarity: cmemory.PolarityPrefer, StrengthVal: 1}
		if old, ok := current.(cmemory.PreferenceData); ok {
			v = old
		}
		v.Topic = content
		if strings.TrimSpace(value.Polarity) != "" {
			v.Polarity = normalizePolarity(value.Polarity, cmemory.PolarityPrefer)
		}
		if value.Strength > 0 {
			v.StrengthVal = clampUnit(value.Strength)
		}
		if strings.TrimSpace(value.Rationale) != "" {
			v.Rationale = strings.TrimSpace(value.Rationale)
		}
		return v, nil
	case "belief":
		v := cmemory.BeliefData{SchemaVersion: 1, Stance: cmemory.StanceBelieve}
		if old, ok := current.(cmemory.BeliefData); ok {
			v = old
		}
		v.Statement = content
		if strings.TrimSpace(value.Subject) != "" {
			v.Subject = strings.TrimSpace(value.Subject)
		}
		return v, nil
	case "goal":
		v := cmemory.GoalData{SchemaVersion: 1, Status: cmemory.GoalActive}
		if old, ok := current.(cmemory.GoalData); ok {
			v = old
		}
		v.Statement = content
		return v, nil
	case "constraint":
		v := cmemory.ConstraintData{
			SchemaVersion: 1, Polarity: cmemory.PolarityDo,
			StrengthVal: cmemory.StrengthFirm, Source: cmemory.ConstraintSourceUserDeclared,
		}
		if old, ok := current.(cmemory.ConstraintData); ok {
			v = old
		}
		v.Statement = content
		if strings.TrimSpace(value.Polarity) != "" {
			v.Polarity = normalizePolarity(value.Polarity, cmemory.PolarityDo)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported user-memory type %q", value.Type)
	}
}

func replacementHead(old cmemory.Head) cmemory.Head {
	old.ID = cmemory.ID{}
	old.CurrentVersion = 0
	old.Tombstoned = nil
	old.LastUpdatedAt = time.Time{}
	old.EmbeddingRef = nil
	old.Forms = cmemory.Forms{}
	return old
}

func defaultMutationImportance(data cmemory.TypedData) uint8 {
	switch cmemory.TypeOf(data) {
	case cmemory.TypePreference:
		return 7
	case cmemory.TypeConstraint:
		return 8
	case cmemory.TypeGoal:
		return 6
	default:
		return 5
	}
}

func primaryMutationText(mem *cmemory.Memory) string {
	if mem == nil {
		return ""
	}
	data, err := cmemory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return mem.Version.Forms.Medium
	}
	switch v := data.(type) {
	case cmemory.FactData:
		return v.Statement
	case cmemory.PreferenceData:
		return v.Topic
	case cmemory.BeliefData:
		return v.Statement
	case cmemory.GoalData:
		return v.Statement
	case cmemory.ConstraintData:
		return v.Statement
	default:
		return mem.Version.Forms.Medium
	}
}

func normalizeMutationText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func mutationDescription(op MutationOperation, data cmemory.TypedData, target *cmemory.Memory) string {
	typeName := "memory"
	text := primaryMutationText(target)
	if data != nil {
		typeName = strings.ToLower(cmemory.TypeOf(data).String())
		encoded, err := cmemory.EncodeData(data)
		if err == nil {
			text = primaryMutationText(&cmemory.Memory{Version: cmemory.Version{Type: cmemory.TypeOf(data), Data: encoded}})
		}
	} else if target != nil {
		typeName = strings.ToLower(target.Head.Type.String())
	}
	switch op {
	case MutationCreate:
		return fmt.Sprintf("Created the %s: %s.", typeName, text)
	case MutationUpdate:
		return fmt.Sprintf("Updated the %s to: %s.", typeName, text)
	case MutationSupersede:
		return fmt.Sprintf("Replaced the previous %s with: %s.", typeName, text)
	case MutationDelete:
		return fmt.Sprintf("Deleted the %s: %s.", typeName, text)
	default:
		return "Memory updated."
	}
}
