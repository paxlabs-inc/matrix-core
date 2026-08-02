package businessoutcome

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

func (store *Store) CommitOutcome(ctx context.Context, value VerifiedOutcome) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.ValidateAt(now) != nil || value.Record.Body.OrganizationID != store.organizationID ||
		value.Record.Body.DerivedAt.After(now) || value.Review.VerifiedAt.After(now) {
		return false, ErrUnauthorized
	}
	body := value.Record.Body
	definition, err := store.LoadMetric(ctx, body.Metric, true)
	if err != nil {
		return false, err
	}
	if body.InitiativeID != definition.Body.InitiativeID || body.Kind != definition.Body.OutcomeKind ||
		body.OrganizationID != definition.Body.OrganizationID || body.Value.Unit != definition.Body.Measure.Unit ||
		body.Value.Scale != definition.Body.Measure.Scale {
		return false, ErrConflict
	}
	observations := make([]Observation, 0, len(body.Observations))
	latestCaptured := time.Time{}
	freshUntil := definition.Body.ExpiresAt
	for _, binding := range body.Observations {
		observation, err := store.LoadObservation(ctx, binding.ID)
		if err != nil {
			return false, err
		}
		if observation.ContentHash != binding.Hash || observation.Body.Metric != body.Metric ||
			observation.Body.InitiativeID != body.InitiativeID || observation.GateSafe(definition, now) != nil {
			return false, ErrReconciliationRequired
		}
		contaminated, err := store.isContaminated(ctx, "observation", string(binding.ID), binding.Hash.Digest)
		if err != nil {
			return false, err
		}
		if contaminated {
			return false, ErrContaminated
		}
		if observation.Body.CapturedAt.After(latestCaptured) {
			latestCaptured = observation.Body.CapturedAt
		}
		if observation.Body.FreshUntil.Before(freshUntil) {
			freshUntil = observation.Body.FreshUntil
		}
		observations = append(observations, observation)
	}
	slices.SortFunc(observations, func(left, right Observation) int {
		if left.Body.ObservedAt.Before(right.Body.ObservedAt) {
			return -1
		}
		if left.Body.ObservedAt.After(right.Body.ObservedAt) {
			return 1
		}
		return strings.Compare(string(left.Body.ID), string(right.Body.ID))
	})
	values := make([]MeasurementValue, 0, len(observations))
	for _, observation := range observations {
		values = append(values, observation.Body.Value)
	}
	aggregate, err := AggregateValues(definition.Body.Aggregation, values)
	if err != nil || aggregate != body.Value || ThresholdFor(definition.Body, aggregate) != body.ThresholdResult ||
		body.DerivedAt.Before(latestCaptured) || body.FreshUntil.After(freshUntil) ||
		value.Review.ExpiresAt.After(freshUntil) {
		return false, ErrConflict
	}
	recordHash, err := OutcomeRecordHash(value.Record)
	if err != nil {
		return false, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.outcomeAD(body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal outcome: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin outcome commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "outcome", string(body.ChainID)); err != nil {
		return false, err
	}
	authorKey, authorRole, authorDepartment, err := store.resolveCurrentSeatKeyTx(ctx, tx, body.AuthorSeatID, value.Record.Signature.KeyID, now)
	if err != nil || authorRole == "auditor" || VerifyOutcomeRecord(value.Record, authorKey) != nil {
		return false, ErrUnauthorized
	}
	verifierKey, verifierRole, verifierDepartment, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.Review.VerifierSeatID, value.Review.Signature.KeyID, now)
	if err != nil || verifierRole != "auditor" || verifierDepartment == authorDepartment ||
		VerifyIndependentReview(value.Review, verifierKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_business_outcomes
		WHERE tenant_id=$1 AND organization_id=$2 AND outcome_id=$3
	`, store.tenantID, store.organizationID, body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("business outcome: inspect outcome replay: %w", err)
	}
	var headID string
	var headVersion uint64
	var headMetricID, headInitiative, headRecordHash string
	err = tx.QueryRow(ctx, `
		SELECT outcome_id,version,metric_id,initiative_id,record_hash
		FROM workforce_business_outcome_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND chain_id=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, body.ChainID).Scan(&headID, &headVersion, &headMetricID, &headInitiative, &headRecordHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (body.Version != 1 || body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (body.Version != headVersion+1 || body.Supersedes == nil ||
		string(*body.Supersedes) != headID || string(body.Metric.ID) != headMetricID ||
		body.InitiativeID != headInitiative):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("business outcome: inspect outcome head: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_outcomes (
			tenant_id,organization_id,outcome_id,chain_id,version,initiative_id,
			metric_id,metric_version,definition_hash,outcome_kind,threshold_result,
			value_micros,numerator_micros,denominator,unit,scale,record_hash,
			canonical_hash,author_seat_id,author_key_id,verifier_seat_id,
			verifier_key_id,sealed_outcome,derived_at,fresh_until,verified_at,
			review_expires_at,supersedes
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27,$28
		)
	`, store.tenantID, store.organizationID, body.ID, body.ChainID, body.Version,
		body.InitiativeID, body.Metric.ID, body.Metric.Version, body.Metric.DefinitionHash.Digest,
		body.Kind, body.ThresholdResult, body.Value.ValueMicros, body.Value.NumeratorMicros,
		body.Value.Denominator, body.Value.Unit, body.Value.Scale, recordHash.Digest,
		canonicalHash.Digest, body.AuthorSeatID, value.Record.Signature.KeyID,
		value.Review.VerifierSeatID, value.Review.Signature.KeyID, sealed, body.DerivedAt,
		body.FreshUntil, value.Review.VerifiedAt, value.Review.ExpiresAt,
		optionalOutcomeID(body.Supersedes))
	if err != nil {
		return false, fmt.Errorf("business outcome: insert outcome: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_outcome_heads (
			tenant_id,organization_id,chain_id,outcome_id,version,initiative_id,
			metric_id,outcome_kind,record_hash,fresh_until,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (tenant_id,organization_id,chain_id) DO UPDATE SET
			outcome_id=EXCLUDED.outcome_id,version=EXCLUDED.version,
			initiative_id=EXCLUDED.initiative_id,metric_id=EXCLUDED.metric_id,
			outcome_kind=EXCLUDED.outcome_kind,record_hash=EXCLUDED.record_hash,
			fresh_until=EXCLUDED.fresh_until,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, body.ChainID, body.ID, body.Version,
		body.InitiativeID, body.Metric.ID, body.Kind, recordHash.Digest, body.FreshUntil, now)
	if err != nil {
		return false, fmt.Errorf("business outcome: update outcome head: %w", err)
	}
	if body.Supersedes != nil {
		if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
			body.InitiativeID,
			RecordPointer{Kind: "outcome", ID: string(*body.Supersedes), Hash: contracts.ContentHash{Algorithm: "sha256", Digest: headRecordHash}},
			RecordPointer{Kind: "outcome", ID: string(body.ID), Hash: recordHash},
			"superseded_by", true, body.AuthorSeatID, value.Record.Signature.KeyID, now); err != nil {
			return false, err
		}
	}
	for _, binding := range body.Observations {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_business_outcome_observations (
				tenant_id,organization_id,outcome_id,observation_id,observation_hash,created_at
			) VALUES ($1,$2,$3,$4,$5,$6)
		`, store.tenantID, store.organizationID, body.ID, binding.ID, binding.Hash.Digest, now)
		if err != nil {
			return false, fmt.Errorf("business outcome: bind outcome observation: %w", err)
		}
		if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
			body.InitiativeID, RecordPointer{Kind: "observation", ID: string(binding.ID), Hash: binding.Hash},
			RecordPointer{Kind: "outcome", ID: string(body.ID), Hash: recordHash},
			"derived_outcome", true, body.AuthorSeatID, value.Record.Signature.KeyID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit outcome: %w", err)
	}
	return false, nil
}

