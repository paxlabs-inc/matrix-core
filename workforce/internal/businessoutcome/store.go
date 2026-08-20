package businessoutcome

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	ErrConflict               = errors.New("business outcome: immutable conflict")
	ErrNotFound               = errors.New("business outcome: record not found")
	ErrUnauthorized           = errors.New("business outcome: unauthorized")
	ErrIntegrity              = errors.New("business outcome: integrity failure")
	ErrStale                  = errors.New("business outcome: stale")
	ErrReconciliationRequired = errors.New("business outcome: reconciliation required")
	ErrContaminated           = errors.New("business outcome: contaminated")
)

type FinancialSourceVerifier interface {
	VerifyFinancialSources(context.Context, contracts.OrganizationID, SourceRef, SourceRef) error
}

type Store struct {
	pool              *pgxpool.Pool
	vault             *vault.UserVault
	tenantID          string
	organizationID    contracts.OrganizationID
	now               func() time.Time
	financialVerifier FinancialSourceVerifier
}

func NewStore(pool *pgxpool.Pool, userVault *vault.UserVault, tenantID string, organizationID contracts.OrganizationID, now func() time.Time) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" || now == nil {
		return nil, fmt.Errorf("business outcome: PostgreSQL, Vault, tenant, organization, and time source are required")
	}
	if userVault.User() != tenantID || validateToken("organization_id", string(organizationID)) != nil {
		return nil, fmt.Errorf("business outcome: Vault tenant or organization is invalid")
	}
	return &Store{pool: pool, vault: userVault, tenantID: tenantID, organizationID: organizationID, now: now}, nil
}

func (store *Store) AttachFinancialSourceVerifier(verifier FinancialSourceVerifier) error {
	if verifier == nil {
		return fmt.Errorf("business outcome: financial source verifier is required")
	}
	store.financialVerifier = verifier
	return nil
}

func (store *Store) RegisterMetric(ctx context.Context, value MetricDefinition) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID ||
		value.Body.RegisteredAt.After(now) || value.Body.EffectiveAt.After(now) || !value.Body.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.metricAD(value.Body.ID, value.Body.Version), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal metric definition: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin metric registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "metric", string(value.Body.ID)); err != nil {
		return false, err
	}
	publicKey, role, _, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || role == "auditor" || VerifyMetricDefinition(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_business_metric_definitions
		WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version).Scan(&existingHash)
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
		return false, fmt.Errorf("business outcome: inspect metric replay: %w", err)
	}
	var headVersion uint64
	var headHash string
	var headInitiative string
	err = tx.QueryRow(ctx, `
		SELECT version,definition_hash,initiative_id
		FROM workforce_business_metric_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, value.Body.ID).Scan(&headVersion, &headHash, &headInitiative)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (value.Body.Version != 1 || value.Body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (value.Body.Version != headVersion+1 || value.Body.Supersedes == nil ||
		value.Body.Supersedes.Version != headVersion || value.Body.Supersedes.DefinitionHash.Digest != headHash ||
		value.Body.InitiativeID != headInitiative):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("business outcome: inspect metric head: %w", err)
	}
	for _, guardrail := range value.Body.Guardrails {
		var current bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_metric_heads
				WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3
				  AND version=$4 AND definition_hash=$5 AND initiative_id=$6
			)
		`, store.tenantID, store.organizationID, guardrail.ID, guardrail.Version,
			guardrail.DefinitionHash.Digest, value.Body.InitiativeID).Scan(&current); err != nil {
			return false, fmt.Errorf("business outcome: inspect guardrail metric: %w", err)
		}
		if !current {
			return false, ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_metric_definitions (
			tenant_id,organization_id,metric_id,version,initiative_id,outcome_kind,
			aggregation,unit,scale,definition_hash,canonical_hash,author_seat_id,
			key_id,sealed_definition,registered_at,effective_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
		value.Body.InitiativeID, value.Body.OutcomeKind, value.Body.Aggregation,
		value.Body.Measure.Unit, value.Body.Measure.Scale, value.ContentHash.Digest,
		canonicalHash.Digest, value.Body.AuthorSeatID, value.Signature.KeyID,
		sealed, value.Body.RegisteredAt, value.Body.EffectiveAt, value.Body.ExpiresAt)
	if err != nil {
		return false, fmt.Errorf("business outcome: insert metric definition: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_metric_heads (
			tenant_id,organization_id,metric_id,version,initiative_id,outcome_kind,
			definition_hash,effective_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,metric_id) DO UPDATE SET
			version=EXCLUDED.version,initiative_id=EXCLUDED.initiative_id,
			outcome_kind=EXCLUDED.outcome_kind,definition_hash=EXCLUDED.definition_hash,
			effective_at=EXCLUDED.effective_at,expires_at=EXCLUDED.expires_at,
			updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
		value.Body.InitiativeID, value.Body.OutcomeKind, value.ContentHash.Digest,
		value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, fmt.Errorf("business outcome: update metric head: %w", err)
	}
	if value.Body.Supersedes != nil {
		if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
			value.Body.InitiativeID,
			RecordPointer{Kind: "metric", ID: string(value.Body.Supersedes.ID), Hash: value.Body.Supersedes.DefinitionHash},
			RecordPointer{Kind: "metric", ID: string(value.Body.ID), Hash: value.ContentHash},
			"superseded_by", true, value.Body.AuthorSeatID, value.Signature.KeyID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit metric registration: %w", err)
	}
	return false, nil
}

