package commercialcapability

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

var (
	ErrConflict     = errors.New("commercial capability: immutable conflict")
	ErrNotFound     = errors.New("commercial capability: record not found")
	ErrUnauthorized = errors.New("commercial capability: unauthorized")
	ErrIntegrity    = errors.New("commercial capability: integrity failure")
	ErrExpired      = errors.New("commercial capability: record expired")
)

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" || now == nil {
		return nil, fmt.Errorf("commercial capability: PostgreSQL, Vault, tenant, organization, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("commercial capability: Vault user does not match tenant")
	}
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return nil, err
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, now: now,
	}, nil
}

func (store *Store) Commit(ctx context.Context, value VerifiedRecord) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := value.ValidateAt(now); err != nil {
		if strings.Contains(err.Error(), "stale") || strings.Contains(err.Error(), "expired") {
			return false, ErrExpired
		}
		return false, err
	}
	body := value.Record.Body
	if body.OrganizationID != store.organizationID || body.EffectiveAt.After(now) ||
		value.Review.VerifiedAt.After(now) {
		return false, ErrUnauthorized
	}
	procedure, err := ProcedureForRecord(body)
	if err != nil {
		return false, err
	}
	if err := validateRequiredSources(body, procedure); err != nil {
		return false, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalDigest := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.recordAD(body), canonical)
	if err != nil {
		return false, fmt.Errorf("commercial capability: seal record: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("commercial capability: begin record commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lock := strings.Join([]string{store.tenantID, string(store.organizationID), string(body.ChainID)}, "|")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lock); err != nil {
		return false, fmt.Errorf("commercial capability: lock record chain: %w", err)
	}
	authorKey, authorRole, authorDepartment, err := store.resolveCurrentSeatKeyTx(
		ctx, tx, body.AuthorSeatID, value.Record.Signature.KeyID, now,
	)
	if err != nil || authorRole == "auditor" || authorDepartment != body.DepartmentID {
		return false, ErrUnauthorized
	}
	verifierKey, verifierRole, verifierDepartment, err := store.resolveCurrentSeatKeyTx(
		ctx, tx, value.Review.VerifierSeatID, value.Review.Signature.KeyID, now,
	)
	if err != nil || verifierRole != "auditor" || verifierDepartment == authorDepartment {
		return false, ErrUnauthorized
	}
	if err := verifyRecord(value.Record, authorKey); err != nil {
		return false, ErrIntegrity
	}
	if err := verifyReview(value.Review, verifierKey); err != nil {
		return false, ErrIntegrity
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_commercial_capability_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalDigest.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commercial capability: commit record replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("commercial capability: inspect record identity: %w", err)
	}
	var headID string
	var headVersion uint64
	var headDomain Domain
	var headKind RecordKind
	var headInitiative InitiativeID
	var headDepartment contracts.DepartmentID
	var headProject contracts.ProjectID
	var headWorkspace contracts.WorkspaceID
	err = tx.QueryRow(ctx, `
		SELECT head.record_id,head.version,head.domain,head.kind,
		       record.initiative_id,record.department_id,record.project_id,record.workspace_id
		FROM workforce_commercial_capability_heads head
		JOIN workforce_commercial_capability_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.record_id=head.record_id
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.chain_id=$3
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, body.ChainID).Scan(
		&headID, &headVersion, &headDomain, &headKind,
		&headInitiative, &headDepartment, &headProject, &headWorkspace,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (body.Version != 1 || body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (body.Version != headVersion+1 || body.Supersedes == nil ||
		string(*body.Supersedes) != headID || body.Domain != headDomain || body.Kind != headKind ||
		body.InitiativeID != headInitiative || body.DepartmentID != headDepartment || body.ProjectID != headProject ||
		body.WorkspaceID != headWorkspace):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("commercial capability: inspect record head: %w", err)
	}
	recordHash, err := RecordHash(value.Record)
	if err != nil {
		return false, err
	}
	customerHash, economicHash, err := recordBoundaryHashes(body)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_capability_records (
			tenant_id,organization_id,chain_id,record_id,version,domain,kind,
			initiative_id,department_id,project_id,workspace_id,author_seat_id,verifier_seat_id,
			supersedes,outcome_kind,customer_boundary_hash,economic_boundary_hash,
			record_hash,canonical_hash,author_key_id,verifier_key_id,sealed_record,
			created_at,effective_at,fresh_until,verified_at,review_expires_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27
		)
	`, store.tenantID, store.organizationID, body.ChainID, body.ID, body.Version,
		body.Domain, body.Kind, body.InitiativeID, body.DepartmentID, body.ProjectID, body.WorkspaceID,
		body.AuthorSeatID, value.Review.VerifierSeatID, optionalRecordID(body.Supersedes),
		body.Outcome.Kind, nullableDigest(customerHash), nullableDigest(economicHash),
		recordHash.Digest, canonicalDigest.Digest, value.Record.Signature.KeyID,
		value.Review.Signature.KeyID, sealed, body.CreatedAt, body.EffectiveAt,
		body.FreshUntil, value.Review.VerifiedAt, value.Review.ExpiresAt)
	if err != nil {
		return false, fmt.Errorf("commercial capability: insert record: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_capability_heads (
			tenant_id,organization_id,chain_id,record_id,version,domain,kind,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id,organization_id,chain_id) DO UPDATE SET
			record_id=EXCLUDED.record_id,version=EXCLUDED.version,
			domain=EXCLUDED.domain,kind=EXCLUDED.kind,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, body.ChainID, body.ID,
		body.Version, body.Domain, body.Kind, now)
	if err != nil {
		return false, fmt.Errorf("commercial capability: update record head: %w", err)
	}
	if err := store.insertBindingsTx(ctx, tx, body, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commercial capability: commit record: %w", err)
	}
	return false, nil
}

