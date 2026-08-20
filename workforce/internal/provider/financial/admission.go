package financial

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
)

func (store *Store) claimDispatch(
	ctx context.Context,
	authorized authorizedOperation,
	operation effect.Operation,
) (attemptClaim, error) {
	now, err := store.currentTime()
	if err != nil {
		return attemptClaim{}, err
	}
	reservationID, err := randomID("finres-")
	if err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: create reservation identity: %w", err)
	}
	attemptID, err := randomID("finatt-")
	if err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: create attempt identity: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: begin capital reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	connection := authorized.connection.connection
	request := authorized.envelope.Request
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(operation.OrganizationID, "capital")); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: lock organization capital: %w", err)
	}
	if err := store.recheckCurrentAuthority(ctx, tx, authorized, now); err != nil {
		return attemptClaim{}, err
	}
	var priorHash, priorProposal, priorIntent, priorReservation, priorState string
	err = tx.QueryRow(ctx, `
		SELECT identity.request_hash,identity.proposal_id,identity.intent_id,
		       reservation.reservation_id,reservation.state
		FROM workforce_financial_effect_identities identity
		JOIN workforce_financial_reservations reservation
		  ON reservation.tenant_id=identity.tenant_id
		 AND reservation.organization_id=identity.organization_id
		 AND reservation.connection_id=identity.connection_id
		 AND reservation.connection_version=identity.connection_version
		 AND reservation.operation=identity.operation
		 AND reservation.idempotency_key=identity.idempotency_key
		WHERE identity.tenant_id=$1 AND identity.organization_id=$2
		  AND identity.connection_id=$3 AND identity.connection_version=$4
		  AND identity.operation=$5 AND identity.idempotency_key=$6
		FOR UPDATE OF reservation
	`, store.tenantID, operation.OrganizationID, connection.ID, connection.Version,
		operation.Name, operation.IdempotencyKey).Scan(
		&priorHash, &priorProposal, &priorIntent, &priorReservation, &priorState,
	)
	if err == nil {
		if priorHash != authorized.requestHash.Digest || priorProposal != operation.ProposalID ||
			priorIntent != string(operation.IntentID) {
			return attemptClaim{}, ErrConflict
		}
		return attemptClaim{}, ErrAmbiguous
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return attemptClaim{}, fmt.Errorf("financial adapter: inspect durable identity: %w", err)
	}
	if err := store.admitCircuit(ctx, tx, connection, authorized.policy, now); err != nil {
		return attemptClaim{}, err
	}
	if request.Action.Mutates() {
		if err := store.requireUnfrozen(ctx, tx, operation, request); err != nil {
			return attemptClaim{}, err
		}
	}
	var concurrent uint16
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_financial_attempts
		WHERE tenant_id=$1 AND organization_id=$2 AND state='in_flight' AND expires_at > $3
	`, store.tenantID, operation.OrganizationID, now).Scan(&concurrent); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: measure concurrent effects: %w", err)
	}
	if concurrent >= connection.Capital.MaxConcurrent {
		return attemptClaim{}, ErrCapacity
	}
	if request.Action.Mutates() {
		if err := store.enforceCapitalLimits(ctx, tx, authorized, now); err != nil {
			return attemptClaim{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_effect_identities (
			tenant_id,organization_id,connection_id,connection_version,operation,
			idempotency_key,request_hash,proposal_id,intent_id,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.tenantID, operation.OrganizationID, connection.ID, connection.Version,
		operation.Name, operation.IdempotencyKey, authorized.requestHash.Digest,
		operation.ProposalID, operation.IntentID, now); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: persist financial identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_reservations (
			tenant_id,organization_id,reservation_id,connection_id,connection_version,
			operation,idempotency_key,request_hash,proposal_id,intent_id,initiative_id,
			asset,venue,rail,counterparty,destination_hash,notional_microunits,
			exposure_increase_microunits,maximum_loss_microunits,
			fee_ceiling_microunits,state,created_at,updated_at,reconciled_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		          $18,$19,$20,'reserved',$21,$21,NULL)
	`, store.tenantID, operation.OrganizationID, reservationID, connection.ID,
		connection.Version, operation.Name, operation.IdempotencyKey,
		authorized.requestHash.Digest, operation.ProposalID, operation.IntentID,
		request.InitiativeID, request.Amount.Asset, request.Venue, request.Rail,
		request.Counterparty, request.DestinationHash.Digest, request.NotionalMicrounits,
		request.ExposureIncreaseMicrounits, request.MaximumLossMicrounits,
		request.FeeCeilingMicrounits, now); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: persist capital reservation: %w", err)
	}
	if authorized.reserved {
		if err := store.consumeFounderReservation(ctx, tx, authorized, now); err != nil {
			return attemptClaim{}, err
		}
	}
	expiresAt := request.ExpiresAt
	if expiresAt.After(now.Add(10 * time.Minute)) {
		expiresAt = now.Add(10 * time.Minute)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_attempts (
			tenant_id,organization_id,attempt_id,reservation_id,connection_id,
			connection_version,operation,idempotency_key,attempt_kind,state,
			started_at,expires_at,finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'dispatch','in_flight',$9,$10,NULL)
	`, store.tenantID, operation.OrganizationID, attemptID, reservationID, connection.ID,
		connection.Version, operation.Name, operation.IdempotencyKey, now, expiresAt); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: persist dispatch attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: commit capital reservation: %w", err)
	}
	return attemptClaim{
		id: attemptID, reservationID: reservationID, organizationID: operation.OrganizationID,
		connectionID: connection.ID, version: connection.Version, operation: operation.Name,
		idempotencyKey: operation.IdempotencyKey, requestHash: authorized.requestHash,
		kind: attemptDispatch, policy: connection.Capital,
	}, nil
}

func (store *Store) claimProbe(
	ctx context.Context,
	authorized authorizedOperation,
	operation effect.Operation,
) (attemptClaim, error) {
	now, err := store.currentTime()
	if err != nil {
		return attemptClaim{}, err
	}
	attemptID, err := randomID("finatt-")
	if err != nil {
		return attemptClaim{}, err
	}
	connection := authorized.connection.connection
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: begin reconciliation attempt: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(operation.OrganizationID, "capital")); err != nil {
		return attemptClaim{}, err
	}
	var reservationID, requestHash, proposalID, intentID, state string
	err = tx.QueryRow(ctx, `
		SELECT reservation_id,request_hash,proposal_id,intent_id,state
		FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
		FOR UPDATE
	`, store.tenantID, operation.OrganizationID, connection.ID, connection.Version,
		operation.Name, operation.IdempotencyKey).Scan(
		&reservationID, &requestHash, &proposalID, &intentID, &state,
	)
	if errors.Is(err, pgx.ErrNoRows) || requestHash != authorized.requestHash.Digest ||
		proposalID != operation.ProposalID || intentID != string(operation.IntentID) {
		return attemptClaim{}, ErrConflict
	}
	if err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: load reconciliation reservation: %w", err)
	}
	if state == "settled" || state == "rejected" || state == "failed" || state == "reversed" {
		return attemptClaim{}, ErrConflict
	}
	if state != "reserved" && state != "ambiguous" {
		return attemptClaim{}, ErrAmbiguous
	}
	if err := store.admitCircuit(ctx, tx, connection, authorized.policy, now); err != nil {
		return attemptClaim{}, err
	}
	var attempts uint16
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_financial_attempts
		WHERE tenant_id=$1 AND organization_id=$2 AND reservation_id=$3
		  AND attempt_kind='probe'
	`, store.tenantID, operation.OrganizationID, reservationID).Scan(&attempts); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: count reconciliation attempts: %w", err)
	}
	if attempts >= connection.Capital.MaxReconciliationAttempts {
		_ = store.recordIncidentTx(ctx, tx, operation.OrganizationID, reservationID,
			connection.ID, connection.Version, operation.Name, operation.IdempotencyKey,
			"reconciliation_exhausted", "financial_reconciliation_budget_exhausted", now)
		return attemptClaim{}, ErrCapacity
	}
	expiresAt := now.Add(10 * time.Minute)
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_attempts (
			tenant_id,organization_id,attempt_id,reservation_id,connection_id,
			connection_version,operation,idempotency_key,attempt_kind,state,
			started_at,expires_at,finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'probe','in_flight',$9,$10,NULL)
	`, store.tenantID, operation.OrganizationID, attemptID, reservationID,
		connection.ID, connection.Version, operation.Name, operation.IdempotencyKey,
		now, expiresAt); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: persist reconciliation attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return attemptClaim{}, fmt.Errorf("financial adapter: commit reconciliation attempt: %w", err)
	}
	return attemptClaim{
		id: attemptID, reservationID: reservationID, organizationID: operation.OrganizationID,
		connectionID: connection.ID, version: connection.Version, operation: operation.Name,
		idempotencyKey: operation.IdempotencyKey, requestHash: authorized.requestHash,
		kind: attemptProbe, policy: connection.Capital,
	}, nil
}

func (store *Store) recheckCurrentAuthority(
	ctx context.Context,
	tx pgx.Tx,
	authorized authorizedOperation,
	now time.Time,
) error {
	connection := authorized.connection.connection
	var version uint64
	var hash, state string
	var effectiveAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT version,canonical_hash,state,effective_at,expires_at
		FROM workforce_financial_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 FOR SHARE
	`, store.tenantID, connection.OrganizationID, connection.ID).Scan(
		&version, &hash, &state, &effectiveAt, &expiresAt,
	); err != nil || version != connection.Version || hash != authorized.connection.hash.Digest ||
		state != "active" || effectiveAt.After(now) || !expiresAt.After(now) {
		return fmt.Errorf("%w: financial authority changed before reservation", ErrDenied)
	}
	var valuationID, valuationHash, riskID, riskHash string
	var valuationVersion, riskVersion uint64
	if err := tx.QueryRow(ctx, `
		SELECT valuation_id,version,canonical_hash FROM workforce_financial_valuation_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		FOR SHARE
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version).Scan(
		&valuationID, &valuationVersion, &valuationHash,
	); err != nil || valuationID != authorized.valuation.valuation.ID ||
		valuationVersion != authorized.valuation.valuation.Version || valuationHash != authorized.valuation.hash.Digest {
		return ErrStaleValuation
	}
	if err := tx.QueryRow(ctx, `
		SELECT snapshot_id,version,canonical_hash FROM workforce_financial_risk_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		FOR SHARE
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version).Scan(
		&riskID, &riskVersion, &riskHash,
	); err != nil || riskID != authorized.risk.id || riskVersion != authorized.risk.version ||
		riskHash != authorized.risk.hash.Digest {
		return ErrStaleRisk
	}
	return nil
}