func (store *Store) LoadMetric(ctx context.Context, reference MetricReference, requireCurrent bool) (MetricDefinition, error) {
	if reference.Validate() != nil {
		return MetricDefinition{}, fmt.Errorf("business outcome: metric reference is invalid")
	}
	if requireCurrent {
		var current bool
		err := store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_metric_heads
				WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3
				  AND version=$4 AND definition_hash=$5
			)
		`, store.tenantID, store.organizationID, reference.ID, reference.Version,
			reference.DefinitionHash.Digest).Scan(&current)
		if err != nil {
			return MetricDefinition{}, fmt.Errorf("business outcome: inspect current metric: %w", err)
		}
		if !current {
			return MetricDefinition{}, ErrStale
		}
	}
	var canonicalHash, authorSeatID, keyID string
	var sealed []byte
	var effectiveAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,author_seat_id,key_id,sealed_definition,effective_at
		FROM workforce_business_metric_definitions
		WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3 AND version=$4
		  AND definition_hash=$5
	`, store.tenantID, store.organizationID, reference.ID, reference.Version,
		reference.DefinitionHash.Digest).Scan(&canonicalHash, &authorSeatID, &keyID, &sealed, &effectiveAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MetricDefinition{}, ErrNotFound
	}
	if err != nil {
		return MetricDefinition{}, fmt.Errorf("business outcome: load metric definition: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.metricAD(reference.ID, reference.Version), sealed)
	if err != nil {
		return MetricDefinition{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[MetricDefinition, *MetricDefinition](opened)
	if err != nil || value.Reference() != reference || value.Body.OrganizationID != store.organizationID ||
		string(value.Body.AuthorSeatID) != authorSeatID || value.Signature.KeyID != keyID {
		return MetricDefinition{}, ErrIntegrity
	}
	actualHash, err := contracts.HashCanonical(&value)
	if err != nil || actualHash.Digest != canonicalHash {
		return MetricDefinition{}, ErrIntegrity
	}
	publicKey, err := store.resolveHistoricalSeatKey(ctx, value.Body.AuthorSeatID, value.Signature.KeyID, effectiveAt)
	if err != nil || VerifyMetricDefinition(value, publicKey) != nil {
		return MetricDefinition{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) CommitObservation(ctx context.Context, value Observation) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID ||
		value.Body.CapturedAt.After(now) || value.Body.ObservedAt.After(now.Add(5*time.Minute)) {
		return false, ErrUnauthorized
	}
	definition, err := store.LoadMetric(ctx, value.Body.Metric, true)
	if err != nil {
		return false, err
	}
	if err := observationCompatible(value.Body, definition.Body); err != nil {
		return false, err
	}
	if err := store.verifySource(ctx, value.Body.Primary); err != nil {
		return false, err
	}
	for _, source := range value.Body.Supporting {
		if err := store.verifySource(ctx, source); err != nil {
			return false, err
		}
	}
	if value.Body.Primary.Family.Financial() && value.Body.Status == MeasurementReconciled {
		commercialBacked, checkErr := store.sourceBackedByCommercialRecord(ctx, value.Body.Primary)
		if checkErr != nil {
			return false, checkErr
		}
		if !commercialBacked && (store.financialVerifier == nil || value.Body.Reconciliation.Independent == nil) {
			return false, ErrReconciliationRequired
		}
		if !commercialBacked {
			if err := store.financialVerifier.VerifyFinancialSources(ctx, store.organizationID,
				value.Body.Primary, *value.Body.Reconciliation.Independent); err != nil {
				return false, fmt.Errorf("business outcome: verify financial sources: %w", err)
			}
		}
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.observationAD(value.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal observation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin observation commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "source-event", value.Body.Primary.EventID); err != nil {
		return false, err
	}
	publicKey, role, _, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || role == "auditor" || VerifyObservation(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_business_observations
		WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
	`, store.tenantID, store.organizationID, value.Body.ID).Scan(&existingHash)
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
		return false, fmt.Errorf("business outcome: inspect observation replay: %w", err)
	}
	var priorObservationID, priorSourceHash string
	err = tx.QueryRow(ctx, `
		SELECT observation_id,source_hash FROM workforce_business_source_events
		WHERE tenant_id=$1 AND organization_id=$2 AND source_family=$3 AND event_id=$4
	`, store.tenantID, store.organizationID, value.Body.Primary.Family,
		value.Body.Primary.EventID).Scan(&priorObservationID, &priorSourceHash)
	if err == nil && (priorObservationID != string(value.Body.ID) || priorSourceHash != value.Body.Primary.Hash.Digest) {
		return false, ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("business outcome: inspect source event: %w", err)
	}
	if value.Body.Supersedes != nil {
		var priorMetricID string
		var priorInitiative string
		var priorContentHash string
		if err := tx.QueryRow(ctx, `
			SELECT metric_id,initiative_id,content_hash FROM workforce_business_observations
			WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
		`, store.tenantID, store.organizationID, *value.Body.Supersedes).Scan(&priorMetricID, &priorInitiative, &priorContentHash); err != nil ||
			priorMetricID != string(value.Body.Metric.ID) || priorInitiative != value.Body.InitiativeID {
			return false, ErrConflict
		}
		if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
			value.Body.InitiativeID,
			RecordPointer{Kind: "observation", ID: string(*value.Body.Supersedes), Hash: contracts.ContentHash{Algorithm: "sha256", Digest: priorContentHash}},
			RecordPointer{Kind: "observation", ID: string(value.Body.ID), Hash: value.ContentHash},
			"superseded_by", true, value.Body.AuthorSeatID, value.Signature.KeyID, now); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_observations (
			tenant_id,organization_id,observation_id,initiative_id,metric_id,
			metric_version,definition_hash,outcome_kind,status,source_family,
			source_event_id,source_hash,value_micros,numerator_micros,denominator,
			unit,scale,uncertainty_bps,author_seat_id,key_id,content_hash,
			canonical_hash,sealed_observation,observed_at,captured_at,fresh_until,supersedes
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27
		)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.InitiativeID,
		value.Body.Metric.ID, value.Body.Metric.Version, value.Body.Metric.DefinitionHash.Digest,
		value.Body.OutcomeKind, value.Body.Status, value.Body.Primary.Family,
		value.Body.Primary.EventID, value.Body.Primary.Hash.Digest, value.Body.Value.ValueMicros,
		value.Body.Value.NumeratorMicros, value.Body.Value.Denominator, value.Body.Value.Unit,
		value.Body.Value.Scale, value.Body.UncertaintyBPS, value.Body.AuthorSeatID,
		value.Signature.KeyID, value.ContentHash.Digest, canonicalHash.Digest, sealed,
		value.Body.ObservedAt, value.Body.CapturedAt, value.Body.FreshUntil,
		optionalObservationID(value.Body.Supersedes))
	if err != nil {
		return false, fmt.Errorf("business outcome: insert observation: %w", err)
	}
	if err := store.insertObservationSourcesTx(ctx, tx, value, now); err != nil {
		return false, err
	}
	if err := insertSystemLineageTx(ctx, tx, store.tenantID, store.organizationID,
		value.Body.InitiativeID,
		RecordPointer{Kind: "metric", ID: string(value.Body.Metric.ID), Hash: value.Body.Metric.DefinitionHash},
		RecordPointer{Kind: "observation", ID: string(value.Body.ID), Hash: value.ContentHash},
		"measures_metric", true, value.Body.AuthorSeatID, value.Signature.KeyID, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit observation: %w", err)
	}
	return false, nil
}

func (store *Store) LoadObservation(ctx context.Context, id ObservationID) (Observation, error) {
	if validateToken("observation_id", string(id)) != nil {
		return Observation{}, fmt.Errorf("business outcome: observation identity is invalid")
	}
	var expectedHash, contentHash, authorSeatID, keyID string
	var sealed []byte
	var observedAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,content_hash,author_seat_id,key_id,sealed_observation,observed_at
		FROM workforce_business_observations
		WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&expectedHash, &contentHash, &authorSeatID, &keyID, &sealed, &observedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Observation{}, ErrNotFound
	}
	if err != nil {
		return Observation{}, fmt.Errorf("business outcome: load observation: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.observationAD(id), sealed)
	if err != nil {
		return Observation{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Observation, *Observation](opened)
	if err != nil || value.Body.ID != id || value.Body.OrganizationID != store.organizationID ||
		value.ContentHash.Digest != contentHash || string(value.Body.AuthorSeatID) != authorSeatID ||
		value.Signature.KeyID != keyID {
		return Observation{}, ErrIntegrity
	}
	actualHash, err := contracts.HashCanonical(&value)
	if err != nil || actualHash.Digest != expectedHash {
		return Observation{}, ErrIntegrity
	}
	publicKey, err := store.resolveHistoricalSeatKey(ctx, value.Body.AuthorSeatID, keyID, observedAt)
	if err != nil || VerifyObservation(value, publicKey) != nil {
		return Observation{}, ErrIntegrity
	}
	return value, nil
}

func observationCompatible(value ObservationBody, definition MetricDefinitionBody) error {
	if value.Metric.ID != definition.ID || value.Metric.Version != definition.Version ||
		value.OrganizationID != definition.OrganizationID || value.InitiativeID != definition.InitiativeID ||
		value.OutcomeKind != definition.OutcomeKind || value.Value.Unit != definition.Measure.Unit ||
		value.Value.Scale != definition.Measure.Scale || value.SubjectRef != definition.Attribution.Subject ||
		value.ObservedAt.Before(definition.EffectiveAt) || !definition.ExpiresAt.After(value.ObservedAt) ||
		value.CapturedAt.Sub(value.ObservedAt) > definition.Freshness.MaximumCaptureDelay ||
		value.FreshUntil.Sub(value.ObservedAt) > definition.Freshness.MaximumObservationAge ||
		value.UncertaintyBPS > definition.MaximumUncertaintyBPS ||
		!slices.Contains(definition.Sources, value.Primary.Family) {
		return fmt.Errorf("business outcome: observation is incompatible with its metric definition")
	}
	if value.Metric.DefinitionHash == (contracts.ContentHash{}) {
		return fmt.Errorf("business outcome: observation lacks an exact metric definition hash")
	}
	if definition.Reconciliation.Required {
		if value.Status == MeasurementObserved || value.Status == MeasurementReconciled && value.Reconciliation.State != ReconciliationReconciled {
			return ErrReconciliationRequired
		}
		if value.Status == MeasurementReconciled && (value.Reconciliation.Independent == nil ||
			!slices.Contains(definition.Reconciliation.IndependentFamilies, value.Reconciliation.Independent.Family)) {
			return ErrReconciliationRequired
		}
	}
	return nil
}

func (store *Store) insertObservationSourcesTx(ctx context.Context, tx pgx.Tx, value Observation, now time.Time) error {
	sources := append([]SourceRef{value.Body.Primary}, value.Body.Supporting...)
	for index, source := range sources {
		role := "supporting"
		if index == 0 {
			role = "primary"
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_business_observation_sources (
				tenant_id,organization_id,observation_id,source_role,source_family,
				authority,record_id,event_id,source_hash,provider,account_ref,
				object_ref,source_state,observed_at,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		`, store.tenantID, store.organizationID, value.Body.ID, role, source.Family,
			source.Authority, source.RecordID, source.EventID, source.Hash.Digest,
			source.Provider, source.Account, source.ObjectRef, source.State, source.ObservedAt, now)
		if err != nil {
			return fmt.Errorf("business outcome: insert observation source: %w", err)
		}
		if index == 0 {
			_, err = tx.Exec(ctx, `
				INSERT INTO workforce_business_source_events (
					tenant_id,organization_id,source_family,event_id,source_hash,observation_id,created_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7)
			`, store.tenantID, store.organizationID, source.Family, source.EventID,
				source.Hash.Digest, value.Body.ID, now)
			if err != nil {
				return fmt.Errorf("business outcome: insert source event identity: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_business_lineage_edges (
				tenant_id,organization_id,edge_id,initiative_id,source_kind,source_id,
				source_hash,consumer_kind,consumer_id,consumer_hash,relation,material,
				author_seat_id,key_id,canonical_hash,sealed_edge,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,'observation',$8,$9,$10,true,$11,$12,NULL,NULL,$13)
		`, store.tenantID, store.organizationID,
			stableToken("lineage", string(source.Family), source.EventID, source.Hash.Digest,
				string(value.Body.ID), value.ContentHash.Digest), value.Body.InitiativeID,
			"source", source.EventID, source.Hash.Digest, value.Body.ID, value.ContentHash.Digest,
			"authoritative_observation", value.Body.AuthorSeatID, value.Signature.KeyID, now)
		if err != nil {
			return fmt.Errorf("business outcome: insert observation lineage: %w", err)
		}
	}
	return nil
}

func (store *Store) verifySource(ctx context.Context, source SourceRef) error {
	var exists bool
	var err error
	switch source.Family {
	case SourceProduct:
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_product_capability_records
				WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_hash=$4
			)
		`, store.tenantID, store.organizationID, source.RecordID, source.Hash.Digest).Scan(&exists)
	case SourceOperational:
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_product_execution_events
				WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 AND content_hash=$4
			)
		`, store.tenantID, store.organizationID, source.RecordID, source.Hash.Digest).Scan(&exists)
	case SourceExternalProvider, SourceProductTelemetry, SourceDeployment:
		if source.ConnectionID == "" {
			return store.verifyCommercialRecordSource(ctx, source)
		}
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_external_observations
				WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
				  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
				  AND canonical_hash=$7 AND external_state='completed'
			)
		`, store.tenantID, store.organizationID, source.ConnectionID, source.ConnectionVersion,
			source.Operation, source.IdempotencyKey, source.Hash.Digest).Scan(&exists)
	case SourceCustomer, SourceCRM, SourceChannel, SourceSupport:
		if source.ConnectionID == "" {
			return store.verifyCommercialRecordSource(ctx, source)
		}
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_customer_observations
				WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
				  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
				  AND canonical_hash=$7 AND external_state='completed'
			)
		`, store.tenantID, store.organizationID, source.ConnectionID, source.ConnectionVersion,
			source.Operation, source.IdempotencyKey, source.Hash.Digest).Scan(&exists)
	case SourceCommercial:
		return store.verifyCommercialRecordSource(ctx, source)
	case SourceLegal:
		return store.verifyCommercialRecordSource(ctx, source)
	case SourceAnalytical:
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_learning_records
				WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
				  AND canonical_hash=$4
			)
		`, store.tenantID, store.organizationID, source.RecordID, source.Hash.Digest).Scan(&exists)
	case SourceBilling, SourceAccounting, SourcePaxeer, SourceLayerX:
		if source.ConnectionID == "" {
			return store.verifyCommercialRecordSource(ctx, source)
		}
		return nil
	default:
		return ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("business outcome: verify source existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (store *Store) verifyCommercialRecordSource(ctx context.Context, source SourceRef) error {
	var exists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM workforce_commercial_capability_records record
			WHERE record.tenant_id=$1 AND record.organization_id=$2
			  AND record.record_id=$3 AND record.record_hash=$4
			UNION ALL
			SELECT 1
			FROM workforce_commercial_observation_bindings binding
			JOIN workforce_commercial_capability_records record
			  ON record.tenant_id=binding.tenant_id
			 AND record.organization_id=binding.organization_id
			 AND record.record_id=binding.record_id
			WHERE binding.tenant_id=$1 AND binding.organization_id=$2
			  AND binding.record_id=$3
			  AND ($4=binding.primary_evidence_hash OR $4=binding.reconciliation_evidence_hash)
		)
	`, store.tenantID, store.organizationID, source.RecordID, source.Hash.Digest).Scan(&exists); err != nil {
		return fmt.Errorf("business outcome: verify commercial source: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (store *Store) sourceBackedByCommercialRecord(ctx context.Context, source SourceRef) (bool, error) {
	if source.ConnectionID != "" {
		return false, nil
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_commercial_capability_records
			WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_hash=$4
		)
	`, store.tenantID, store.organizationID, source.RecordID, source.Hash.Digest).Scan(&exists); err != nil {
		return false, fmt.Errorf("business outcome: inspect commercial financial source: %w", err)
	}
	return exists, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("business outcome: time source must return UTC")
	}
	return now, nil
}

