// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode"

	"matrix/cortex"
	cmemory "matrix/cortex/memory"
	"matrix/cortex/query"
)

const typedUpsertScanCap = 512

func (p *Pager) UpsertFact(ctx context.Context, subject, predicate, statement string) (string, error) {
	return p.upsertFact(ctx, subject, predicate, statement, 5)
}

func (p *Pager) upsertFact(ctx context.Context, subject, predicate, statement string, importance uint8) (string, error) {
	_ = ctx
	subject = strings.TrimSpace(subject)
	predicate = canonicalField(predicate)
	statement = strings.TrimSpace(statement)
	if subject == "" || predicate == "" || statement == "" {
		return "", nil
	}
	data := cmemory.FactData{
		SchemaVersion: 1, Subject: subject, Predicate: predicate,
		Statement: statement,
	}
	predicateFilter := query.Predicate(query.Eq{Field: "data.predicate", Value: predicate})
	acceptsLegacyUserFact := subject == userFactSubject && predicate != userFactPredicate
	if acceptsLegacyUserFact {
		predicateFilter = query.Or{Children: []query.Predicate{
			query.Eq{Field: "data.predicate", Value: predicate},
			query.Eq{Field: "data.predicate", Value: userFactPredicate},
		}}
	}
	return p.upsertCurrent(cmemory.TypeFact, data, importance, query.And{Children: []query.Predicate{
		query.Eq{Field: "data.subject", Value: subject},
		predicateFilter,
	}}, func(existing cmemory.TypedData) bool {
		fact, ok := asFact(existing)
		if !ok || strings.TrimSpace(fact.Subject) != subject {
			return false
		}
		existingPredicate := canonicalField(fact.Predicate)
		return existingPredicate == predicate ||
			acceptsLegacyUserFact && existingPredicate == userFactPredicate &&
				legacyUserFactPredicate(fact.Statement) == predicate
	})
}

func (p *Pager) UpsertUserFact(ctx context.Context, predicate, statement string) (string, error) {
	predicate = canonicalField(predicate)
	if predicate == "" {
		return "", nil
	}
	return p.upsertFact(ctx, userFactSubject, predicate, statement, 7)
}

func (p *Pager) UpsertPreference(ctx context.Context, topic, polarity string, strength float32, rationale string) (string, error) {
	_ = ctx
	topic = strings.TrimSpace(topic)
	key := normalizeStatement(topic)
	if key == "" {
		return "", nil
	}
	data := cmemory.PreferenceData{
		SchemaVersion: 1, Topic: topic,
		Polarity:    normalizePolarity(polarity, cmemory.PolarityPrefer),
		StrengthVal: clampUnit(strength), Rationale: strings.TrimSpace(rationale),
	}
	return p.upsertCurrent(cmemory.TypePreference, data, 7, nil, func(existing cmemory.TypedData) bool {
		preference, ok := asPreference(existing)
		return ok && normalizeStatement(preference.Topic) == key
	})
}

func (p *Pager) UpsertConstraint(ctx context.Context, statement, polarity, strength string) (string, error) {
	_ = ctx
	statement = strings.TrimSpace(statement)
	key := normalizeStatement(statement)
	if key == "" {
		return "", nil
	}
	data := cmemory.ConstraintData{
		SchemaVersion: 1, Statement: statement,
		Polarity:    normalizePolarity(polarity, cmemory.PolarityDo),
		StrengthVal: normalizeStrength(strength),
		Source:      cmemory.ConstraintSourceLearned,
	}
	return p.upsertCurrent(cmemory.TypeConstraint, data, 8, nil, func(existing cmemory.TypedData) bool {
		constraint, ok := asConstraint(existing)
		return ok && normalizeStatement(constraint.Statement) == key
	})
}

func (p *Pager) UpsertOutcome(ctx context.Context, summary string, outcome cmemory.Outcome, intentID string) (string, error) {
	_ = ctx
	summary = strings.TrimSpace(summary)
	intentID = strings.TrimSpace(intentID)
	if summary == "" || intentID == "" {
		return "", nil
	}
	data := cmemory.EventData{
		SchemaVersion: 1, Kind: cmemory.EventObservation,
		OutcomeVal: outcome, Summary: summary, IntentRef: intentID,
	}
	return p.upsertCurrent(cmemory.TypeEvent, data, 4,
		query.Eq{Field: "data.intent_ref", Value: intentID},
		func(existing cmemory.TypedData) bool {
			event, ok := asEvent(existing)
			return ok && event.IntentRef == intentID && !strings.HasPrefix(event.IntentRef, "death:")
		})
}

