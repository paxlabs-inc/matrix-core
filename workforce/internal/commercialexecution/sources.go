package commercialexecution

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (store *Store) verifyEvidenceSources(
	ctx context.Context,
	tx pgx.Tx,
	body EvidenceBody,
	scope Scope,
	now time.Time,
) error {
	seenRoles := make(map[SourceRole]int, len(body.Sources))
	seenRecords := make(map[string]bool, len(body.Sources))
	for _, source := range body.Sources {
		identity := string(source.Kind) + "|" + source.RecordID
		if seenRecords[identity] {
			return fmt.Errorf("%w: one source record cannot satisfy multiple commercial facts", ErrIntegrity)
		}
		seenRecords[identity] = true
		seenRoles[source.Role]++
		if source.Role != RoleMetricObservation && source.Role != RoleBalancedAccounting && seenRoles[source.Role] > 1 {
			return fmt.Errorf("%w: commercial evidence role is duplicated", ErrIntegrity)
		}
		if err := store.verifySource(ctx, tx, body.InitiativeID, body.Phase, source, scope, now); err != nil {
			return err
		}
	}
	if seenRoles[RoleBalancedAccounting] != 0 {
		if err := store.verifyBalancedAccountingSet(ctx, tx, body.InitiativeID, body.Sources, scope); err != nil {
			return err
		}
	}
	if body.Disposition == StateCompleted {
		for _, source := range body.Sources {
			if !sourceCompletes(source) {
				return fmt.Errorf("%w: pending, ambiguous, failed, or untrusted source cannot complete a phase", ErrPending)
			}
		}
		if body.Phase == PhaseSale {
			if seenRoles[RoleSalesOrder] != 1 || seenRoles[RoleContract] != 1 {
				return fmt.Errorf("%w: sale requires distinct order and contract evidence", ErrIntegrity)
			}
		}
		if body.Phase == PhaseFinancialReconciliation && seenRoles[RoleBalancedAccounting] < 2 {
			return fmt.Errorf("%w: settlement requires balanced debit and credit entries", ErrIntegrity)
		}
		if body.Phase == PhaseMeasurement && seenRoles[RoleMetricObservation] == 0 {
			return fmt.Errorf("%w: measurement requires authoritative observations", ErrIntegrity)
		}
	}
	return nil
}

func sourceCompletes(source SourceRef) bool {
	switch source.Role {
	case RoleFinancialIntent:
		return source.Authority == AuthorityInternalVerified &&
			(source.State == SourcePending || source.State == SourceAmbiguous ||
				source.State == SourceCompleted || source.State == SourceReconciled)
	case RoleFinancialSettlement, RoleBalancedAccounting:
		return source.Authority == AuthorityReconciledFinancial && source.State == SourceReconciled
	case RoleBusinessGate:
		return source.Authority == AuthorityIndependentOutcome && source.State == SourceSatisfied
	case RoleCommercialOutcome:
		return source.Authority == AuthorityIndependentOutcome && source.State == SourceCompleted
	case RoleMetricDefinition, RoleMetricObservation, RoleLaunchedProduct, RoleQualifiedCustomer:
		return source.Authority == AuthorityInternalVerified &&
			(source.State == SourceCompleted || source.State == SourceReconciled)
	case RoleAcquisitionEvent, RoleCRMRecord, RoleSalesOrder, RoleContract, RoleSupportCycle:
		return (source.Authority == AuthorityProviderAuthoritative || source.Authority == AuthorityControlPlane) &&
			source.State == SourceCompleted
	default:
		return false
	}
}

