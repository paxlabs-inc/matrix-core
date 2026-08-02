package businessoutcome

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
)

type PostgreSQLFinancialSourceVerifier struct {
	pool     *pgxpool.Pool
	tenantID string
}

type verifiedFinancialSource struct {
	reservationID string
	accounting    bool
}

func NewPostgreSQLFinancialSourceVerifier(
	pool *pgxpool.Pool,
	tenantID string,
) (*PostgreSQLFinancialSourceVerifier, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || tenantID == "" {
		return nil, fmt.Errorf("business outcome: financial source database and tenant are required")
	}
	return &PostgreSQLFinancialSourceVerifier{pool: pool, tenantID: tenantID}, nil
}

func (verifier *PostgreSQLFinancialSourceVerifier) VerifyFinancialSources(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	primary SourceRef,
	independent SourceRef,
) error {
	if verifier == nil || verifier.pool == nil || organizationID == "" ||
		primary.Validate() != nil || independent.Validate() != nil ||
		!primary.Family.Financial() || !independent.Family.Financial() ||
		primary.Authority != AuthorityReconciledFinancial ||
		independent.Authority != AuthorityReconciledFinancial ||
		primary.State != SourceReconciled || independent.State != SourceReconciled ||
		primary.ConnectionID == "" || independent.ConnectionID == "" ||
		primary.EventID == independent.EventID || primary.Hash == independent.Hash ||
		primary.Provider == independent.Provider {
		return ErrReconciliationRequired
	}
	verifiedPrimary, err := verifier.verifyFinancialSource(ctx, organizationID, primary)
	if err != nil {
		return err
	}
	verifiedIndependent, err := verifier.verifyFinancialSource(ctx, organizationID, independent)
	if err != nil {
		return err
	}
	if verifiedPrimary.reservationID == verifiedIndependent.reservationID &&
		!verifiedPrimary.accounting && !verifiedIndependent.accounting {
		return ErrReconciliationRequired
	}
	return nil
}

func (verifier *PostgreSQLFinancialSourceVerifier) verifyFinancialSource(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	source SourceRef,
) (verifiedFinancialSource, error) {
	if source.Family == SourceAccounting {
		return verifier.verifyAccountingSource(ctx, organizationID, source)
	}
	var observationID, reservationID, adapterName, upstreamName, accountID, externalID string
	var observedAt time.Time
	err := verifier.pool.QueryRow(ctx, `
		SELECT observation.observation_id,observation.reservation_id,
		       connection.adapter_name,connection.external_adapter_name,
		       connection.account_id,observation.external_id,
		       observation.provider_observed_at
		FROM workforce_financial_observations observation
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=observation.tenant_id
		 AND connection.organization_id=observation.organization_id
		 AND connection.connection_id=observation.connection_id
		 AND connection.version=observation.connection_version
		WHERE observation.tenant_id=$1 AND observation.organization_id=$2
		  AND observation.connection_id=$3 AND observation.connection_version=$4
		  AND observation.operation=$5 AND observation.idempotency_key=$6
		  AND observation.canonical_hash=$7
		  AND observation.reconciled=TRUE AND observation.economic_truth=TRUE
		  AND observation.authority IN ('provider_authoritative','control_plane_authoritative')
	`, verifier.tenantID, organizationID, source.ConnectionID,
		source.ConnectionVersion, source.Operation, source.IdempotencyKey,
		source.Hash.Digest).Scan(&observationID, &reservationID, &adapterName,
		&upstreamName, &accountID, &externalID, &observedAt)
	if err != nil {
		return verifiedFinancialSource{}, fmt.Errorf("business outcome: authoritative financial source is unavailable: %w", err)
	}
	if source.RecordID != observationID ||
		(source.Provider != adapterName && source.Provider != upstreamName) ||
		source.Account != accountID || source.ObjectRef != externalID ||
		!source.ObservedAt.Equal(observedAt.UTC()) {
		return verifiedFinancialSource{}, ErrReconciliationRequired
	}
	return verifiedFinancialSource{reservationID: reservationID}, nil
}

func (verifier *PostgreSQLFinancialSourceVerifier) verifyAccountingSource(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	source SourceRef,
) (verifiedFinancialSource, error) {
	var reservationID, observationID, adapterName, upstreamName, accountID string
	var valuedAt time.Time
	err := verifier.pool.QueryRow(ctx, `
		SELECT entry.reservation_id,entry.observation_id,connection.adapter_name,
		       connection.external_adapter_name,entry.account_id,entry.valuation_time
		FROM workforce_financial_accounting_entries entry
		JOIN workforce_financial_observations observation
		  ON observation.tenant_id=entry.tenant_id
		 AND observation.organization_id=entry.organization_id
		 AND observation.observation_id=entry.observation_id
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=observation.tenant_id
		 AND connection.organization_id=observation.organization_id
		 AND connection.connection_id=observation.connection_id
		 AND connection.version=observation.connection_version
		WHERE entry.tenant_id=$1 AND entry.organization_id=$2
		  AND entry.entry_id=$3 AND entry.evidence_hash=$4
		  AND observation.connection_id=$5 AND observation.connection_version=$6
		  AND observation.operation=$7 AND observation.idempotency_key=$8
		  AND observation.reconciled=TRUE AND observation.economic_truth=TRUE
		  AND observation.authority IN ('provider_authoritative','control_plane_authoritative')
	`, verifier.tenantID, organizationID, source.RecordID, source.Hash.Digest,
		source.ConnectionID, source.ConnectionVersion, source.Operation,
		source.IdempotencyKey).Scan(&reservationID, &observationID, &adapterName,
		&upstreamName, &accountID, &valuedAt)
	if err != nil {
		return verifiedFinancialSource{}, fmt.Errorf("business outcome: authoritative accounting source is unavailable: %w", err)
	}
	if (source.Provider != "accounting-ledger" && source.Provider != adapterName &&
		source.Provider != upstreamName) || source.Account != accountID ||
		source.ObjectRef != observationID || !source.ObservedAt.Equal(valuedAt.UTC()) {
		return verifiedFinancialSource{}, ErrReconciliationRequired
	}
	return verifiedFinancialSource{reservationID: reservationID, accounting: true}, nil
}

var _ FinancialSourceVerifier = (*PostgreSQLFinancialSourceVerifier)(nil)