func (store *Store) insertBindingsTx(ctx context.Context, tx pgx.Tx, body RecordBody, now time.Time) error {
	for _, observation := range body.Observations {
		var reconciliationClass any
		var reconciliationHash any
		if observation.Reconciliation != nil {
			reconciliationClass = observation.Reconciliation.Class
			reconciliationHash = observation.Reconciliation.Evidence.Hash.Digest
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_commercial_observation_bindings (
				tenant_id,organization_id,record_id,observation_id,observation_kind,
				primary_source_class,primary_evidence_hash,reconciliation_source_class,
				reconciliation_evidence_hash,value_hash,observed_at,fresh_until,
				uncertainty_bps,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		`, store.tenantID, store.organizationID, body.ID, observation.ID,
			observation.Kind, observation.Primary.Class, observation.Primary.Evidence.Hash.Digest,
			reconciliationClass, reconciliationHash, observation.Value.ValueHash.Digest,
			observation.ObservedAt, observation.FreshUntil, observation.UncertaintyBPS, now)
		if err != nil {
			return fmt.Errorf("commercial capability: insert observation binding: %w", err)
		}
	}
	for _, metric := range body.Metrics {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_commercial_metric_bindings (
				tenant_id,organization_id,record_id,metric_id,metric_version,
				definition_hash,source_class,source_provider,freshness_micros,
				maximum_uncertainty_bps
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, store.tenantID, store.organizationID, body.ID, metric.ID, metric.Version,
			metric.DefinitionHash.Digest, metric.SourceClass, metric.SourceProvider,
			metric.Freshness.Microseconds(), metric.MaximumUncertaintyBPS)
		if err != nil {
			return fmt.Errorf("commercial capability: insert metric binding: %w", err)
		}
	}
	for _, handoff := range body.Handoffs {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_commercial_handoffs (
				tenant_id,organization_id,handoff_id,record_id,kind,from_domain,
				to_domain,created_at,expires_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, handoff.ID, body.ID, handoff.Kind,
			handoff.FromDomain, handoff.ToDomain, handoff.CreatedAt, handoff.ExpiresAt)
		if err != nil {
			return fmt.Errorf("commercial capability: insert handoff: %w", err)
		}
	}
	return nil
}

func (store *Store) LoadCurrent(ctx context.Context, chainID ChainID) (VerifiedRecord, error) {
	if err := validateToken("chain_id", string(chainID)); err != nil {
		return VerifiedRecord{}, err
	}
	var recordID RecordID
	err := store.pool.QueryRow(ctx, `
		SELECT record_id FROM workforce_commercial_capability_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND chain_id=$3
	`, store.tenantID, store.organizationID, chainID).Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedRecord{}, ErrNotFound
	}
	if err != nil {
		return VerifiedRecord{}, fmt.Errorf("commercial capability: load record head: %w", err)
	}
	return store.Load(ctx, recordID)
}

func (store *Store) Load(ctx context.Context, recordID RecordID) (VerifiedRecord, error) {
	return store.load(ctx, recordID, true)
}

func (store *Store) ListCurrent(
	ctx context.Context,
	domain *Domain,
	offset uint64,
	limit int,
) ([]VerifiedRecord, bool, error) {
	if limit <= 0 || limit > 200 || offset > uint64(1<<63-1) {
		return nil, false, fmt.Errorf("commercial capability: projection page is outside bounds")
	}
	domainValue := ""
	if domain != nil {
		if !domain.Valid() {
			return nil, false, fmt.Errorf("commercial capability: projection domain is invalid")
		}
		domainValue = string(*domain)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT record.record_id
		FROM workforce_commercial_capability_heads head
		JOIN workforce_commercial_capability_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.chain_id=head.chain_id
		 AND record.record_id=head.record_id
		 AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND ($3='' OR record.domain=$3)
		ORDER BY record.verified_at DESC,record.record_id
		LIMIT $4 OFFSET $5
	`, store.tenantID, store.organizationID, domainValue, limit+1, int64(offset))
	if err != nil {
		return nil, false, fmt.Errorf("commercial capability: list current records: %w", err)
	}
	defer rows.Close()
	ids := make([]RecordID, 0, limit+1)
	for rows.Next() {
		var id RecordID
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("commercial capability: scan current record: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("commercial capability: list current records: %w", err)
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	result := make([]VerifiedRecord, 0, len(ids))
	for _, id := range ids {
		value, err := store.load(ctx, id, false)
		if err != nil {
			return nil, false, fmt.Errorf("commercial capability: project record %q: %w", id, err)
		}
		result = append(result, value)
	}
	return result, hasMore, nil
}

func (store *Store) ListReadyHandoffs(
	ctx context.Context,
	toDomain string,
	limit int,
) ([]CrossFunctionalHandoff, error) {
	if err := validateToken("handoff destination", toDomain); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("commercial capability: handoff page is outside bounds")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT handoff.handoff_id,handoff.record_id
		FROM workforce_commercial_handoffs handoff
		JOIN workforce_commercial_capability_heads head
		  ON head.tenant_id=handoff.tenant_id
		 AND head.organization_id=handoff.organization_id
		 AND head.record_id=handoff.record_id
		WHERE handoff.tenant_id=$1 AND handoff.organization_id=$2
		  AND handoff.to_domain=$3 AND handoff.expires_at>$4
		ORDER BY handoff.created_at,handoff.handoff_id
		LIMIT $5
	`, store.tenantID, store.organizationID, toDomain, now, limit)
	if err != nil {
		return nil, fmt.Errorf("commercial capability: list handoffs: %w", err)
	}
	defer rows.Close()
	type handoffRow struct {
		handoffID HandoffID
		recordID  RecordID
	}
	refs := make([]handoffRow, 0, limit)
	for rows.Next() {
		var ref handoffRow
		if err := rows.Scan(&ref.handoffID, &ref.recordID); err != nil {
			return nil, fmt.Errorf("commercial capability: scan handoff: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("commercial capability: list handoffs: %w", err)
	}
	result := make([]CrossFunctionalHandoff, 0, len(refs))
	for _, ref := range refs {
		record, err := store.Load(ctx, ref.recordID)
		if err != nil {
			return nil, err
		}
		found := false
		for _, handoff := range record.Record.Body.Handoffs {
			if handoff.ID == ref.handoffID {
				result = append(result, handoff)
				found = true
				break
			}
		}
		if !found {
			return nil, ErrIntegrity
		}
	}
	return result, nil
}

func (store *Store) load(ctx context.Context, recordID RecordID, requireFresh bool) (VerifiedRecord, error) {
	if err := validateToken("record_id", string(recordID)); err != nil {
		return VerifiedRecord{}, err
	}
	var chainID ChainID
	var version uint64
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT chain_id,version,canonical_hash,sealed_record
		FROM workforce_commercial_capability_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, recordID).Scan(
		&chainID, &version, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedRecord{}, ErrNotFound
	}
	if err != nil {
		return VerifiedRecord{}, fmt.Errorf("commercial capability: load record: %w", err)
	}
	locator := RecordBody{ID: recordID, ChainID: chainID, Version: version, OrganizationID: store.organizationID}
	canonical, err := store.vault.OpenRecord(store.recordAD(locator), sealed)
	if err != nil || digestBytes(canonical).Digest != expectedHash {
		return VerifiedRecord{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[VerifiedRecord, *VerifiedRecord](canonical)
	if err != nil || value.Record.Body.ID != recordID || value.Record.Body.ChainID != chainID ||
		value.Record.Body.Version != version || value.Record.Body.OrganizationID != store.organizationID {
		return VerifiedRecord{}, ErrIntegrity
	}
	authorKey, err := store.resolveHistoricalSeatKey(
		ctx, value.Record.Body.AuthorSeatID, value.Record.Signature.KeyID,
		value.Record.Body.EffectiveAt,
	)
	if err != nil || verifyRecord(value.Record, authorKey) != nil {
		return VerifiedRecord{}, ErrIntegrity
	}
	verifierKey, err := store.resolveHistoricalSeatKey(
		ctx, value.Review.VerifierSeatID, value.Review.Signature.KeyID,
		value.Review.VerifiedAt,
	)
	if err != nil || verifyReview(value.Review, verifierKey) != nil {
		return VerifiedRecord{}, ErrIntegrity
	}
	validationTime := value.Review.VerifiedAt
	if requireFresh {
		validationTime, err = store.currentTime()
		if err != nil {
			return VerifiedRecord{}, err
		}
	}
	if err := value.ValidateAt(validationTime); err != nil {
		if requireFresh {
			return VerifiedRecord{}, ErrExpired
		}
		return VerifiedRecord{}, ErrIntegrity
	}
	procedure, err := ProcedureForRecord(value.Record.Body)
	if err != nil || validateRequiredSources(value.Record.Body, procedure) != nil {
		return VerifiedRecord{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) AdvanceCheckpoint(
	ctx context.Context,
	expectedVersion uint64,
	next Checkpoint,
) (Checkpoint, bool, error) {
	if err := next.Validate(); err != nil {
		return Checkpoint{}, false, err
	}
	if next.OrganizationID != store.organizationID || next.Version != expectedVersion+1 {
		return Checkpoint{}, false, ErrConflict
	}
	now, err := store.currentTime()
	if err != nil {
		return Checkpoint{}, false, err
	}
	if next.UpdatedAt.After(now) || !next.Source.Fresh || !next.Source.FreshUntil.After(now) {
		return Checkpoint{}, false, ErrConflict
	}
	canonical, err := contracts.EncodeCanonical(&next)
	if err != nil {
		return Checkpoint{}, false, err
	}
	digest := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.checkpointAD(next), canonical)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: seal checkpoint: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: begin checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var replayID CheckpointID
	var replayVersion uint64
	var replayHash string
	err = tx.QueryRow(ctx, `
		SELECT checkpoint_id,version,canonical_hash
		FROM workforce_commercial_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND workflow_id=$4 AND idempotency_key=$5
	`, store.tenantID, store.organizationID, next.InitiativeID,
		next.WorkflowID, next.IdempotencyKey).Scan(&replayID, &replayVersion, &replayHash)
	if err == nil {
		if replayID != next.ID || replayVersion != next.Version || replayHash != digest.Digest {
			return Checkpoint{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Checkpoint{}, false, err
		}
		return next, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: inspect checkpoint replay: %w", err)
	}
	var previousID CheckpointID
	var previousVersion uint64
	var previousHash string
	var previousSealed []byte
	err = tx.QueryRow(ctx, `
		SELECT checkpoint.checkpoint_id,checkpoint.version,
		       checkpoint.canonical_hash,checkpoint.sealed_checkpoint
		FROM workforce_commercial_checkpoint_heads head
		JOIN workforce_commercial_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.workflow_id=head.workflow_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.initiative_id=$3 AND head.workflow_id=$4
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, next.InitiativeID, next.WorkflowID).Scan(
		&previousID, &previousVersion, &previousHash, &previousSealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if expectedVersion != 0 || next.Version != 1 || next.Phase != PhaseIntake {
			return Checkpoint{}, false, ErrConflict
		}
	} else if err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: load checkpoint head: %w", err)
	} else {
		locator := Checkpoint{
			ID: previousID, Version: previousVersion, OrganizationID: store.organizationID,
			InitiativeID: next.InitiativeID, WorkflowID: next.WorkflowID,
		}
		opened, openErr := store.vault.OpenRecord(store.checkpointAD(locator), previousSealed)
		if openErr != nil || digestBytes(opened).Digest != previousHash {
			return Checkpoint{}, false, ErrIntegrity
		}
		previous, decodeErr := contracts.DecodeCanonical[Checkpoint, *Checkpoint](opened)
		if decodeErr != nil || previous.Version != expectedVersion || ValidateResume(previous, next) != nil {
			return Checkpoint{}, false, ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_checkpoints (
			tenant_id,organization_id,initiative_id,workflow_id,checkpoint_id,
			version,skill_id,record_chain_id,phase,idempotency_key,
			source_generation,canonical_hash,sealed_checkpoint,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.tenantID, store.organizationID, next.InitiativeID, next.WorkflowID,
		next.ID, next.Version, next.SkillID, next.RecordChainID, next.Phase,
		next.IdempotencyKey, next.Source.Generation, digest.Digest, sealed, next.UpdatedAt)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: insert checkpoint: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_checkpoint_heads (
			tenant_id,organization_id,initiative_id,workflow_id,checkpoint_id,
			version,phase,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id,organization_id,initiative_id,workflow_id) DO UPDATE SET
			checkpoint_id=EXCLUDED.checkpoint_id,version=EXCLUDED.version,
			phase=EXCLUDED.phase,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, next.InitiativeID, next.WorkflowID,
		next.ID, next.Version, next.Phase, next.UpdatedAt)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: update checkpoint head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Checkpoint{}, false, fmt.Errorf("commercial capability: commit checkpoint: %w", err)
	}
	return next, false, nil
}

func (store *Store) LoadCheckpoint(
	ctx context.Context,
	initiativeID InitiativeID,
	workflowID WorkflowID,
) (Checkpoint, error) {
	if err := validateToken("initiative_id", string(initiativeID)); err != nil {
		return Checkpoint{}, err
	}
	if err := validateToken("workflow_id", string(workflowID)); err != nil {
		return Checkpoint{}, err
	}
	var id CheckpointID
	var version uint64
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT checkpoint.checkpoint_id,checkpoint.version,
		       checkpoint.canonical_hash,checkpoint.sealed_checkpoint
		FROM workforce_commercial_checkpoint_heads head
		JOIN workforce_commercial_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.workflow_id=head.workflow_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.initiative_id=$3 AND head.workflow_id=$4
	`, store.tenantID, store.organizationID, initiativeID, workflowID).Scan(
		&id, &version, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("commercial capability: load checkpoint: %w", err)
	}
	locator := Checkpoint{
		ID: id, Version: version, OrganizationID: store.organizationID,
		InitiativeID: initiativeID, WorkflowID: workflowID,
	}
	opened, err := store.vault.OpenRecord(store.checkpointAD(locator), sealed)
	if err != nil || digestBytes(opened).Digest != expectedHash {
		return Checkpoint{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Checkpoint, *Checkpoint](opened)
	if err != nil || value.ID != id || value.Version != version ||
		value.OrganizationID != store.organizationID || value.InitiativeID != initiativeID ||
		value.WorkflowID != workflowID {
		return Checkpoint{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) CommitQualification(
	ctx context.Context,
	record VerifiedRecord,
	checkpoint Checkpoint,
	evidence QualificationEvidence,
) (QualificationResult, bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return QualificationResult{}, false, err
	}
	if record.Record.Body.OrganizationID != store.organizationID ||
		checkpoint.OrganizationID != store.organizationID {
		return QualificationResult{}, false, ErrUnauthorized
	}
	committedRecord, err := store.load(ctx, record.Record.Body.ID, false)
	if err != nil {
		return QualificationResult{}, false, err
	}
	providedRecord, err := contracts.EncodeCanonical(&record)
	if err != nil {
		return QualificationResult{}, false, err
	}
	storedRecord, err := contracts.EncodeCanonical(&committedRecord)
	if err != nil || digestBytes(providedRecord) != digestBytes(storedRecord) {
		return QualificationResult{}, false, ErrIntegrity
	}
	checkpointBytes, err := contracts.EncodeCanonical(&checkpoint)
	if err != nil {
		return QualificationResult{}, false, err
	}
	var checkpointHash string
	err = store.pool.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_commercial_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND workflow_id=$4 AND checkpoint_id=$5 AND version=$6
	`, store.tenantID, store.organizationID, checkpoint.InitiativeID,
		checkpoint.WorkflowID, checkpoint.ID, checkpoint.Version).Scan(&checkpointHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return QualificationResult{}, false, ErrNotFound
	}
	if err != nil {
		return QualificationResult{}, false, fmt.Errorf("commercial capability: load qualification checkpoint: %w", err)
	}
	if digestBytes(checkpointBytes).Digest != checkpointHash {
		return QualificationResult{}, false, ErrIntegrity
	}
	result, err := QualifyAt(record, checkpoint, evidence, now)
	if err != nil {
		return QualificationResult{}, false, err
	}
	envelope := QualificationEnvelope{Evidence: evidence, Result: result}
	canonical, err := contracts.EncodeCanonical(&envelope)
	if err != nil {
		return QualificationResult{}, false, err
	}
	canonicalHash := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.qualificationAD(envelope), canonical)
	if err != nil {
		return QualificationResult{}, false, fmt.Errorf("commercial capability: seal qualification: %w", err)
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_commercial_qualifications (
			tenant_id,organization_id,record_id,initiative_id,workflow_id,
			checkpoint_id,checkpoint_version,skill_id,author_wake_id,
			verifier_wake_id,evidence_digest,canonical_hash,sealed_qualification,qualified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (tenant_id,organization_id,record_id,evidence_digest) DO NOTHING
	`, store.tenantID, store.organizationID, record.Record.Body.ID, checkpoint.InitiativeID,
		checkpoint.WorkflowID, checkpoint.ID, checkpoint.Version, result.SkillID,
		result.AuthorWakeID, result.VerifierWakeID, result.EvidenceDigest.Digest,
		canonicalHash.Digest, sealed, result.QualifiedAt)
	if err != nil {
		return QualificationResult{}, false, fmt.Errorf("commercial capability: commit qualification: %w", err)
	}
	if command.RowsAffected() == 1 {
		return result, false, nil
	}
	var existingHash string
	err = store.pool.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_commercial_qualifications
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND evidence_digest=$4
	`, store.tenantID, store.organizationID, record.Record.Body.ID,
		result.EvidenceDigest.Digest).Scan(&existingHash)
	if err != nil || existingHash != canonicalHash.Digest {
		return QualificationResult{}, false, ErrConflict
	}
	return result, true, nil
}

func (store *Store) LoadQualification(
	ctx context.Context,
	recordID RecordID,
	evidenceDigest contracts.ContentHash,
) (QualificationEnvelope, error) {
	if err := validateToken("qualification record_id", string(recordID)); err != nil {
		return QualificationEnvelope{}, err
	}
	if err := evidenceDigest.Validate(); err != nil {
		return QualificationEnvelope{}, err
	}
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_qualification
		FROM workforce_commercial_qualifications
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND evidence_digest=$4
	`, store.tenantID, store.organizationID, recordID, evidenceDigest.Digest).Scan(
		&expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return QualificationEnvelope{}, ErrNotFound
	}
	if err != nil {
		return QualificationEnvelope{}, fmt.Errorf("commercial capability: load qualification: %w", err)
	}
	locator := QualificationEnvelope{Result: QualificationResult{
		RecordID: recordID, EvidenceDigest: evidenceDigest,
	}}
	opened, err := store.vault.OpenRecord(store.qualificationAD(locator), sealed)
	if err != nil || digestBytes(opened).Digest != expectedHash {
		return QualificationEnvelope{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[QualificationEnvelope, *QualificationEnvelope](opened)
	if err != nil || value.Result.RecordID != recordID || value.Result.EvidenceDigest != evidenceDigest {
		return QualificationEnvelope{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) resolveCurrentSeatKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
	keyID string,
	now time.Time,
) (ed25519.PublicKey, string, contracts.DepartmentID, error) {
	var publicKey []byte
	var role string
	var departmentID contracts.DepartmentID
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,seat.seat_role,seat.department_id
		FROM workforce_mail_keys key
		JOIN workforce_organization_seats seat
		  ON seat.tenant_id=key.tenant_id
		 AND seat.organization_id=key.organization_id
		 AND seat.seat_id=key.seat_id
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		  AND seat.active=true
		FOR SHARE OF key,seat
	`, store.tenantID, store.organizationID, seatID, keyID, now).Scan(&publicKey, &role, &departmentID)
	if errors.Is(err, pgx.ErrNoRows) || len(publicKey) != ed25519.PublicKeySize {
		return nil, "", "", ErrUnauthorized
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("commercial capability: resolve current seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), role, departmentID, nil
}

func (store *Store) resolveHistoricalSeatKey(
	ctx context.Context,
	seatID contracts.SeatID,
	keyID string,
	at time.Time,
) (ed25519.PublicKey, error) {
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
		return nil, fmt.Errorf("commercial capability: resolve historical seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("commercial capability: time source must return UTC")
	}
	return now, nil
}

func (store *Store) recordAD(value RecordBody) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.commercial-capability.record",
		Stream: strings.Join([]string{
			string(store.organizationID), string(value.ChainID), string(value.ID),
			fmt.Sprintf("%d", value.Version),
		}, "/"),
		Schema: SchemaVersion,
	}
}

