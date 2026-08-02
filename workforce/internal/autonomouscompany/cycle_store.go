package autonomouscompany

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/vault"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/learning"
)

type claimedNextCycle struct {
	value          learning.NextCycle
	conclusionHash contracts.ContentHash
}

func (store *Store) ClaimDueNextCycles(
	ctx context.Context,
	limit int,
) ([]NextCycleSnapshot, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("autonomous company: next-cycle claim limit is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `
		SELECT cycle.conclusion_id,cycle.hypothesis_id,cycle.initiative_id,
		       cycle.next_action,cycle.portfolio_feedback_id,cycle.due_at,
		       record.canonical_hash
		FROM workforce_learning_next_cycles cycle
		JOIN workforce_learning_records record
		  ON record.tenant_id=cycle.tenant_id
		 AND record.organization_id=cycle.organization_id
		 AND record.record_id=cycle.conclusion_id
		 AND record.record_kind='conclusion'
		WHERE cycle.tenant_id=$1 AND cycle.organization_id=$2
		  AND cycle.state='planned' AND cycle.due_at<=$3
		ORDER BY cycle.due_at,cycle.conclusion_id
		LIMIT $4 FOR UPDATE OF cycle SKIP LOCKED
	`, store.tenantID, store.organization, now, limit)
	if err != nil {
		return nil, err
	}
	var claimed []claimedNextCycle
	for rows.Next() {
		var value claimedNextCycle
		value.conclusionHash.Algorithm = "sha256"
		if err := rows.Scan(
			&value.value.ConclusionID,
			&value.value.HypothesisID,
			&value.value.InitiativeID,
			&value.value.Action,
			&value.value.PortfolioFeedbackID,
			&value.value.DueAt,
			&value.conclusionHash.Digest,
		); err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	result := make([]NextCycleSnapshot, 0, len(claimed))
	for _, claimedCycle := range claimed {
		plan := newNextCyclePlan(
			store.organization,
			claimedCycle.value,
			claimedCycle.conclusionHash,
			now,
		)
		if err := signNextCyclePlan(&plan, store.keyID, store.privateKey); err != nil {
			return nil, err
		}
		snapshot, err := store.insertNextCyclePlanTx(ctx, tx, plan, now)
		if err != nil {
			return nil, err
		}
		command, err := tx.Exec(ctx, `
			UPDATE workforce_learning_next_cycles
			SET state='claimed',claimed_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND conclusion_id=$4
			  AND state='planned'
		`, now, store.tenantID, store.organization, plan.ConclusionID)
		if err != nil {
			return nil, err
		}
		if command.RowsAffected() != 1 {
			return nil, ErrConflict
		}
		result = append(result, snapshot)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (store *Store) insertNextCyclePlanTx(
	ctx context.Context,
	tx pgx.Tx,
	plan NextCyclePlan,
	createdAt time.Time,
) (NextCycleSnapshot, error) {
	canonical, err := contracts.EncodeCanonical(&plan)
	if err != nil {
		return NextCycleSnapshot{}, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.nextCyclePlanAD(plan.ID), canonical)
	if err != nil {
		return NextCycleSnapshot{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_autonomous_company_next_cycle_plans (
			tenant_id,organization_id,plan_id,conclusion_id,conclusion_hash,
			hypothesis_id,initiative_id,selected_action,portfolio_feedback_id,
			due_at,claimed_at,canonical_hash,sealed_plan,key_id,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, store.tenantID, store.organization, plan.ID, plan.ConclusionID,
		plan.ConclusionHash.Digest, plan.HypothesisID, plan.InitiativeID,
		plan.SelectedAction, plan.PortfolioFeedbackID, plan.DueAt, plan.ClaimedAt,
		canonicalHash.Digest, sealed, store.keyID, createdAt)
	if err != nil {
		return NextCycleSnapshot{}, err
	}
	for _, operation := range plan.Operations {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_next_cycle_operations (
				tenant_id,organization_id,plan_id,sequence,operation_kind,
				from_state,to_state,decision,runtime_action
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organization, plan.ID, operation.Sequence,
			operation.Kind, nullableString(string(operation.FromState)),
			nullableString(string(operation.ToState)), nullableString(string(operation.Decision)),
			nullableString(string(operation.RuntimeAction))); err != nil {
			return NextCycleSnapshot{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_autonomous_company_next_cycle_heads (
			tenant_id,organization_id,plan_id,conclusion_id,initiative_id,
			state,last_event_sequence,updated_at
		) VALUES ($1,$2,$3,$4,$5,'planned',0,$6)
	`, store.tenantID, store.organization, plan.ID, plan.ConclusionID,
		plan.InitiativeID, createdAt); err != nil {
		return NextCycleSnapshot{}, err
	}
	return NextCycleSnapshot{
		Plan: plan, CanonicalHash: canonicalHash, State: NextCyclePlanned,
	}, nil
}

