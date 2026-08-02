package companystate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

const (
	CorrectionSchemaVersion     = "workforce.company-state.correction.v1"
	ReconciliationSchemaVersion = "workforce.company-state.reconciliation.v1"
)

type CorrectionBody struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               contracts.CorrectionID   `json:"correction_id"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	Target           RecordReference          `json:"target"`
	Replacement      *RecordReference         `json:"replacement"`
	Reason           string                   `json:"reason"`
	MateriallyUnsafe bool                     `json:"materially_unsafe"`
	Evidence         []EvidenceReference      `json:"evidence"`
	AuthorSeatID     contracts.SeatID         `json:"author_seat_id"`
	EffectiveAt      time.Time                `json:"effective_at"`
}

func (value CorrectionBody) Validate() error {
	if value.SchemaVersion != CorrectionSchemaVersion {
		return fmt.Errorf("company state: unsupported correction schema %q", value.SchemaVersion)
	}
	if err := validateID("correction_id", string(value.ID)); err != nil {
		return err
	}
	if err := validateID("organization_id", string(value.OrganizationID)); err != nil {
		return err
	}
	if err := value.Target.Validate(); err != nil {
		return err
	}
	if value.Replacement != nil {
		if err := value.Replacement.Validate(); err != nil {
			return err
		}
		if value.Target.ID == value.Replacement.ID && value.Target.Version == value.Replacement.Version {
			return fmt.Errorf("company state: correction replacement cannot equal its target")
		}
	}
	if err := validateText("correction reason", value.Reason, 4096); err != nil {
		return err
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("company state: correction requires 1 to 256 evidence references")
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("company state: correction evidence %d: %w", index, err)
		}
		if index > 0 && string(value.Evidence[index-1].ID) >= string(value.Evidence[index].ID) {
			return fmt.Errorf("company state: correction evidence must be sorted and unique")
		}
	}
	if err := validateID("correction author_seat_id", string(value.AuthorSeatID)); err != nil {
		return err
	}
	if !validUTC(value.EffectiveAt) {
		return fmt.Errorf("company state: correction effective_at must be non-zero UTC")
	}
	return nil
}

type Correction struct {
	Body        CorrectionBody        `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value Correction) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func SignCorrection(value *Correction, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("company state: correction is required")
	}
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := validateSigningAuthority(keyID, privateKey); err != nil {
		return err
	}
	contentHash, err := hashCorrectionBody(value.Body)
	if err != nil {
		return err
	}
	value.ContentHash = contentHash
	value.Signature = signaturePlaceholder(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return nil
}

func VerifyCorrection(value Correction, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	contentHash, err := hashCorrectionBody(value.Body)
	if err != nil || contentHash != value.ContentHash {
		return ErrIntegrity
	}
	return verifySignedCanonical(value.Signature, publicKey, func() ([]byte, error) {
		prepared := value
		prepared.Signature = signaturePlaceholder(value.Signature.KeyID)
		return contracts.EncodeCanonical(&prepared)
	})
}

type ReconciliationOutcome string

const (
	ReconciliationClosed    ReconciliationOutcome = "reconciled"
	ReconciliationEscalated ReconciliationOutcome = "escalated"
)

type ReconciliationBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"reconciliation_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	CorrectionID   contracts.CorrectionID   `json:"correction_id"`
	Outcome        ReconciliationOutcome    `json:"outcome"`
	Resolution     *RecordReference         `json:"resolution"`
	Evidence       []EvidenceReference      `json:"evidence"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	EffectiveAt    time.Time                `json:"effective_at"`
}

func (value ReconciliationBody) Validate() error {
	if value.SchemaVersion != ReconciliationSchemaVersion {
		return fmt.Errorf("company state: unsupported reconciliation schema %q", value.SchemaVersion)
	}
	if err := validateID("reconciliation_id", value.ID); err != nil {
		return err
	}
	if err := validateID("organization_id", string(value.OrganizationID)); err != nil {
		return err
	}
	if err := validateID("correction_id", string(value.CorrectionID)); err != nil {
		return err
	}
	if value.Outcome != ReconciliationClosed && value.Outcome != ReconciliationEscalated {
		return fmt.Errorf("company state: reconciliation outcome is invalid")
	}
	if value.Outcome == ReconciliationClosed && value.Resolution == nil {
		return fmt.Errorf("company state: closed reconciliation requires a resolution record")
	}
	if value.Resolution != nil {
		if err := value.Resolution.Validate(); err != nil {
			return err
		}
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("company state: reconciliation requires 1 to 256 evidence references")
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return err
		}
		if index > 0 && string(value.Evidence[index-1].ID) >= string(value.Evidence[index].ID) {
			return fmt.Errorf("company state: reconciliation evidence must be sorted and unique")
		}
	}
	if err := validateID("reconciliation author_seat_id", string(value.AuthorSeatID)); err != nil {
		return err
	}
	if !validUTC(value.EffectiveAt) {
		return fmt.Errorf("company state: reconciliation effective_at must be non-zero UTC")
	}
	return nil
}

type Reconciliation struct {
	Body        ReconciliationBody    `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value Reconciliation) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func SignReconciliation(value *Reconciliation, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("company state: reconciliation is required")
	}
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := validateSigningAuthority(keyID, privateKey); err != nil {
		return err
	}
	contentHash, err := hashReconciliationBody(value.Body)
	if err != nil {
		return err
	}
	value.ContentHash = contentHash
	value.Signature = signaturePlaceholder(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return nil
}

func VerifyReconciliation(value Reconciliation, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	contentHash, err := hashReconciliationBody(value.Body)
	if err != nil || contentHash != value.ContentHash {
		return ErrIntegrity
	}
	return verifySignedCanonical(value.Signature, publicKey, func() ([]byte, error) {
		prepared := value
		prepared.Signature = signaturePlaceholder(value.Signature.KeyID)
		return contracts.EncodeCanonical(&prepared)
	})
}