func (store *Store) checkpointAD(value Checkpoint) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.commercial-capability.checkpoint",
		Stream: strings.Join([]string{
			string(store.organizationID), string(value.InitiativeID), string(value.WorkflowID),
			string(value.ID), fmt.Sprintf("%d", value.Version),
		}, "/"),
		Schema: CheckpointSchemaVersion,
	}
}

func (store *Store) qualificationAD(value QualificationEnvelope) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.commercial-capability.qualification",
		Stream: strings.Join([]string{
			string(store.organizationID), string(value.Result.RecordID),
			value.Result.EvidenceDigest.Digest,
		}, "/"),
		Schema: SchemaVersion,
	}
}

func recordBoundaryHashes(body RecordBody) (*contracts.ContentHash, *contracts.ContentHash, error) {
	var customerHash *contracts.ContentHash
	var economicHash *contracts.ContentHash
	if body.Customer != nil {
		hash, err := CustomerBoundaryHash(*body.Customer)
		if err != nil {
			return nil, nil, err
		}
		customerHash = &hash
	}
	if body.Economic != nil {
		hash, err := EconomicBoundaryHash(*body.Economic)
		if err != nil {
			return nil, nil, err
		}
		economicHash = &hash
	}
	return customerHash, economicHash, nil
}

func optionalRecordID(value *RecordID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func nullableDigest(value *contracts.ContentHash) any {
	if value == nil {
		return nil
	}
	return value.Digest
}