func (store *Store) LoadNextCyclePlan(
	ctx context.Context,
	planID string,
) (NextCycleSnapshot, error) {
	if token(planID) != nil {
		return NextCycleSnapshot{}, ErrNotFound
	}
	var expected string
	var keyID string
	var sealed []byte
	var state NextCycleState
	var lastSequence uint64
	var lastEventID sql.NullString
	err := store.pool.QueryRow(ctx, `
		SELECT plan.canonical_hash,plan.key_id,plan.sealed_plan,head.state,
		       head.last_event_sequence,head.last_event_id
		FROM workforce_autonomous_company_next_cycle_plans plan
		JOIN workforce_autonomous_company_next_cycle_heads head
		  ON head.tenant_id=plan.tenant_id
		 AND head.organization_id=plan.organization_id
		 AND head.plan_id=plan.plan_id
		WHERE plan.tenant_id=$1 AND plan.organization_id=$2 AND plan.plan_id=$3
	`, store.tenantID, store.organization, planID).Scan(
		&expected, &keyID, &sealed, &state, &lastSequence, &lastEventID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return NextCycleSnapshot{}, ErrNotFound
	}
	if err != nil {
		return NextCycleSnapshot{}, err
	}
	opened, err := store.vault.OpenRecord(store.nextCyclePlanAD(planID), sealed)
	if err != nil || digest(opened).Digest != expected {
		return NextCycleSnapshot{}, ErrIntegrity
	}
	plan, err := contracts.DecodeCanonical[NextCyclePlan, *NextCyclePlan](opened)
	if err != nil || plan.ID != planID || plan.OrganizationID != store.organization ||
		keyID != store.keyID || verifyNextCyclePlan(plan, store.keyID, store.publicKey) != nil ||
		!state.Valid() {
		return NextCycleSnapshot{}, ErrIntegrity
	}
	snapshot := NextCycleSnapshot{
		Plan:          plan,
		CanonicalHash: contracts.ContentHash{Algorithm: "sha256", Digest: expected},
		State:         state,
	}
	if lastSequence == 0 {
		if lastEventID.Valid || state != NextCyclePlanned {
			return NextCycleSnapshot{}, ErrIntegrity
		}
		return snapshot, nil
	}
	if !lastEventID.Valid {
		return NextCycleSnapshot{}, ErrIntegrity
	}
	event, err := store.LoadNextCycleEvent(ctx, lastEventID.String)
	if err != nil || event.Sequence != lastSequence || event.PlanID != planID ||
		event.State != state {
		return NextCycleSnapshot{}, ErrIntegrity
	}
	snapshot.LastEvent = &event
	return snapshot, nil
}