func (store *Store) requireUnfrozen(
	ctx context.Context,
	tx pgx.Tx,
	operation effect.Operation,
	request Request,
) error {
	var count uint64
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_financial_scope_freezes
		WHERE tenant_id=$1 AND organization_id=$2 AND state='open' AND (
			(scope_kind='organization' AND scope_key=$3) OR
			(scope_kind='asset' AND scope_key=$4) OR
			(scope_kind='venue' AND scope_key=$5) OR
			(scope_kind='counterparty' AND scope_key=$6) OR
			(scope_kind='destination' AND scope_key=$7)
		)
	`, store.tenantID, operation.OrganizationID, operation.OrganizationID,
		request.Amount.Asset, request.Venue+"/"+request.Rail,
		request.Counterparty, request.DestinationHash.Digest).Scan(&count)
	if err != nil {
		return fmt.Errorf("financial adapter: inspect ambiguity freeze: %w", err)
	}
	if count != 0 {
		return ErrFrozen
	}
	return nil
}

func (store *Store) enforceCapitalLimits(
	ctx context.Context,
	tx pgx.Tx,
	authorized authorizedOperation,
	now time.Time,
) error {
	request := authorized.envelope.Request
	capital := authorized.connection.connection.Capital
	organizationID := authorized.connection.connection.OrganizationID
	cost, ok := add(request.NotionalMicrounits, request.FeeCeilingMicrounits)
	if !ok || cost > capital.PerEffectMicrounits || request.FeeCeilingMicrounits > capital.MaxFeeMicrounits {
		return ErrLimit
	}
	dailyStart := now.Truncate(24 * time.Hour)
	daily, err := store.sumReservations(ctx, tx, organizationID, dailyStart, ScopeKind(""), "", false)
	if err != nil {
		return err
	}
	rolling, err := store.sumReservations(ctx, tx, organizationID, now.Add(-capital.RollingWindow), ScopeKind(""), "", false)
	if err != nil {
		return err
	}
	velocity, err := store.countReservations(ctx, tx, organizationID, now.Add(-capital.RollingWindow), ScopeKind(""), "")
	if err != nil {
		return err
	}
	if exceeds(daily, cost, capital.DailyMicrounits) ||
		exceeds(rolling, cost, capital.RollingMicrounits) || velocity >= uint64(capital.MaxVelocityCount) {
		return ErrLimit
	}
	pendingCapital, err := store.sumReservations(ctx, tx, organizationID, authorized.risk.observedAt, ScopeKind(""), "", true)
	if err != nil {
		return err
	}
	pendingLoss, err := store.sumRisk(ctx, tx, organizationID, authorized.risk.observedAt, "maximum_loss_microunits")
	if err != nil {
		return err
	}
	pendingExposure, err := store.sumRisk(ctx, tx, organizationID, authorized.risk.observedAt, "exposure_increase_microunits")
	if err != nil {
		return err
	}
	risk := authorized.risk.state
	if risk.TotalCapitalMicrounits == 0 || risk.TotalCapitalMicrounits > capital.AggregateCapitalMicrounits ||
		exceeds(risk.GrossExposureMicrounits, pendingExposure, capital.MaxGrossExposureMicrounits) ||
		exceeds(risk.GrossExposureMicrounits+pendingExposure, request.ExposureIncreaseMicrounits,
			capital.MaxGrossExposureMicrounits) ||
		exceeds(risk.DrawdownMicrounits, pendingLoss, capital.MaxDrawdownMicrounits) ||
		exceeds(risk.DrawdownMicrounits+pendingLoss, request.MaximumLossMicrounits,
			capital.MaxDrawdownMicrounits) || exceeds(pendingCapital, cost, capital.AggregateCapitalMicrounits) {
		return ErrLimit
	}
	liquidityImpact := request.FeeCeilingMicrounits
	if authorized.policy.CapitalDirection == DirectionOutflow {
		liquidityImpact = cost
	} else if authorized.policy.RiskClass == RiskExposure {
		liquidityImpact, ok = add(request.MaximumLossMicrounits, request.FeeCeilingMicrounits)
		if !ok {
			return ErrLimit
		}
	}
	committedLiquidity, ok := add(pendingLoss, liquidityImpact)
	if !ok || risk.AvailableLiquidityMicrounits < committedLiquidity ||
		risk.AvailableLiquidityMicrounits-committedLiquidity < capital.MinLiquidityMicrounits ||
		risk.RunwayMicrounits < committedLiquidity ||
		risk.RunwayMicrounits-committedLiquidity < capital.MinRunwayMicrounits {
		return ErrLimit
	}
	for _, scoped := range []struct {
		kind ScopeKind
		key  string
	}{
		{ScopeAsset, request.Amount.Asset},
		{ScopeVenue, request.Venue + "/" + request.Rail},
		{ScopeCounterparty, request.Counterparty},
		{ScopeInitiative, request.InitiativeID},
	} {
		limit, found := capital.Limit(scoped.kind, scoped.key)
		if !found {
			return ErrLimit
		}
		used, err := store.sumReservations(ctx, tx, organizationID, now.Add(-capital.RollingWindow), scoped.kind, scoped.key, false)
		if err != nil {
			return err
		}
		count, err := store.countReservations(ctx, tx, organizationID, now.Add(-capital.RollingWindow), scoped.kind, scoped.key)
		if err != nil {
			return err
		}
		pendingScoped, err := store.sumScopedExposure(ctx, tx, organizationID, authorized.risk.observedAt, scoped.kind, scoped.key)
		if err != nil {
			return err
		}
		current := risk.Exposure(scoped.kind, scoped.key)
		if exceeds(used, cost, limit.RollingMicrounits) || count >= uint64(limit.MaxVelocityCount) ||
			exceeds(current, pendingScoped, limit.MaxExposureMicrounits) ||
			exceeds(current+pendingScoped, request.ExposureIncreaseMicrounits, limit.MaxExposureMicrounits) {
			return ErrLimit
		}
		concentrated, ok := add(current+pendingScoped, request.ExposureIncreaseMicrounits)
		if !ok || concentrationExceeds(concentrated, risk.TotalCapitalMicrounits, capital.MaxConcentrationBPS) {
			return ErrLimit
		}
	}
	return nil
}

func (store *Store) sumReservations(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	since time.Time,
	kind ScopeKind,
	key string,
	openOnly bool,
) (uint64, error) {
	states := "state IN ('reserved','ambiguous','settled')"
	if openOnly {
		states = "state IN ('reserved','ambiguous')"
	}
	query := `SELECT COALESCE(SUM(notional_microunits + fee_ceiling_microunits),0)
		FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND created_at >= $3 AND ` + states + ` AND (
			$4='' OR $4='asset' AND asset=$5 OR $4='venue' AND venue || '/' || rail=$5 OR
			$4='counterparty' AND counterparty=$5 OR $4='initiative' AND initiative_id=$5
		)`
	var total uint64
	err := tx.QueryRow(ctx, query, store.tenantID, organizationID, since, kind, key).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("financial adapter: sum scoped capital: %w", err)
	}
	return total, nil
}

func (store *Store) countReservations(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	since time.Time,
	kind ScopeKind,
	key string,
) (uint64, error) {
	query := `SELECT COUNT(*) FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND created_at >= $3
		  AND state IN ('reserved','ambiguous','settled') AND (
			$4='' OR $4='asset' AND asset=$5 OR $4='venue' AND venue || '/' || rail=$5 OR
			$4='counterparty' AND counterparty=$5 OR $4='initiative' AND initiative_id=$5
		)`
	var count uint64
	err := tx.QueryRow(ctx, query, store.tenantID, organizationID, since, kind, key).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("financial adapter: count scoped velocity: %w", err)
	}
	return count, nil
}

func (store *Store) sumRisk(ctx context.Context, tx pgx.Tx, organizationID contracts.OrganizationID, since time.Time, column string) (uint64, error) {
	if column != "maximum_loss_microunits" && column != "exposure_increase_microunits" {
		return 0, fmt.Errorf("financial adapter: invalid risk column")
	}
	query := `SELECT COALESCE(SUM(` + column + `),0) FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND created_at >= $3 AND state IN ('reserved','ambiguous')`
	var total uint64
	if err := tx.QueryRow(ctx, query, store.tenantID, organizationID, since).Scan(&total); err != nil {
		return 0, fmt.Errorf("financial adapter: sum pending risk: %w", err)
	}
	return total, nil
}

func (store *Store) sumScopedExposure(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	since time.Time,
	kind ScopeKind,
	key string,
) (uint64, error) {
	query := `SELECT COALESCE(SUM(exposure_increase_microunits),0)
		FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND created_at >= $3
		  AND state IN ('reserved','ambiguous') AND (
			$4='asset' AND asset=$5 OR $4='venue' AND venue || '/' || rail=$5 OR
			$4='counterparty' AND counterparty=$5 OR $4='initiative' AND initiative_id=$5
		)`
	var total uint64
	if err := tx.QueryRow(ctx, query, store.tenantID, organizationID, since, kind, key).Scan(&total); err != nil {
		return 0, fmt.Errorf("financial adapter: sum scoped exposure: %w", err)
	}
	return total, nil
}

func exceeds(current, addition, ceiling uint64) bool {
	return current > ceiling || addition > ceiling-current
}

func (store *Store) admitCircuit(
	ctx context.Context,
	tx pgx.Tx,
	connection Connection,
	policy OperationPolicy,
	now time.Time,
) error {
	var state string
	var failures uint16
	var window time.Time
	var retryAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT state,failure_count,window_started_at,retry_at
		FROM workforce_financial_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, policy.Name).Scan(&state, &failures, &window, &retryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_financial_operation_circuits (
				tenant_id,organization_id,connection_id,connection_version,operation,
				state,failure_count,window_started_at,retry_at,updated_at,version
			) VALUES ($1,$2,$3,$4,$5,'closed',0,$6,NULL,$6,1)
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, policy.Name, now)
		return err
	}
	if err != nil {
		return fmt.Errorf("financial adapter: load operation circuit: %w", err)
	}
	if now.Sub(window) >= connection.Capital.CircuitWindow && state == "closed" {
		_, err := tx.Exec(ctx, `
			UPDATE workforce_financial_operation_circuits
			SET failure_count=0,window_started_at=$1,updated_at=$1,version=version+1
			WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
			  AND connection_version=$5 AND operation=$6
		`, now, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, policy.Name)
		return err
	}
	if state == "open" {
		if retryAt == nil || retryAt.After(now) {
			return ErrCircuitOpen
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_financial_operation_circuits
			SET state='half_open',retry_at=NULL,updated_at=$1,version=version+1
			WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
			  AND connection_version=$5 AND operation=$6
		`, now, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, policy.Name); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) consumeFounderReservation(
	ctx context.Context,
	tx pgx.Tx,
	authorized authorizedOperation,
	now time.Time,
) error {
	reservation := authorized.envelope.Founder
	if reservation == nil {
		return ErrReserved
	}
	canonical, err := contracts.EncodeCanonical(reservation)
	if err != nil {
		return err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.founderReservationAD(
		reservation.OrganizationID, reservation.ID,
	), canonical)
	if err != nil {
		return fmt.Errorf("financial adapter: seal founder reservation: %w", err)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_founder_reservation_uses (
			tenant_id,organization_id,founder_reservation_id,connection_id,
			connection_version,request_hash,proposal_id,approval_id,canonical_hash,
			sealed_record,consumed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING
	`, store.tenantID, reservation.OrganizationID, reservation.ID,
		reservation.ConnectionID, reservation.ConnectionVersion,
		reservation.RequestHash.Digest, reservation.ProposalID, reservation.ApprovalID,
		hash, sealed, now)
	if err != nil {
		return fmt.Errorf("financial adapter: consume founder reservation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrReserved
	}
	return nil
}