func (store *Store) verifySource(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	phase Phase,
	source SourceRef,
	scope Scope,
	now time.Time,
) error {
	if source.Validate() != nil {
		return ErrIntegrity
	}
	switch source.Kind {
	case SourceProductExecution:
		return store.verifyProductExecution(ctx, tx, initiativeID, source, scope)
	case SourceCustomerScope:
		return store.verifyCustomerScope(ctx, tx, source, scope, now)
	case SourceCustomerObservation:
		return store.verifyCustomerObservation(ctx, tx, phase, source, scope)
	case SourceFinancialReservation:
		return store.verifyFinancialReservation(ctx, tx, initiativeID, phase, source, scope)
	case SourceFinancialObservation:
		return store.verifyFinancialObservation(ctx, tx, initiativeID, phase, source, scope)
	case SourceFinancialAccounting:
		return store.verifyFinancialAccounting(ctx, tx, initiativeID, source)
	case SourceBusinessMetric:
		return store.verifyBusinessMetric(ctx, tx, initiativeID, source, scope, now)
	case SourceBusinessObservation:
		return store.verifyBusinessObservation(ctx, tx, initiativeID, source, scope, now)
	case SourceBusinessOutcome:
		return store.verifyBusinessOutcome(ctx, tx, initiativeID, source, scope, now)
	case SourceBusinessGate:
		return store.verifyBusinessGate(ctx, tx, initiativeID, source, scope)
	case SourceBusinessCorrection:
		return store.verifyBusinessCorrection(ctx, tx, initiativeID, source)
	default:
		return ErrIntegrity
	}
}

func (store *Store) verifyProductExecution(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
	scope Scope,
) error {
	var storedInitiative, phase, hash string
	var version uint64
	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,phase,version,canonical_hash,updated_at
		FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&storedInitiative, &phase, &version, &hash, &updatedAt,
	); err != nil || storedInitiative != initiativeID || source.RecordID != scope.ProductExecutionID ||
		version != source.Version || hash != source.Hash.Digest || hash != scope.ProductExecutionHash.Digest ||
		phase != "launched" || source.State != SourceCompleted ||
		source.Authority != AuthorityInternalVerified || !updatedAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: product launch source is not exact", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyCustomerScope(
	ctx context.Context,
	tx pgx.Tx,
	source SourceRef,
	scope Scope,
	now time.Time,
) error {
	var version, connectionVersion uint64
	var hash, connectionID, recipientHash, state string
	var updatedAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT version,canonical_hash,connection_id,connection_version,recipient_hash,
		       state,updated_at,expires_at
		FROM workforce_customer_scope_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND customer_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&version, &hash, &connectionID, &connectionVersion, &recipientHash,
		&state, &updatedAt, &expiresAt,
	); err != nil || version != source.Version || hash != source.Hash.Digest ||
		connectionID != scope.CustomerConnectionID || connectionVersion != scope.CustomerConnectionVersion ||
		recipientHash != scope.AudienceHash.Digest || state != "active" || !expiresAt.After(now) ||
		source.State != SourceCompleted || source.Authority != AuthorityInternalVerified ||
		!updatedAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: qualified customer record is not current and exact", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyCustomerObservation(
	ctx context.Context,
	tx pgx.Tx,
	phase Phase,
	source SourceRef,
	scope Scope,
) error {
	var connectionVersion uint64
	var hash, connectionID, operation, recipientHash, state, authority, provider, externalID string
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT observation.canonical_hash,observation.connection_id,observation.connection_version,
		       observation.operation,observation.recipient_hash,observation.external_state,
		       observation.authority,connection.external_adapter_name,observation.external_id,
		       observation.provider_observed_at
		FROM workforce_customer_observations observation
		JOIN workforce_customer_connections connection
		  ON connection.tenant_id=observation.tenant_id
		 AND connection.organization_id=observation.organization_id
		 AND connection.connection_id=observation.connection_id
		 AND connection.version=observation.connection_version
		WHERE observation.tenant_id=$1 AND observation.organization_id=$2 AND observation.observation_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&hash, &connectionID, &connectionVersion, &operation, &recipientHash,
		&state, &authority, &provider, &externalID, &observedAt,
	); err != nil || hash != source.Hash.Digest || connectionID != scope.CustomerConnectionID ||
		connectionVersion != scope.CustomerConnectionVersion || operation != source.Operation ||
		!scope.OperationAllowed(phase, operation) || recipientHash != scope.AudienceHash.Digest ||
		provider != source.Provider || externalID != source.ExternalRef || !observedAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: customer observation source is not exact", ErrIntegrity)
	}
	expectedState, expectedAuthority := customerSourceProjection(state, authority)
	if source.State != expectedState || source.Authority != expectedAuthority {
		return fmt.Errorf("%w: customer source state or authority was promoted", ErrIntegrity)
	}
	return nil
}