func (store *Store) LoadNextCycleEvent(
	ctx context.Context,
	eventID string,
) (NextCycleEvent, error) {
	if token(eventID) != nil {
		return NextCycleEvent{}, ErrNotFound
	}
	var planID string
	var sequence uint64
	var expected string
	var keyID string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT plan_id,sequence,canonical_hash,key_id,sealed_event
		FROM workforce_autonomous_company_next_cycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
	`, store.tenantID, store.organization, eventID).Scan(
		&planID, &sequence, &expected, &keyID, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return NextCycleEvent{}, ErrNotFound
	}
	if err != nil {
		return NextCycleEvent{}, err
	}
	opened, err := store.vault.OpenRecord(store.nextCycleEventAD(planID, sequence), sealed)
	if err != nil || digest(opened).Digest != expected {
		return NextCycleEvent{}, ErrIntegrity
	}
	event, err := contracts.DecodeCanonical[NextCycleEvent, *NextCycleEvent](opened)
	if err != nil || event.ID != eventID || event.PlanID != planID ||
		event.Sequence != sequence || event.OrganizationID != store.organization ||
		keyID != store.keyID || verifyNextCycleEvent(event, store.keyID, store.publicKey) != nil {
		return NextCycleEvent{}, ErrIntegrity
	}
	return event, nil
}

func (store *Store) RecordNextCycleUpdate(
	ctx context.Context,
	update NextCycleUpdate,
) (NextCycleEvent, bool, error) {
	if update.Validate() != nil {
		return NextCycleEvent{}, false, fmt.Errorf("autonomous company: next-cycle update is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	if update.OccurredAt.After(now) {
		return NextCycleEvent{}, false, fmt.Errorf("autonomous company: next-cycle update is in the future")
	}
	snapshot, err := store.LoadNextCyclePlan(ctx, update.PlanID)
	if err != nil || snapshot.CanonicalHash != update.PlanHash {
		return NextCycleEvent{}, false, ErrConflict
	}
	for _, binding := range update.Evidence {
		if binding.OrganizationID != store.organization ||
			binding.InitiativeID != snapshot.Plan.InitiativeID {
			return NextCycleEvent{}, false, ErrUnauthorized
		}
		if update.State != NextCycleUncertain && !binding.currentAt(update.OccurredAt) {
			return NextCycleEvent{}, false, ErrEvidence
		}
	}
	eventID, err := nextCycleEventID(update)
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+string(store.organization)+"|next-cycle|"+update.PlanID); err != nil {
		return NextCycleEvent{}, false, err
	}
	var replaySequence uint64
	err = tx.QueryRow(ctx, `
		SELECT sequence FROM workforce_autonomous_company_next_cycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
	`, store.tenantID, store.organization, eventID).Scan(&replaySequence)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return NextCycleEvent{}, false, err
		}
		event, err := store.LoadNextCycleEvent(ctx, eventID)
		if err != nil || !eventMatchesUpdate(event, update) {
			return NextCycleEvent{}, false, ErrConflict
		}
		return event, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return NextCycleEvent{}, false, err
	}
	var currentState NextCycleState
	var currentSequence uint64
	err = tx.QueryRow(ctx, `
		SELECT state,last_event_sequence
		FROM workforce_autonomous_company_next_cycle_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND plan_id=$3
		FOR UPDATE
	`, store.tenantID, store.organization, update.PlanID).Scan(
		&currentState, &currentSequence,
	)
	if err != nil || !nextCycleTransitionAllowed(currentState, update.State) {
		return NextCycleEvent{}, false, ErrConflict
	}
	for _, binding := range update.Evidence {
		if err := store.verifier.VerifyAutonomousCompanyEvidence(
			ctx, tx, binding, update.OccurredAt,
		); err != nil {
			return NextCycleEvent{}, false, fmt.Errorf("%w: %s: %v", ErrEvidence, binding.Kind, err)
		}
	}
	event := NextCycleEvent{
		SchemaVersion:  NextCycleEventSchemaVersion,
		ID:             eventID,
		Sequence:       currentSequence + 1,
		PlanID:         update.PlanID,
		PlanHash:       update.PlanHash,
		OrganizationID: store.organization,
		InitiativeID:   snapshot.Plan.InitiativeID,
		State:          update.State,
		Evidence:       append([]EvidenceBinding(nil), update.Evidence...),
		ReasonCodes:    append([]string(nil), update.ReasonCodes...),
		OccurredAt:     update.OccurredAt,
	}
	if err := signNextCycleEvent(&event, store.keyID, store.privateKey); err != nil {
		return NextCycleEvent{}, false, err
	}
	canonical, err := contracts.EncodeCanonical(&event)
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(
		store.nextCycleEventAD(event.PlanID, event.Sequence), canonical,
	)
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_autonomous_company_next_cycle_events (
			tenant_id,organization_id,event_id,plan_id,sequence,initiative_id,
			state,canonical_hash,sealed_event,key_id,occurred_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, store.tenantID, store.organization, event.ID, event.PlanID, event.Sequence,
		event.InitiativeID, event.State, canonicalHash.Digest, sealed, store.keyID,
		event.OccurredAt, now); err != nil {
		return NextCycleEvent{}, false, err
	}
	for _, binding := range event.Evidence {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_next_cycle_event_evidence (
				tenant_id,organization_id,event_id,evidence_kind,initiative_id,
				record_id,record_version,record_hash,authority,source_state,
				validity,reconciliation,contaminated,observed_at,fresh_until
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		`, store.tenantID, store.organization, event.ID, binding.Kind,
			binding.InitiativeID, binding.RecordID, binding.RecordVersion,
			binding.RecordHash.Digest, binding.Authority, binding.SourceState,
			binding.Validity, binding.Reconciliation, binding.Contaminated,
			binding.ObservedAt, binding.FreshUntil); err != nil {
			return NextCycleEvent{}, false, err
		}
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_autonomous_company_next_cycle_heads
		SET state=$1,last_event_sequence=$2,last_event_id=$3,
		    last_event_hash=$4,updated_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND plan_id=$8
		  AND state=$9 AND last_event_sequence=$10
	`, event.State, event.Sequence, event.ID, canonicalHash.Digest, now,
		store.tenantID, store.organization, event.PlanID, currentState, currentSequence)
	if err != nil {
		return NextCycleEvent{}, false, err
	}
	if command.RowsAffected() != 1 {
		return NextCycleEvent{}, false, ErrConflict
	}
	if err := store.advanceLearningCycleTx(ctx, tx, snapshot.Plan, update, now); err != nil {
		return NextCycleEvent{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NextCycleEvent{}, false, err
	}
	return event, false, nil
}

func (store *Store) advanceLearningCycleTx(
	ctx context.Context,
	tx pgx.Tx,
	plan NextCyclePlan,
	update NextCycleUpdate,
	now time.Time,
) error {
	if update.State == NextCycleBlocked &&
		len(update.Evidence) == 0 &&
		len(update.ReasonCodes) == 1 && update.ReasonCodes[0] == "founder_required" {
		return nil
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_learning_next_cycles
		SET state='dispatched',dispatched_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND conclusion_id=$4
		  AND state='claimed'
	`, now, store.tenantID, store.organization, plan.ConclusionID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var state string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM workforce_learning_next_cycles
			WHERE tenant_id=$1 AND organization_id=$2 AND conclusion_id=$3
		`, store.tenantID, store.organization, plan.ConclusionID).Scan(&state); err != nil ||
			(state != "dispatched" && state != "completed") {
			return ErrConflict
		}
	}
	if update.State != NextCyclePassed && update.State != NextCycleFailed {
		return nil
	}
	command, err = tx.Exec(ctx, `
		UPDATE workforce_learning_next_cycles
		SET state='completed',completed_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND conclusion_id=$4
		  AND state='dispatched'
	`, now, store.tenantID, store.organization, plan.ConclusionID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) ListNextCycles(
	ctx context.Context,
	states []NextCycleState,
	limit int,
) ([]NextCycleSnapshot, error) {
	if limit <= 0 || limit > 200 || len(states) == 0 || len(states) > 6 {
		return nil, fmt.Errorf("autonomous company: next-cycle list filter is invalid")
	}
	seen := make(map[NextCycleState]bool, len(states))
	stateValues := make([]string, 0, len(states))
	for _, state := range states {
		if !state.Valid() || seen[state] {
			return nil, fmt.Errorf("autonomous company: next-cycle list state is invalid")
		}
		seen[state] = true
		stateValues = append(stateValues, string(state))
	}
	rows, err := store.pool.Query(ctx, `
		SELECT plan_id
		FROM workforce_autonomous_company_next_cycle_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state=ANY($3)
		ORDER BY updated_at,plan_id LIMIT $4
	`, store.tenantID, store.organization, stateValues, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]NextCycleSnapshot, 0, len(ids))
	for _, id := range ids {
		snapshot, err := store.LoadNextCyclePlan(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func (store *Store) ListActiveNextCycles(
	ctx context.Context,
	limit int,
) ([]NextCycleSnapshot, error) {
	return store.ListNextCycles(ctx, []NextCycleState{
		NextCycleRunning, NextCycleUncertain,
	}, limit)
}

func (store *Store) NextCycleEvidenceBinding(
	ctx context.Context,
	eventID string,
	kind EvidenceKind,
	freshUntil time.Time,
) (EvidenceBinding, error) {
	if (kind != EvidenceNextCycleDispatchReceipt &&
		kind != EvidenceNextCycleCompletionReceipt) || !utc(freshUntil) {
		return EvidenceBinding{}, ErrEvidence
	}
	now, err := store.currentTime()
	if err != nil || !freshUntil.After(now) {
		return EvidenceBinding{}, ErrEvidence
	}
	event, err := store.LoadNextCycleEvent(ctx, eventID)
	if err != nil || (kind == EvidenceNextCycleDispatchReceipt &&
		(event.State != NextCycleRunning && event.State != NextCyclePassed)) ||
		(kind == EvidenceNextCycleCompletionReceipt && event.State != NextCyclePassed) ||
		!freshUntil.After(event.OccurredAt) {
		return EvidenceBinding{}, ErrEvidence
	}
	var hash string
	var keyID string
	err = store.pool.QueryRow(ctx, `
		SELECT canonical_hash,key_id
		FROM workforce_autonomous_company_next_cycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
	`, store.tenantID, store.organization, event.ID).Scan(&hash, &keyID)
	if err != nil || keyID != event.Signature.KeyID {
		return EvidenceBinding{}, ErrIntegrity
	}
	binding := EvidenceBinding{
		SchemaVersion:  EvidenceSchemaVersion,
		Kind:           kind,
		OrganizationID: store.organization,
		InitiativeID:   event.InitiativeID,
		RecordID:       event.ID,
		RecordVersion:  event.Sequence,
		RecordHash:     contracts.ContentHash{Algorithm: "sha256", Digest: hash},
		Authority:      keyID,
		SourceState:    string(event.State),
		Validity:       contracts.ValidityActive,
		Reconciliation: ReconciliationNotApplicable,
		ObservedAt:     event.OccurredAt,
		FreshUntil:     freshUntil,
	}
	if binding.Validate() != nil {
		return EvidenceBinding{}, ErrIntegrity
	}
	return binding, nil
}

func nextCycleTransitionAllowed(from, to NextCycleState) bool {
	if !from.Valid() || !to.Valid() || to == NextCyclePlanned ||
		from == NextCyclePassed || from == NextCycleFailed {
		return false
	}
	switch from {
	case NextCyclePlanned:
		return to == NextCycleRunning || to == NextCycleBlocked ||
			to == NextCyclePassed || to == NextCycleFailed || to == NextCycleUncertain
	case NextCycleRunning, NextCycleBlocked, NextCycleUncertain:
		return to == NextCycleRunning || to == NextCycleBlocked ||
			to == NextCyclePassed || to == NextCycleFailed || to == NextCycleUncertain
	default:
		return false
	}
}

func eventMatchesUpdate(event NextCycleEvent, update NextCycleUpdate) bool {
	return event.PlanID == update.PlanID && event.PlanHash == update.PlanHash &&
		event.State == update.State && event.OccurredAt.Equal(update.OccurredAt) &&
		slices.Equal(event.Evidence, update.Evidence) &&
		slices.Equal(event.ReasonCodes, update.ReasonCodes)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (store *Store) nextCyclePlanAD(planID string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.autonomous-company.next-cycle-plan",
		Stream: string(store.organization) + "/" + planID,
		Schema: NextCyclePlanSchemaVersion,
	}
}

func (store *Store) nextCycleEventAD(planID string, sequence uint64) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.autonomous-company.next-cycle-event",
		Stream: string(store.organization) + "/" + planID + "/" + strconv.FormatUint(sequence, 10),
		Schema: NextCycleEventSchemaVersion,
	}
}