func (store *Store) LoadOutcome(ctx context.Context, id OutcomeID, requireFresh bool) (VerifiedOutcome, error) {
	if validateToken("outcome_id", string(id)) != nil {
		return VerifiedOutcome{}, fmt.Errorf("business outcome: outcome identity is invalid")
	}
	var expectedHash, authorSeatID, authorKeyID, verifierSeatID, verifierKeyID string
	var sealed []byte
	var derivedAt, verifiedAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,author_seat_id,author_key_id,verifier_seat_id,
		       verifier_key_id,sealed_outcome,derived_at,verified_at
		FROM workforce_business_outcomes
		WHERE tenant_id=$1 AND organization_id=$2 AND outcome_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&expectedHash, &authorSeatID,
		&authorKeyID, &verifierSeatID, &verifierKeyID, &sealed, &derivedAt, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedOutcome{}, ErrNotFound
	}
	if err != nil {
		return VerifiedOutcome{}, fmt.Errorf("business outcome: load outcome: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.outcomeAD(id), sealed)
	if err != nil {
		return VerifiedOutcome{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[VerifiedOutcome, *VerifiedOutcome](opened)
	if err != nil || value.Record.Body.ID != id || value.Record.Body.OrganizationID != store.organizationID ||
		string(value.Record.Body.AuthorSeatID) != authorSeatID || value.Record.Signature.KeyID != authorKeyID ||
		string(value.Review.VerifierSeatID) != verifierSeatID || value.Review.Signature.KeyID != verifierKeyID {
		return VerifiedOutcome{}, ErrIntegrity
	}
	actualHash, err := contracts.HashCanonical(&value)
	if err != nil || actualHash.Digest != expectedHash {
		return VerifiedOutcome{}, ErrIntegrity
	}
	authorKey, err := store.resolveHistoricalSeatKey(ctx, value.Record.Body.AuthorSeatID, authorKeyID, derivedAt)
	if err != nil || VerifyOutcomeRecord(value.Record, authorKey) != nil {
		return VerifiedOutcome{}, ErrIntegrity
	}
	verifierKey, err := store.resolveHistoricalSeatKey(ctx, value.Review.VerifierSeatID, verifierKeyID, verifiedAt)
	if err != nil || VerifyIndependentReview(value.Review, verifierKey) != nil {
		return VerifiedOutcome{}, ErrIntegrity
	}
	validationTime := verifiedAt
	if requireFresh {
		validationTime, err = store.currentTime()
		if err != nil {
			return VerifiedOutcome{}, err
		}
	}
	if value.ValidateAt(validationTime) != nil {
		if requireFresh {
			return VerifiedOutcome{}, ErrStale
		}
		return VerifiedOutcome{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) EvaluateGate(ctx context.Context, requirement GateRequirement) (GateDecision, bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return GateDecision{}, false, err
	}
	if requirement.Validate() != nil || requirement.OrganizationID != store.organizationID ||
		requirement.EvaluateAt.After(now) {
		return GateDecision{}, false, ErrUnauthorized
	}
	outcome, err := store.LoadOutcome(ctx, requirement.OutcomeID, true)
	if err != nil {
		return GateDecision{}, false, err
	}
	recordHash, err := OutcomeRecordHash(outcome.Record)
	if err != nil {
		return GateDecision{}, false, err
	}
	body := outcome.Record.Body
	reasons := make([]string, 0, 16)
	guardrailOutcomes := make([]RecordPointer, 0, 8)
	blocked := false
	if body.OrganizationID != requirement.OrganizationID || body.InitiativeID != requirement.InitiativeID ||
		body.Metric != requirement.Metric || body.Kind != requirement.OutcomeKind {
		reasons = append(reasons, "incompatible_outcome_identity")
		blocked = true
	}
	if requirement.Purpose == GateBusinessSuccess && !body.Kind.BusinessOutcome() {
		reasons = append(reasons, "activity_or_output_is_not_business_success")
		blocked = true
	}
	definition, metricErr := store.LoadMetric(ctx, requirement.Metric, true)
	if metricErr != nil {
		reasons = append(reasons, "metric_not_current")
		blocked = true
	} else {
		guardrails, guardrailReasons, guardrailErr := store.evaluateGuardrails(ctx, definition, now)
		if guardrailErr != nil {
			return GateDecision{}, false, guardrailErr
		}
		guardrailOutcomes = guardrails
		if len(guardrailReasons) != 0 {
			reasons = append(reasons, guardrailReasons...)
			blocked = true
		}
	}
	contaminated, contaminationErr := store.isContaminated(ctx, "outcome", string(body.ID), recordHash.Digest)
	if contaminationErr != nil {
		return GateDecision{}, false, contaminationErr
	}
	if contaminated {
		reasons = append(reasons, "outcome_contaminated")
		blocked = true
	}
	sourceSet := make(map[SourceFamily]bool)
	for _, binding := range body.Observations {
		observation, loadErr := store.LoadObservation(ctx, binding.ID)
		if loadErr != nil || observation.ContentHash != binding.Hash {
			reasons = append(reasons, "observation_missing_or_changed")
			blocked = true
			continue
		}
		if metricErr == nil {
			if safeErr := observation.GateSafe(definition, now); safeErr != nil {
				reasons = append(reasons, gateReason(safeErr))
				blocked = true
			}
		}
		if observation.Body.UncertaintyBPS > requirement.MaximumUncertaintyBPS {
			reasons = append(reasons, "uncertainty_exceeds_gate")
			blocked = true
		}
		observationContaminated, contaminationErr := store.isContaminated(ctx, "observation", string(binding.ID), binding.Hash.Digest)
		if contaminationErr != nil {
			return GateDecision{}, false, contaminationErr
		}
		if observationContaminated {
			reasons = append(reasons, "observation_contaminated")
			blocked = true
		}
		sourceSet[observation.Body.Primary.Family] = true
		for _, source := range observation.Body.Supporting {
			sourceSet[source.Family] = true
		}
	}
	for _, required := range requirement.RequiredSources {
		if !sourceSet[required] {
			reasons = append(reasons, "required_source_missing:"+string(required))
			blocked = true
		}
	}
	if body.Value.Denominator < requirement.MinimumDenominator {
		reasons = append(reasons, "minimum_denominator_not_met")
		blocked = true
	}
	state := GateOpen
	if body.ThresholdResult != requirement.ExpectedResult {
		reasons = append(reasons, "preregistered_threshold_not_met")
	} else if !blocked {
		state = GateSatisfied
		reasons = append(reasons, "authoritative_threshold_satisfied")
	}
	if blocked {
		state = GateBlocked
	}
	decision, err := NewGateDecision(requirement, state, recordHash,
		slices.Clone(body.Observations), guardrailOutcomes, reasons, now)
	if err != nil {
		return GateDecision{}, false, err
	}
	replay, err := store.commitGateDecision(ctx, decision, body.AuthorSeatID, outcome.Record.Signature.KeyID)
	if err != nil {
		return GateDecision{}, false, err
	}
	return decision, replay, nil
}

func (store *Store) evaluateGuardrails(ctx context.Context, definition MetricDefinition, now time.Time) ([]RecordPointer, []string, error) {
	reasons := make([]string, 0, len(definition.Body.Guardrails))
	outcomes := make([]RecordPointer, 0, len(definition.Body.Guardrails))
	for _, reference := range definition.Body.Guardrails {
		guardrail, err := store.LoadMetric(ctx, reference, true)
		if err != nil {
			reasons = append(reasons, "guardrail_metric_not_current:"+string(reference.ID))
			continue
		}
		var outcomeID OutcomeID
		err = store.pool.QueryRow(ctx, `
			SELECT head.outcome_id
			FROM workforce_business_outcome_heads head
			JOIN workforce_business_outcomes outcome
			  ON outcome.tenant_id=head.tenant_id
			 AND outcome.organization_id=head.organization_id
			 AND outcome.outcome_id=head.outcome_id
			WHERE head.tenant_id=$1 AND head.organization_id=$2
			  AND head.initiative_id=$3 AND outcome.metric_id=$4
			  AND outcome.metric_version=$5 AND outcome.definition_hash=$6
			ORDER BY head.updated_at DESC,head.outcome_id
			LIMIT 1
		`, store.tenantID, store.organizationID, definition.Body.InitiativeID,
			reference.ID, reference.Version, reference.DefinitionHash.Digest).Scan(&outcomeID)
		if errors.Is(err, pgx.ErrNoRows) {
			reasons = append(reasons, "guardrail_outcome_missing:"+string(reference.ID))
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("business outcome: load guardrail outcome: %w", err)
		}
		outcome, err := store.LoadOutcome(ctx, outcomeID, true)
		if err != nil || outcome.Record.Body.Metric != reference {
			reasons = append(reasons, "guardrail_not_satisfied:"+string(reference.ID))
			continue
		}
		recordHash, err := OutcomeRecordHash(outcome.Record)
		if err != nil {
			return nil, nil, err
		}
		outcomes = append(outcomes, RecordPointer{Kind: "outcome", ID: string(outcomeID), Hash: recordHash})
		if outcome.Record.Body.ThresholdResult != ThresholdSuccess {
			reasons = append(reasons, "guardrail_not_satisfied:"+string(reference.ID))
			continue
		}
		contaminated, err := store.isContaminated(ctx, "outcome", string(outcomeID), recordHash.Digest)
		if err != nil {
			return nil, nil, err
		}
		if contaminated {
			reasons = append(reasons, "guardrail_contaminated:"+string(reference.ID))
			continue
		}
		unsafe := false
		for _, binding := range outcome.Record.Body.Observations {
			observation, loadErr := store.LoadObservation(ctx, binding.ID)
			if loadErr != nil || observation.ContentHash != binding.Hash || observation.GateSafe(guardrail, now) != nil {
				unsafe = true
				break
			}
		}
		if unsafe {
			reasons = append(reasons, "guardrail_evidence_not_current:"+string(reference.ID))
		}
	}
	return outcomes, reasons, nil
}

func (store *Store) commitGateDecision(ctx context.Context, value GateDecision, authorSeatID contracts.SeatID, keyID string) (bool, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.gateAD(value.Requirement.ID, value.DecisionHash), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal gate decision: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin gate decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_business_gate_decisions (
			tenant_id,organization_id,gate_id,decision_hash,initiative_id,purpose,
			outcome_id,outcome_hash,metric_id,metric_version,definition_hash,
			outcome_kind,state,sealed_decision,evaluated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.Requirement.ID, value.DecisionHash.Digest,
		value.Requirement.InitiativeID, value.Requirement.Purpose, value.Requirement.OutcomeID,
		value.OutcomeHash.Digest, value.Requirement.Metric.ID, value.Requirement.Metric.Version,
		value.Requirement.Metric.DefinitionHash.Digest, value.Requirement.OutcomeKind,
		value.State, sealed, value.EvaluatedAt)
	if err != nil {
		return false, fmt.Errorf("business outcome: insert gate decision: %w", err)
	}
	replay := command.RowsAffected() == 0
	if replay {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_gate_decisions
				WHERE tenant_id=$1 AND organization_id=$2 AND gate_id=$3 AND decision_hash=$4
			)
		`, store.tenantID, store.organizationID, value.Requirement.ID, value.DecisionHash.Digest).Scan(&exists); err != nil || !exists {
			return false, ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_gate_heads (
			tenant_id,organization_id,gate_id,decision_hash,state,evaluated_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$6)
		ON CONFLICT (tenant_id,organization_id,gate_id) DO UPDATE SET
			decision_hash=EXCLUDED.decision_hash,state=EXCLUDED.state,
			evaluated_at=EXCLUDED.evaluated_at,updated_at=EXCLUDED.updated_at
		WHERE workforce_business_gate_heads.evaluated_at<=EXCLUDED.evaluated_at
	`, store.tenantID, store.organizationID, value.Requirement.ID,
		value.DecisionHash.Digest, value.State, value.EvaluatedAt)
	if err != nil {
		return false, fmt.Errorf("business outcome: update gate head: %w", err)
	}
	if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
		value.Requirement.InitiativeID,
		RecordPointer{Kind: "outcome", ID: string(value.Requirement.OutcomeID), Hash: value.OutcomeHash},
		RecordPointer{Kind: "gate", ID: string(value.Requirement.ID), Hash: value.DecisionHash},
		"evaluated_gate", true, authorSeatID, keyID, value.EvaluatedAt); err != nil {
		return false, err
	}
	for _, guardrail := range value.Guardrails {
		if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
			value.Requirement.InitiativeID, guardrail,
			RecordPointer{Kind: "gate", ID: string(value.Requirement.ID), Hash: value.DecisionHash},
			"guards_gate", true, authorSeatID, keyID, value.EvaluatedAt); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit gate decision: %w", err)
	}
	return replay, nil
}

func gateReason(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "stale"):
		return "stale_observation"
	case strings.Contains(message, "unreconciled"):
		return "unreconciled_observation"
	case strings.Contains(message, "proposed") || strings.Contains(message, "pending"):
		return "proposed_or_pending_observation"
	case strings.Contains(message, "incompatible"):
		return "incompatible_metric"
	case strings.Contains(message, "denominator"):
		return "missing_denominator"
	default:
		return "observation_not_gate_safe"
	}
}