func (p *Pager) upsertCurrent(t cmemory.Type, data cmemory.TypedData, importance uint8, where query.Predicate, sameIdentity func(cmemory.TypedData) bool) (string, error) {
	if p == nil || p.cortex == nil || data == nil {
		return "", nil
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	res, err := p.cortex.Find(query.Query{
		Type: []cmemory.Type{t}, Where: where, Limit: typedUpsertScanCap,
	})
	if err != nil {
		return "", err
	}
	type match struct {
		uri  cmemory.URI
		data cmemory.TypedData
	}
	var matches []match
	if res != nil {
		for _, candidate := range res.Memories {
			decoded, decodeErr := cmemory.DecodeData(candidate.Version.Type, candidate.Version.Data)
			if decodeErr != nil || !sameIdentity(decoded) {
				continue
			}
			matches = append(matches, match{
				uri:  cortex.BuildURI(candidate.Head.Type, candidate.Head.ID, candidate.Head.CurrentVersion),
				data: decoded,
			})
		}
	}
	if len(matches) == 0 {
		uri, writeErr := p.cortex.Write(p.head(importance), data, p.writeMeta())
		return string(uri), writeErr
	}

	desired, err := cmemory.EncodeData(data)
	if err != nil {
		return "", err
	}
	canonical := 0
	for i, candidate := range matches {
		encoded, encodeErr := cmemory.EncodeData(candidate.data)
		if encodeErr == nil && bytes.Equal(encoded, desired) {
			canonical = i
			break
		}
	}
	replacementURI := matches[canonical].uri
	encoded, encodeErr := cmemory.EncodeData(matches[canonical].data)
	if encodeErr != nil || !bytes.Equal(encoded, desired) {
		replacementURI, err = p.cortex.Supersede(matches[canonical].uri, data, cortex.SupersedeOptions{
			Head: p.head(importance), WriteMeta: p.writeMeta(),
			EdgeMeta: cortex.AddEdgeMeta{CreatedBy: p.cfg.CortexActor},
		})
		if err != nil {
			return "", err
		}
	}
	for i, duplicate := range matches {
		if i == canonical {
			continue
		}
		replacementURI, err = p.cortex.Supersede(duplicate.uri, data, cortex.SupersedeOptions{
			ReplacementURI: replacementURI,
			Head:           p.head(importance), WriteMeta: p.writeMeta(),
			EdgeMeta: cortex.AddEdgeMeta{
				CreatedBy: p.cfg.CortexActor,
				Data:      []byte("canonical identity merge"),
			},
		})
		if err != nil {
			return "", fmt.Errorf("merge duplicate %s: %w", duplicate.uri, err)
		}
	}
	return string(replacementURI), nil
}

func canonicalField(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == ':':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func canonicalRetrievalIdentity(data cmemory.TypedData) string {
	switch value := data.(type) {
	case cmemory.FactData:
		return canonicalFactRetrievalIdentity(value)
	case *cmemory.FactData:
		return canonicalFactRetrievalIdentity(*value)
	case cmemory.PreferenceData:
		return "preference\x00" + normalizeStatement(value.Topic)
	case *cmemory.PreferenceData:
		return "preference\x00" + normalizeStatement(value.Topic)
	case cmemory.ConstraintData:
		return "constraint\x00" + normalizeStatement(value.Statement)
	case *cmemory.ConstraintData:
		return "constraint\x00" + normalizeStatement(value.Statement)
	case cmemory.EventData:
		return canonicalEventRetrievalIdentity(value)
	case *cmemory.EventData:
		return canonicalEventRetrievalIdentity(*value)
	case cmemory.PatternData:
		return "pattern\x00" + DecodePatternSpec(value.Statement).dedupKey()
	case *cmemory.PatternData:
		return "pattern\x00" + DecodePatternSpec(value.Statement).dedupKey()
	default:
		return ""
	}
}

func canonicalFactRetrievalIdentity(fact cmemory.FactData) string {
	subject := strings.TrimSpace(fact.Subject)
	predicate := canonicalField(fact.Predicate)
	if subject == userFactSubject && predicate == userFactPredicate {
		if inferred := legacyUserFactPredicate(fact.Statement); inferred != "" {
			predicate = inferred
		}
	}
	if subject == userFactSubject && predicate != "" && predicate != userFactPredicate {
		return "fact\x00" + subject + "\x00" + predicate
	}
	return "fact\x00" + subject + "\x00" + predicate + "\x00" + normalizeStatement(fact.Statement)
}

func legacyUserFactPredicate(statement string) string {
	statement = normalizeStatement(statement)
	switch {
	case strings.Contains(statement, "user's name"),
		strings.Contains(statement, "user name"),
		strings.Contains(statement, "name is"):
		return "name"
	case strings.Contains(statement, "user lives in"),
		strings.Contains(statement, "user is based in"):
		return "location"
	case strings.Contains(statement, "user works as"),
		strings.Contains(statement, "user's role"):
		return "role"
	default:
		return ""
	}
}

func canonicalEventRetrievalIdentity(event cmemory.EventData) string {
	if intentID := strings.TrimSpace(event.IntentRef); intentID != "" {
		return "event\x00intent\x00" + intentID
	}
	return "event\x00" + string(event.Kind) + "\x00" + string(event.OutcomeVal) +
		"\x00" + normalizeStatement(event.Summary)
}

func asFact(data cmemory.TypedData) (cmemory.FactData, bool) {
	switch value := data.(type) {
	case cmemory.FactData:
		return value, true
	case *cmemory.FactData:
		return *value, true
	default:
		return cmemory.FactData{}, false
	}
}

func asPreference(data cmemory.TypedData) (cmemory.PreferenceData, bool) {
	switch value := data.(type) {
	case cmemory.PreferenceData:
		return value, true
	case *cmemory.PreferenceData:
		return *value, true
	default:
		return cmemory.PreferenceData{}, false
	}
}

func asConstraint(data cmemory.TypedData) (cmemory.ConstraintData, bool) {
	switch value := data.(type) {
	case cmemory.ConstraintData:
		return value, true
	case *cmemory.ConstraintData:
		return *value, true
	default:
		return cmemory.ConstraintData{}, false
	}
}

func asEvent(data cmemory.TypedData) (cmemory.EventData, bool) {
	switch value := data.(type) {
	case cmemory.EventData:
		return value, true
	case *cmemory.EventData:
		return *value, true
	default:
		return cmemory.EventData{}, false
	}
}