func (store *Store) resolveCurrentSeatKeyTx(ctx context.Context, tx pgx.Tx, seatID contracts.SeatID, keyID string, at time.Time) (ed25519.PublicKey, string, contracts.DepartmentID, error) {
	var publicKey []byte
	var role string
	var departmentID contracts.DepartmentID
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,seat.seat_role,seat.department_id
		FROM workforce_mail_keys key
		JOIN workforce_organization_seats seat
		  ON seat.tenant_id=key.tenant_id AND seat.organization_id=key.organization_id
		 AND seat.seat_id=key.seat_id
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		  AND seat.active=true
		FOR SHARE OF key,seat
	`, store.tenantID, store.organizationID, seatID, keyID, at).Scan(&publicKey, &role, &departmentID)
	if errors.Is(err, pgx.ErrNoRows) || len(publicKey) != ed25519.PublicKeySize {
		return nil, "", "", ErrUnauthorized
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("business outcome: resolve current seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), role, departmentID, nil
}

func (store *Store) resolveHistoricalSeatKey(ctx context.Context, seatID contracts.SeatID, keyID string, at time.Time) (ed25519.PublicKey, error) {
	var publicKey []byte
	err := store.pool.QueryRow(ctx, `
		SELECT public_key FROM workforce_mail_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3 AND key_id=$4
		  AND effective_at<=$5 AND (revoked_at IS NULL OR revoked_at>$5)
	`, store.tenantID, store.organizationID, seatID, keyID, at).Scan(&publicKey)
	if errors.Is(err, pgx.ErrNoRows) || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrIntegrity
	}
	if err != nil {
		return nil, fmt.Errorf("business outcome: resolve historical seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), nil
}

func lockScope(ctx context.Context, tx pgx.Tx, values ...string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, strings.Join(values, "|")); err != nil {
		return fmt.Errorf("business outcome: lock scope: %w", err)
	}
	return nil
}

func (store *Store) metricAD(id MetricID, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.metric",
		Stream: strings.Join([]string{string(store.organizationID), string(id), fmt.Sprint(version)}, "/"), Schema: SchemaVersion}
}

func (store *Store) observationAD(id ObservationID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.observation",
		Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func optionalObservationID(value *ObservationID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}