func customerSourceProjection(state, authority string) (SourceState, SourceAuthority) {
	projectedState := SourceAmbiguous
	switch state {
	case "completed":
		projectedState = SourceCompleted
	case "pending":
		projectedState = SourcePending
	case "rejected":
		projectedState = SourceFailed
	case "reversed":
		projectedState = SourceReversed
	}
	projectedAuthority := AuthorityUntrusted
	if authority == "provider_authoritative" {
		projectedAuthority = AuthorityProviderAuthoritative
	} else if authority == "control_plane_authoritative" {
		projectedAuthority = AuthorityControlPlane
	}
	return projectedState, projectedAuthority
}

func (store *Store) verifyFinancialReservation(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	phase Phase,
	source SourceRef,
	scope Scope,
) error {
	var connectionVersion uint64
	var connectionID, operation, hash, state, accountID string
	var notional uint64
	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT reservation.connection_id,reservation.connection_version,reservation.operation,
		       reservation.request_hash,reservation.state,connection.account_id,
		       reservation.notional_microunits,reservation.updated_at
		FROM workforce_financial_reservations reservation
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=reservation.tenant_id
		 AND connection.organization_id=reservation.organization_id
		 AND connection.connection_id=reservation.connection_id
		 AND connection.version=reservation.connection_version
		WHERE reservation.tenant_id=$1 AND reservation.organization_id=$2
		  AND reservation.reservation_id=$3 AND reservation.initiative_id=$4
	`, store.tenantID, store.organizationID, source.RecordID, initiativeID).Scan(
		&connectionID, &connectionVersion, &operation, &hash, &state, &accountID, &notional, &updatedAt,
	); err != nil || connectionID != scope.FinancialConnectionID ||
		connectionVersion != scope.FinancialConnectionVersion || operation != source.Operation ||
		!scope.OperationAllowed(phase, operation) || hash != source.Hash.Digest ||
		notional > scope.MaximumTransactionMicrounits || !updatedAt.Equal(source.ObservedAt) ||
		accountID != source.AccountRef || source.Authority != AuthorityInternalVerified {
		return fmt.Errorf("%w: financial intent source is not exact", ErrIntegrity)
	}
	expected := SourcePending
	switch state {
	case "ambiguous":
		expected = SourceAmbiguous
	case "settled":
		expected = SourceCompleted
	case "failed", "rejected":
		expected = SourceFailed
	case "reversed":
		expected = SourceReversed
	}
	if source.State != expected {
		return fmt.Errorf("%w: financial reservation state was promoted", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyFinancialObservation(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	phase Phase,
	source SourceRef,
	scope Scope,
) error {
	var connectionVersion uint64
	var connectionID, operation, hash, state, authority, provider, accountID, externalID string
	var reconciled, economicTruth bool
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT observation.connection_id,observation.connection_version,observation.operation,
		       observation.canonical_hash,observation.financial_state,observation.authority,
		       connection.external_adapter_name,connection.account_id,observation.external_id,
		       observation.reconciled,observation.economic_truth,observation.provider_observed_at
		FROM workforce_financial_observations observation
		JOIN workforce_financial_reservations reservation
		  ON reservation.tenant_id=observation.tenant_id
		 AND reservation.organization_id=observation.organization_id
		 AND reservation.reservation_id=observation.reservation_id
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=observation.tenant_id
		 AND connection.organization_id=observation.organization_id
		 AND connection.connection_id=observation.connection_id
		 AND connection.version=observation.connection_version
		WHERE observation.tenant_id=$1 AND observation.organization_id=$2
		  AND observation.observation_id=$3 AND reservation.initiative_id=$4
	`, store.tenantID, store.organizationID, source.RecordID, initiativeID).Scan(
		&connectionID, &connectionVersion, &operation, &hash, &state, &authority,
		&provider, &accountID, &externalID, &reconciled, &economicTruth, &observedAt,
	); err != nil || connectionID != scope.FinancialConnectionID ||
		connectionVersion != scope.FinancialConnectionVersion || operation != source.Operation ||
		!scope.OperationAllowed(phase, operation) || hash != source.Hash.Digest ||
		provider != source.Provider || accountID != source.AccountRef || externalID != source.ExternalRef ||
		!observedAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: financial observation source is not exact", ErrIntegrity)
	}
	projectedState := SourcePending
	if state == "unknown" {
		projectedState = SourceAmbiguous
	} else if state == "rejected" || state == "failed" {
		projectedState = SourceFailed
	} else if state == "reversed" {
		projectedState = SourceReversed
	} else if reconciled && economicTruth {
		projectedState = SourceReconciled
	}
	projectedAuthority := AuthorityUntrusted
	if reconciled && economicTruth && (authority == "provider_authoritative" || authority == "control_plane_authoritative") {
		projectedAuthority = AuthorityReconciledFinancial
	} else if authority == "provider_authoritative" {
		projectedAuthority = AuthorityProviderAuthoritative
	} else if authority == "control_plane_authoritative" {
		projectedAuthority = AuthorityControlPlane
	}
	if source.State != projectedState || source.Authority != projectedAuthority {
		return fmt.Errorf("%w: financial observation state was promoted", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyFinancialAccounting(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
) error {
	var observationID, accountID, evidenceHash, currency, side string
	var microunits uint64
	var valuationTime, createdAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT observation_id,account_id,evidence_hash,currency,side,microunits,
		       valuation_time,created_at
		FROM workforce_financial_accounting_entries
		WHERE tenant_id=$1 AND organization_id=$2 AND entry_id=$3 AND initiative_id=$4
	`, store.tenantID, store.organizationID, source.RecordID, initiativeID).Scan(
		&observationID, &accountID, &evidenceHash, &currency, &side, &microunits,
		&valuationTime, &createdAt,
	); err != nil || observationID != source.RelatedID || accountID != source.AccountRef ||
		evidenceHash != source.Hash.Digest || microunits == 0 ||
		(side != "debit" && side != "credit") || currency == "" ||
		source.ValuationTime == nil || !valuationTime.Equal(*source.ValuationTime) ||
		source.State != SourceReconciled || source.Authority != AuthorityReconciledFinancial ||
		!createdAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: financial accounting entry is not exact and reconciled", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBalancedAccountingSet(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	sources []SourceRef,
	scope Scope,
) error {
	settlementID := ""
	for _, source := range sources {
		if source.Role == RoleFinancialSettlement {
			settlementID = source.RecordID
			break
		}
	}
	if settlementID == "" {
		return fmt.Errorf("%w: accounting lacks its settlement observation", ErrIntegrity)
	}
	var debits, credits uint64
	count := 0
	for _, source := range sources {
		if source.Role != RoleBalancedAccounting {
			continue
		}
		var side, currency string
		var microunits uint64
		if err := tx.QueryRow(ctx, `
			SELECT side,currency,microunits FROM workforce_financial_accounting_entries
			WHERE tenant_id=$1 AND organization_id=$2 AND entry_id=$3
			  AND initiative_id=$4 AND observation_id=$5
		`, store.tenantID, store.organizationID, source.RecordID, initiativeID, settlementID).Scan(
			&side, &currency, &microunits,
		); err != nil || currency != scope.Currency {
			return fmt.Errorf("%w: accounting currency or settlement lineage disagrees", ErrIntegrity)
		}
		if side == "debit" {
			if ^uint64(0)-debits < microunits {
				return ErrIntegrity
			}
			debits += microunits
		} else {
			if ^uint64(0)-credits < microunits {
				return ErrIntegrity
			}
			credits += microunits
		}
		count++
	}
	if count < 2 || debits == 0 || debits != credits {
		return fmt.Errorf("%w: accounting entries are not balanced", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBusinessMetric(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
	scope Scope,
	now time.Time,
) error {
	var hash, storedInitiative string
	var version uint64
	var registeredAt, effectiveAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,version,definition_hash,registered_at,effective_at,expires_at
		FROM workforce_business_metric_definitions
		WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, source.RecordID, source.Version).Scan(
		&storedInitiative, &version, &hash, &registeredAt, &effectiveAt, &expiresAt,
	); err != nil || storedInitiative != initiativeID || source.RecordID != string(scope.Gate.Metric.ID) ||
		version != scope.Gate.Metric.Version || hash != source.Hash.Digest ||
		hash != scope.Gate.Metric.DefinitionHash.Digest || now.Before(effectiveAt) || !expiresAt.After(now) ||
		source.State != SourceCompleted || source.Authority != AuthorityInternalVerified ||
		!registeredAt.Equal(source.ObservedAt) {
		return fmt.Errorf("%w: metric definition is not current and exact", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBusinessObservation(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
	scope Scope,
	now time.Time,
) error {
	var storedInitiative, metricID, hash, status string
	var metricVersion uint64
	var observedAt, freshUntil time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,metric_id,metric_version,content_hash,status,observed_at,fresh_until
		FROM workforce_business_observations
		WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&storedInitiative, &metricID, &metricVersion, &hash, &status, &observedAt, &freshUntil,
	); err != nil || storedInitiative != initiativeID || metricID != string(scope.Gate.Metric.ID) ||
		metricVersion != scope.Gate.Metric.Version || hash != source.Hash.Digest || !freshUntil.After(now) ||
		!observedAt.Equal(source.ObservedAt) || source.Authority != AuthorityInternalVerified {
		return fmt.Errorf("%w: business observation is not current and exact", ErrIntegrity)
	}
	expected := SourceCompleted
	if status == "reconciled" {
		expected = SourceReconciled
	} else if status == "pending" || status == "proposed" {
		expected = SourcePending
	} else if status == "contested" {
		expected = SourceAmbiguous
	} else if status == "retracted" {
		expected = SourceReversed
	}
	if source.State != expected {
		return fmt.Errorf("%w: business observation status was promoted", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBusinessOutcome(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
	scope Scope,
	now time.Time,
) error {
	var storedInitiative, metricID, hash, kind string
	var version uint64
	var verifiedAt, freshUntil time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,metric_id,version,record_hash,outcome_kind,verified_at,fresh_until
		FROM workforce_business_outcomes
		WHERE tenant_id=$1 AND organization_id=$2 AND outcome_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&storedInitiative, &metricID, &version, &hash, &kind, &verifiedAt, &freshUntil,
	); err != nil || storedInitiative != initiativeID || metricID != string(scope.Gate.Metric.ID) ||
		version != source.Version || hash != source.Hash.Digest || kind != string(scope.Gate.OutcomeKind) ||
		!freshUntil.After(now) || !verifiedAt.Equal(source.ObservedAt) ||
		source.State != SourceCompleted || source.Authority != AuthorityIndependentOutcome {
		return fmt.Errorf("%w: independently verified outcome is not current and exact", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBusinessGate(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
	scope Scope,
) error {
	var storedInitiative, decisionHash, state, outcomeID, metricID string
	var evaluatedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT decision.initiative_id,decision.decision_hash,decision.state,
		       decision.outcome_id,decision.metric_id,decision.evaluated_at
		FROM workforce_business_gate_heads head
		JOIN workforce_business_gate_decisions decision
		  ON decision.tenant_id=head.tenant_id AND decision.organization_id=head.organization_id
		 AND decision.gate_id=head.gate_id AND decision.decision_hash=head.decision_hash
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.gate_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&storedInitiative, &decisionHash, &state, &outcomeID, &metricID, &evaluatedAt,
	); err != nil || source.RecordID != string(scope.Gate.ID) || storedInitiative != initiativeID ||
		decisionHash != source.Hash.Digest || state != "satisfied" || outcomeID != string(scope.Gate.OutcomeID) ||
		metricID != string(scope.Gate.Metric.ID) || !evaluatedAt.Equal(source.ObservedAt) ||
		source.State != SourceSatisfied || source.Authority != AuthorityIndependentOutcome {
		return fmt.Errorf("%w: business gate is not authoritatively satisfied", ErrIntegrity)
	}
	return nil
}

func (store *Store) verifyBusinessCorrection(
	ctx context.Context,
	tx pgx.Tx,
	initiativeID string,
	source SourceRef,
) error {
	var storedInitiative, hash string
	var effectiveAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,content_hash,effective_at FROM workforce_business_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
	`, store.tenantID, store.organizationID, source.RecordID).Scan(
		&storedInitiative, &hash, &effectiveAt,
	); err != nil || storedInitiative != initiativeID || hash != source.Hash.Digest ||
		!effectiveAt.Equal(source.ObservedAt) || source.Authority != AuthorityInternalVerified ||
		source.State != SourceCompleted {
		return fmt.Errorf("%w: business correction source is not exact", ErrIntegrity)
	}
	return nil
}