func (store *Store) CommitLineageEdge(ctx context.Context, value LineageEdge) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.OrganizationID != store.organizationID || value.CreatedAt.After(now) {
		return false, ErrUnauthorized
	}
	for _, pointer := range []RecordPointer{value.Source, value.Consumer} {
		bound, err := store.recordPointerBelongsToInitiative(ctx, pointer, value.InitiativeID)
		if err != nil {
			return false, err
		}
		if !bound {
			return false, ErrNotFound
		}
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.lineageAD(value.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal lineage edge: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	publicKey, _, _, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || VerifyLineageEdge(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_business_lineage_edges (
			tenant_id,organization_id,edge_id,initiative_id,source_kind,source_id,
			source_hash,consumer_kind,consumer_id,consumer_hash,relation,material,
			author_seat_id,key_id,canonical_hash,sealed_edge,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.ID, value.InitiativeID,
		value.Source.Kind, value.Source.ID, value.Source.Hash.Digest,
		value.Consumer.Kind, value.Consumer.ID, value.Consumer.Hash.Digest,
		value.Relation, value.Material, value.AuthorSeatID, value.Signature.KeyID,
		hash.Digest, sealed, value.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("business outcome: insert lineage edge: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		err := tx.QueryRow(ctx, `
			SELECT canonical_hash
			FROM workforce_business_lineage_edges
			WHERE tenant_id=$1 AND organization_id=$2 AND edge_id=$3
		`, store.tenantID, store.organizationID, value.ID).Scan(&existingHash)
		if err != nil || existingHash != hash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func insertSystemLineageTx(ctx context.Context, tx pgx.Tx, tenantID string, organizationID contracts.OrganizationID, initiativeID string, source, consumer RecordPointer, relation string, material bool, authorSeatID contracts.SeatID, keyID string, createdAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_business_lineage_edges (
			tenant_id,organization_id,edge_id,initiative_id,source_kind,source_id,
			source_hash,consumer_kind,consumer_id,consumer_hash,relation,material,
			author_seat_id,key_id,canonical_hash,sealed_edge,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULL,NULL,$15)
		ON CONFLICT DO NOTHING
	`, tenantID, organizationID, stableToken("lineage", source.Kind, source.ID, source.Hash.Digest,
		consumer.Kind, consumer.ID, consumer.Hash.Digest, relation),
		initiativeID, source.Kind, source.ID, source.Hash.Digest, consumer.Kind,
		consumer.ID, consumer.Hash.Digest, relation, material, authorSeatID, keyID, createdAt)
	if err != nil {
		return fmt.Errorf("business outcome: insert system lineage: %w", err)
	}
	return nil
}

func (store *Store) isContaminated(ctx context.Context, kind, id, hash string) (bool, error) {
	var contaminated bool
	err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_business_contamination
			WHERE tenant_id=$1 AND organization_id=$2 AND affected_kind=$3
			  AND affected_id=$4 AND affected_hash=$5 AND state='open' AND material
		)
	`, store.tenantID, store.organizationID, kind, id, hash).Scan(&contaminated)
	if err != nil {
		return false, fmt.Errorf("business outcome: inspect contamination: %w", err)
	}
	return contaminated, nil
}

func optionalOutcomeID(value *OutcomeID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func (store *Store) outcomeAD(id OutcomeID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.outcome",
		Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func (store *Store) gateAD(id GateID, hash contracts.ContentHash) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.gate",
		Stream: strings.Join([]string{string(store.organizationID), string(id), hash.Digest}, "/"), Schema: GateSchemaVersion}
}

func (store *Store) lineageAD(id LineageEdgeID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.lineage",
		Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