func (store *Store) ApplyCorrection(ctx context.Context, value Correction) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := value.Validate(); err != nil {
		return false, err
	}
	if value.Body.OrganizationID != store.organizationID || value.Body.EffectiveAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.correctionAD(value.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("company state: seal correction: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("company state: begin correction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return false, err
	}
	if err := store.requireSchemaTx(ctx, tx, StoreSchemaVersion, "active"); err != nil {
		return false, err
	}
	publicKey, err := store.resolveSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || VerifyCorrection(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_company_state_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
	`, store.tenantID, store.organizationID, value.Body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("company state: commit correction replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("company state: inspect correction replay: %w", err)
	}
	references := []RecordReference{value.Body.Target}
	if value.Body.Replacement != nil {
		references = append(references, *value.Body.Replacement)
	}
	if _, err := store.validateReferencesTx(ctx, tx, references); err != nil {
		return false, err
	}
	var replacementID any
	var replacementVersion any
	if value.Body.Replacement != nil {
		replacementID = value.Body.Replacement.ID
		replacementVersion = value.Body.Replacement.Version
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_state_corrections (
			tenant_id,organization_id,correction_id,target_record_id,target_version,
			replacement_record_id,replacement_version,materially_unsafe,content_hash,
			canonical_hash,signature_key_id,sealed_correction,effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Target.ID,
		value.Body.Target.Version, replacementID, replacementVersion,
		value.Body.MateriallyUnsafe, value.ContentHash.Digest, canonicalHash,
		value.Signature.KeyID, sealed, value.Body.EffectiveAt, now)
	if err != nil {
		return false, fmt.Errorf("company state: insert correction: %w", err)
	}
	_, err = tx.Exec(ctx, `
		WITH RECURSIVE affected(record_id,record_version,depth) AS (
			SELECT $4::text,$5::bigint,0
			UNION
			SELECT edge.consumer_record_id,edge.consumer_version,affected.depth+1
			FROM workforce_company_state_derivations edge
			JOIN affected
			  ON edge.source_record_id=affected.record_id
			 AND edge.source_version=affected.record_version
			WHERE edge.tenant_id=$1 AND edge.organization_id=$2 AND affected.depth<4096
		), minimum_depth AS (
			SELECT record_id,record_version,MIN(depth) AS depth
			FROM affected GROUP BY record_id,record_version
		)
		INSERT INTO workforce_company_state_contamination (
			tenant_id,organization_id,correction_id,affected_record_id,
			affected_version,derivation_depth,materially_unsafe,state,contaminated_at
		)
		SELECT $1,$2,$3,record_id,record_version,depth,$6,'open',$7
		FROM minimum_depth
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Target.ID,
		value.Body.Target.Version, value.Body.MateriallyUnsafe, now)
	if err != nil {
		return false, fmt.Errorf("company state: propagate correction contamination: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("company state: commit correction: %w", err)
	}
	return false, nil
}

func (store *Store) Reconcile(ctx context.Context, value Reconciliation) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := value.Validate(); err != nil {
		return false, err
	}
	if value.Body.OrganizationID != store.organizationID || value.Body.EffectiveAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.reconciliationAD(value.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("company state: seal reconciliation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("company state: begin reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return false, err
	}
	if err := store.requireSchemaTx(ctx, tx, StoreSchemaVersion, "active"); err != nil {
		return false, err
	}
	publicKey, err := store.resolveSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || VerifyReconciliation(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_company_state_reconciliations
		WHERE tenant_id=$1 AND organization_id=$2 AND reconciliation_id=$3
	`, store.tenantID, store.organizationID, value.Body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("company state: commit reconciliation replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("company state: inspect reconciliation replay: %w", err)
	}
	var correctionExists bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_company_state_corrections
			WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
			FOR UPDATE
		)
	`, store.tenantID, store.organizationID, value.Body.CorrectionID).Scan(&correctionExists)
	if err != nil || !correctionExists {
		return false, ErrConflict
	}
	var resolutionID any
	var resolutionVersion any
	if value.Body.Resolution != nil {
		contaminated, err := store.validateReferencesTx(ctx, tx, []RecordReference{*value.Body.Resolution})
		if err != nil {
			return false, err
		}
		if value.Body.Outcome == ReconciliationClosed && contaminated {
			return false, ErrReconciliationRequired
		}
		resolutionID = value.Body.Resolution.ID
		resolutionVersion = value.Body.Resolution.Version
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_state_reconciliations (
			tenant_id,organization_id,reconciliation_id,correction_id,outcome,
			resolution_record_id,resolution_version,content_hash,canonical_hash,
			signature_key_id,sealed_reconciliation,effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.CorrectionID,
		value.Body.Outcome, resolutionID, resolutionVersion, value.ContentHash.Digest,
		canonicalHash, value.Signature.KeyID, sealed, value.Body.EffectiveAt, now)
	if err != nil {
		return false, fmt.Errorf("company state: insert reconciliation: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_company_state_contamination
		SET state=$4,resolved_at=$5,resolution_record_id=$6,resolution_version=$7
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3 AND state='open'
	`, store.tenantID, store.organizationID, value.Body.CorrectionID,
		value.Body.Outcome, now, resolutionID, resolutionVersion)
	if err != nil {
		return false, fmt.Errorf("company state: close contamination: %w", err)
	}
	if command.RowsAffected() == 0 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("company state: commit reconciliation: %w", err)
	}
	return false, nil
}

func hashCorrectionBody(value CorrectionBody) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func hashReconciliationBody(value ReconciliationBody) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func verifySignedCanonical(
	signature contracts.Signature,
	publicKey ed25519.PublicKey,
	payload func() ([]byte, error),
) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return ErrUnauthorized
	}
	canonical, err := payload()
	if err != nil || !ed25519.Verify(publicKey, canonical, decoded) {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) correctionAD(id contracts.CorrectionID) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-state.correction",
		Stream: string(store.organizationID) + "/" + string(id), Schema: CorrectionSchemaVersion,
	}
}

func (store *Store) reconciliationAD(id string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-state.reconciliation",
		Stream: string(store.organizationID) + "/" + id, Schema: ReconciliationSchemaVersion,
	}
}
