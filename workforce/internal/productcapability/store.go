package productcapability

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/contracts"
)

var (
	// ErrConflict means an immutable identity or optimistic version disagrees.
	ErrConflict = errors.New("product capability: immutable conflict")
	// ErrNotFound means the requested durable record does not exist.
	ErrNotFound = errors.New("product capability: record not found")
	// ErrUnauthorized means current seat or key authority cannot be proven.
	ErrUnauthorized = errors.New("product capability: unauthorized")
	// ErrIntegrity means sealed bytes, hashes, or signatures are inconsistent.
	ErrIntegrity = errors.New("product capability: integrity failure")
	// ErrExpired means evidence is no longer fresh enough for a live gate.
	ErrExpired = errors.New("product capability: evidence expired")
)

// Store owns tenant-scoped, Vault-separated, immutable product capability
// records and restart checkpoints.
type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
}

// NewStore constructs a fail-closed durable product capability store.
func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" ||
		organizationID == "" || now == nil {
		return nil, fmt.Errorf("product capability: PostgreSQL, Vault, tenant, organization, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("product capability: Vault user does not match tenant")
	}
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return nil, err
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, now: now,
	}, nil
}

// Commit verifies current author and Auditor keys, signatures, lineage, and
// gate bindings before atomically appending one immutable verified record.
// The boolean is true only for an exact idempotent replay.
func (store *Store) Commit(ctx context.Context, value VerifiedRecord) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := value.ValidateAt(now); err != nil {
		if strings.Contains(err.Error(), "expired") {
			return false, ErrExpired
		}
		return false, err
	}
	body := value.Record.Body
	if body.OrganizationID != store.organizationID || body.EffectiveAt.After(now) ||
		value.Verification.VerifiedAt.After(now) {
		return false, ErrUnauthorized
	}
	bindings, err := LifecycleBindings(value, now)
	if err != nil && body.Kind != RecordMetricDefinition {
		return false, err
	}
	canonical, err := canonicalBytes(value)
	if err != nil {
		return false, err
	}
	canonicalDigest := sha256Digest(canonical)
	sealed, err := store.vault.SealRecord(store.recordAD(body), canonical)
	if err != nil {
		return false, fmt.Errorf("product capability: seal record: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("product capability: begin record commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lock := strings.Join([]string{
		store.tenantID, string(store.organizationID), string(body.ChainID),
	}, "|")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lock); err != nil {
		return false, fmt.Errorf("product capability: lock record chain: %w", err)
	}
	authorKey, authorRole, err := store.resolveCurrentSeatKeyTx(
		ctx, tx, body.AuthorSeatID, value.Record.Signature.KeyID, now,
	)
	if err != nil || authorRole == "auditor" {
		return false, ErrUnauthorized
	}
	verifierKey, verifierRole, err := store.resolveCurrentSeatKeyTx(
		ctx, tx, value.Verification.VerifierSeatID,
		value.Verification.Signature.KeyID, now,
	)
	if err != nil || verifierRole != "auditor" {
		return false, ErrUnauthorized
	}
	if err := verifyRecord(value.Record, authorKey); err != nil {
		return false, ErrIntegrity
	}
	if err := verifyVerification(value.Verification, verifierKey); err != nil {
		return false, ErrIntegrity
	}
	if body.Handoff != nil {
		if err := store.verifyCompanyStateBindingsTx(ctx, tx, *body.Handoff, now); err != nil {
			return false, err
		}
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_product_capability_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalDigest.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("product capability: commit record replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("product capability: inspect record identity: %w", err)
	}
	var headID string
	var headVersion uint64
	var headKind RecordKind
	var headInitiative InitiativeID
	var headProject contracts.ProjectID
	var headWorkspace contracts.WorkspaceID
	err = tx.QueryRow(ctx, `
		SELECT head.record_id,head.version,head.kind,
		       record.initiative_id,record.project_id,record.workspace_id
		FROM workforce_product_capability_heads head
		JOIN workforce_product_capability_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.record_id=head.record_id
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.chain_id=$3
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, body.ChainID).Scan(
		&headID, &headVersion, &headKind, &headInitiative, &headProject, &headWorkspace,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (body.Version != 1 || body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (body.Version != headVersion+1 || body.Supersedes == nil ||
		string(*body.Supersedes) != headID || body.Kind != headKind ||
		body.InitiativeID != headInitiative || body.ProjectID != headProject ||
		body.WorkspaceID != headWorkspace):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("product capability: inspect record head: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_product_capability_records (
			tenant_id,organization_id,chain_id,record_id,version,kind,
			initiative_id,project_id,workspace_id,author_seat_id,verifier_seat_id,
			supersedes,record_hash,canonical_hash,author_key_id,verifier_key_id,
			sealed_record,created_at,effective_at,fresh_until,verified_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21
		)
	`, store.tenantID, store.organizationID, body.ChainID, body.ID,
		body.Version, body.Kind, body.InitiativeID, body.ProjectID, body.WorkspaceID,
		body.AuthorSeatID, value.Verification.VerifierSeatID, optionalRecordID(body.Supersedes),
		value.Verification.RecordHash.Digest, canonicalDigest.Digest,
		value.Record.Signature.KeyID, value.Verification.Signature.KeyID,
		sealed, body.CreatedAt, body.EffectiveAt, body.FreshUntil,
		value.Verification.VerifiedAt)
	if err != nil {
		return false, fmt.Errorf("product capability: insert record: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_product_capability_heads (
			tenant_id,organization_id,chain_id,record_id,version,kind,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id,organization_id,chain_id) DO UPDATE SET
			record_id=EXCLUDED.record_id,version=EXCLUDED.version,
			kind=EXCLUDED.kind,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, body.ChainID, body.ID,
		body.Version, body.Kind, now)
	if err != nil {
		return false, fmt.Errorf("product capability: update record head: %w", err)
	}
	for _, binding := range bindings {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_product_capability_gate_bindings (
				tenant_id,organization_id,record_id,record_version,initiative_id,
				evidence_id,evidence_kind,evidence_hash,fresh_until,verdict_id,
				verdict_hash,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, store.tenantID, store.organizationID, body.ID, body.Version,
			body.InitiativeID, binding.ID, binding.Kind, binding.EvidenceHash.Digest,
			binding.FreshUntil, *binding.IndependentVerdictID,
			binding.IndependentVerdictHash.Digest, now)
		if err != nil {
			return false, fmt.Errorf("product capability: insert lifecycle binding: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("product capability: commit record: %w", err)
	}
	return false, nil
}

// LoadCurrent opens and verifies the current live record for one exact chain.
func (store *Store) LoadCurrent(ctx context.Context, chainID ChainID) (VerifiedRecord, error) {
	if err := validateToken("chain_id", string(chainID)); err != nil {
		return VerifiedRecord{}, err
	}
	var recordID RecordID
	err := store.pool.QueryRow(ctx, `
		SELECT record_id FROM workforce_product_capability_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND chain_id=$3
	`, store.tenantID, store.organizationID, chainID).Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedRecord{}, ErrNotFound
	}
	if err != nil {
		return VerifiedRecord{}, fmt.Errorf("product capability: load record head: %w", err)
	}
	return store.Load(ctx, recordID)
}

// Load opens, authenticates, decodes, verifies, and freshness-checks one record.
func (store *Store) Load(ctx context.Context, recordID RecordID) (VerifiedRecord, error) {
	return store.load(ctx, recordID, true)
}

// ListCurrent opens the latest immutable record in every capability chain for
// an authenticated operating projection. Historical truth is signature and
// integrity checked even when its freshness window has elapsed; callers must
// render that elapsed freshness rather than silently dropping the record.
func (store *Store) ListCurrent(
	ctx context.Context,
	offset uint64,
	limit int,
) ([]VerifiedRecord, bool, error) {
	if limit <= 0 || limit > 200 || offset > uint64(1<<63-1) {
		return nil, false, fmt.Errorf("product capability: projection page is outside bounds")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT record.record_id
		FROM workforce_product_capability_heads head
		JOIN workforce_product_capability_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.chain_id=head.chain_id
		 AND record.record_id=head.record_id
		 AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY record.verified_at DESC,record.record_id
		LIMIT $3 OFFSET $4
	`, store.tenantID, store.organizationID, limit+1, int64(offset))
	if err != nil {
		return nil, false, fmt.Errorf("product capability: list current records: %w", err)
	}
	defer rows.Close()
	recordIDs := make([]RecordID, 0, limit+1)
	for rows.Next() {
		var recordID RecordID
		if err := rows.Scan(&recordID); err != nil {
			return nil, false, fmt.Errorf("product capability: scan current record: %w", err)
		}
		recordIDs = append(recordIDs, recordID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("product capability: list current records: %w", err)
	}
	rows.Close()
	hasMore := len(recordIDs) > limit
	if hasMore {
		recordIDs = recordIDs[:limit]
	}
	values := make([]VerifiedRecord, 0, len(recordIDs))
	for _, recordID := range recordIDs {
		value, err := store.load(ctx, recordID, false)
		if err != nil {
			return nil, false, fmt.Errorf("product capability: project record %q: %w", recordID, err)
		}
		values = append(values, value)
	}
	return values, hasMore, nil
}

func (store *Store) load(
	ctx context.Context,
	recordID RecordID,
	requireFresh bool,
) (VerifiedRecord, error) {
	if err := validateToken("record_id", string(recordID)); err != nil {
		return VerifiedRecord{}, err
	}
	var chainID ChainID
	var version uint64
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT chain_id,version,canonical_hash,sealed_record
		FROM workforce_product_capability_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, recordID).Scan(
		&chainID, &version, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedRecord{}, ErrNotFound
	}
	if err != nil {
		return VerifiedRecord{}, fmt.Errorf("product capability: load record: %w", err)
	}
	body := RecordBody{ID: recordID, ChainID: chainID, Version: version, OrganizationID: store.organizationID}
	canonical, err := store.vault.OpenRecord(store.recordAD(body), sealed)
	if err != nil || sha256Digest(canonical).Digest != expectedHash {
		return VerifiedRecord{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[VerifiedRecord, *VerifiedRecord](canonical)
	if err != nil || value.Record.Body.ID != recordID ||
		value.Record.Body.ChainID != chainID || value.Record.Body.Version != version ||
		value.Record.Body.OrganizationID != store.organizationID {
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
		ctx, value.Verification.VerifierSeatID, value.Verification.Signature.KeyID,
		value.Verification.VerifiedAt,
	)
	if err != nil || verifyVerification(value.Verification, verifierKey) != nil {
		return VerifiedRecord{}, ErrIntegrity
	}
	validationTime := value.Verification.VerifiedAt
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
	return value, nil
}

// LoadLifecycleBindings opens a current verified record and returns the exact
// closed lifecycle evidence projections derived from its signed artifacts.
func (store *Store) LoadLifecycleBindings(
	ctx context.Context,
	recordID RecordID,
) ([]companylifecycle.EvidenceBinding, error) {
	value, err := store.Load(ctx, recordID)
	if err != nil {
		return nil, err
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	return LifecycleBindings(value, now)
}

// AdvanceCheckpoint atomically advances an optimistic checkpoint version. An
// exact idempotency replay returns the committed checkpoint with replay=true.
func (store *Store) AdvanceCheckpoint(
	ctx context.Context,
	expectedVersion uint64,
	next Checkpoint,
) (current Checkpoint, replay bool, err error) {
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
	if next.UpdatedAt.After(now) || !next.Source.Fresh {
		return Checkpoint{}, false, ErrConflict
	}
	canonical, err := canonicalBytes(next)
	if err != nil {
		return Checkpoint{}, false, err
	}
	digest := sha256Digest(canonical)
	sealed, err := store.vault.SealRecord(store.checkpointAD(next), canonical)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: seal checkpoint: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: begin checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var replayID CheckpointID
	var replayVersion uint64
	var replayHash string
	err = tx.QueryRow(ctx, `
		SELECT checkpoint_id,version,canonical_hash
		FROM workforce_product_capability_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND idempotency_key=$4
	`, store.tenantID, store.organizationID, next.InitiativeID,
		next.IdempotencyKey).Scan(&replayID, &replayVersion, &replayHash)
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
		return Checkpoint{}, false, fmt.Errorf("product capability: inspect checkpoint replay: %w", err)
	}
	var previous Checkpoint
	var previousSealed []byte
	var previousHash string
	var previousID CheckpointID
	err = tx.QueryRow(ctx, `
		SELECT checkpoint_id,version,canonical_hash,sealed_checkpoint
		FROM workforce_product_capability_checkpoint_heads head
		JOIN workforce_product_capability_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.initiative_id=$3
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, next.InitiativeID).Scan(
		&previousID, &previous.Version, &previousHash, &previousSealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if expectedVersion != 0 || next.Version != 1 || next.Phase != PhaseIntake {
			return Checkpoint{}, false, ErrConflict
		}
	} else if err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: load checkpoint head: %w", err)
	} else {
		previous.ID = previousID
		previous.OrganizationID = store.organizationID
		previous.InitiativeID = next.InitiativeID
		opened, openErr := store.vault.OpenRecord(store.checkpointAD(previous), previousSealed)
		if openErr != nil || sha256Digest(opened).Digest != previousHash {
			return Checkpoint{}, false, ErrIntegrity
		}
		previous, err = contracts.DecodeCanonical[Checkpoint, *Checkpoint](opened)
		if err != nil || previous.Version != expectedVersion ||
			!validCheckpointAdvance(previous.Phase, next.Phase) ||
			previous.HandoffID != next.HandoffID || previous.ProjectID != next.ProjectID ||
			previous.WorkspaceID != next.WorkspaceID || !checkpointPreserves(previous, next) {
			return Checkpoint{}, false, ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_product_capability_checkpoints (
			tenant_id,organization_id,initiative_id,checkpoint_id,version,
			handoff_id,project_id,workspace_id,phase,idempotency_key,
			canonical_hash,sealed_checkpoint,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, store.tenantID, store.organizationID, next.InitiativeID, next.ID,
		next.Version, next.HandoffID, next.ProjectID, next.WorkspaceID,
		next.Phase, next.IdempotencyKey, digest.Digest, sealed, next.UpdatedAt)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: insert checkpoint: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_product_capability_checkpoint_heads (
			tenant_id,organization_id,initiative_id,checkpoint_id,version,phase,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id,organization_id,initiative_id) DO UPDATE SET
			checkpoint_id=EXCLUDED.checkpoint_id,version=EXCLUDED.version,
			phase=EXCLUDED.phase,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, next.InitiativeID,
		next.ID, next.Version, next.Phase, next.UpdatedAt)
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: update checkpoint head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Checkpoint{}, false, fmt.Errorf("product capability: commit checkpoint: %w", err)
	}
	return next, false, nil
}

// LoadCheckpoint opens and authenticates the latest durable restart checkpoint.
func (store *Store) LoadCheckpoint(ctx context.Context, initiativeID InitiativeID) (Checkpoint, error) {
	if err := validateToken("initiative_id", string(initiativeID)); err != nil {
		return Checkpoint{}, err
	}
	var id CheckpointID
	var version uint64
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT checkpoint.checkpoint_id,checkpoint.version,
		       checkpoint.canonical_hash,checkpoint.sealed_checkpoint
		FROM workforce_product_capability_checkpoint_heads head
		JOIN workforce_product_capability_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.initiative_id=$3
	`, store.tenantID, store.organizationID, initiativeID).Scan(
		&id, &version, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("product capability: load checkpoint: %w", err)
	}
	stub := Checkpoint{ID: id, Version: version, OrganizationID: store.organizationID, InitiativeID: initiativeID}
	opened, err := store.vault.OpenRecord(store.checkpointAD(stub), sealed)
	if err != nil || sha256Digest(opened).Digest != expectedHash {
		return Checkpoint{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Checkpoint, *Checkpoint](opened)
	if err != nil || value.ID != id || value.Version != version ||
		value.OrganizationID != store.organizationID || value.InitiativeID != initiativeID {
		return Checkpoint{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) resolveCurrentSeatKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
	keyID string,
	now time.Time,
) (ed25519.PublicKey, string, error) {
	var publicKey []byte
	var role string
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,seat.seat_role
		FROM workforce_mail_keys key
		JOIN workforce_organization_seats seat
		  ON seat.tenant_id=key.tenant_id
		 AND seat.organization_id=key.organization_id
		 AND seat.seat_id=key.seat_id
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		  AND seat.active=true
		FOR SHARE OF key,seat
	`, store.tenantID, store.organizationID, seatID, keyID, now).Scan(&publicKey, &role)
	if errors.Is(err, pgx.ErrNoRows) || len(publicKey) != ed25519.PublicKeySize {
		return nil, "", ErrUnauthorized
	}
	if err != nil {
		return nil, "", fmt.Errorf("product capability: resolve current seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), role, nil
}

func (store *Store) verifyCompanyStateBindingsTx(
	ctx context.Context,
	tx pgx.Tx,
	handoff ProductDesignHandoff,
	now time.Time,
) error {
	for _, binding := range []CompanyStateBinding{
		handoff.ProductState, handoff.TargetSegmentState, handoff.ValuePropositionState,
	} {
		var safe bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_company_state_heads head
				WHERE head.tenant_id=$1 AND head.organization_id=$2
				  AND head.record_id=$3 AND head.kind=$4
				  AND head.latest_version=$5 AND head.latest_content_hash=$6
				  AND head.state='active'
				  AND (head.expires_at IS NULL OR head.expires_at>$7)
				  AND NOT EXISTS (
					SELECT 1 FROM workforce_company_state_contamination contamination
					WHERE contamination.tenant_id=head.tenant_id
					  AND contamination.organization_id=head.organization_id
					  AND contamination.affected_record_id=head.record_id
					  AND contamination.affected_version=head.latest_version
					  AND contamination.state='open'
					  AND contamination.materially_unsafe=true
				  )
			)
		`, store.tenantID, store.organizationID, binding.Reference.ID,
			binding.Kind, binding.Reference.Version, binding.Reference.ContentHash.Digest,
			now).Scan(&safe)
		if err != nil {
			return fmt.Errorf("product capability: verify Company State binding: %w", err)
		}
		if !safe {
			return ErrIntegrity
		}
	}
	return nil
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
		return nil, fmt.Errorf("product capability: resolve historical seat key: %w", err)
	}
	return ed25519.PublicKey(publicKey), nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("product capability: time source must return UTC")
	}
	return now, nil
}

func (store *Store) recordAD(value RecordBody) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.product-capability.record",
		Stream: strings.Join([]string{
			string(store.organizationID), string(value.ChainID),
			string(value.ID), fmt.Sprintf("%d", value.Version),
		}, "/"),
		Schema: SchemaVersion,
	}
}

func (store *Store) checkpointAD(value Checkpoint) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.product-capability.checkpoint",
		Stream: strings.Join([]string{
			string(store.organizationID), string(value.InitiativeID),
			string(value.ID), fmt.Sprintf("%d", value.Version),
		}, "/"),
		Schema: CheckpointSchemaVersion,
	}
}

func checkpointPreserves(previous, next Checkpoint) bool {
	if previous.Source.RootDigest != next.Source.RootDigest ||
		previous.Source.GraphDigest != next.Source.GraphDigest ||
		previous.Source.Generation != next.Source.Generation {
		return false
	}
	for _, id := range previous.CompletedRecordIDs {
		if !containsRecordID(next.CompletedRecordIDs, id) {
			return false
		}
	}
	for _, id := range previous.CommittedEffectIDs {
		if !slicesContains(next.CommittedEffectIDs, id) {
			return false
		}
	}
	for _, id := range previous.ReconciledEffectIDs {
		if !slicesContains(next.ReconciledEffectIDs, id) {
			return false
		}
	}
	return true
}

func containsRecordID(values []RecordID, wanted RecordID) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func optionalRecordID(value *RecordID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func sha256Digest(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}
