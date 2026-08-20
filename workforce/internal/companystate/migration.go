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
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type LegacyKind string

const (
	LegacyOrganization LegacyKind = "organization"
	LegacyDepartment   LegacyKind = "department"
	LegacySeat         LegacyKind = "seat"
	LegacyMandate      LegacyKind = "mandate"
	LegacyWorkOrder    LegacyKind = "work_order"
	LegacyGraphNode    LegacyKind = "graph_node"
	LegacyMailRecord   LegacyKind = "mail_record"
	LegacyPolicy       LegacyKind = "policy"
	LegacyApproval     LegacyKind = "approval"
	LegacyLeaseHistory LegacyKind = "lease_history"
	LegacyEffect       LegacyKind = "effect"
	LegacyEvidence     LegacyKind = "evidence"
	LegacyReceipt      LegacyKind = "receipt"
	LegacyCorrection   LegacyKind = "correction"
	LegacyProjectBrain LegacyKind = "project_brain"
)

func AllLegacyKinds() []LegacyKind {
	return []LegacyKind{
		LegacyOrganization, LegacyDepartment, LegacySeat, LegacyMandate,
		LegacyWorkOrder, LegacyGraphNode, LegacyMailRecord, LegacyPolicy,
		LegacyApproval, LegacyLeaseHistory, LegacyEffect, LegacyEvidence,
		LegacyReceipt, LegacyCorrection, LegacyProjectBrain,
	}
}

func (value LegacyKind) Valid() bool {
	for _, candidate := range AllLegacyKinds() {
		if value == candidate {
			return true
		}
	}
	return false
}

type MigrationEntry struct {
	LegacyKind    LegacyKind            `json:"legacy_kind"`
	CanonicalID   string                `json:"canonical_id"`
	SourceHash    contracts.ContentHash `json:"source_hash"`
	ProjectedHash contracts.ContentHash `json:"projected_hash"`
	Projection    string                `json:"projection"`
}

func (value MigrationEntry) Validate() error {
	if !value.LegacyKind.Valid() {
		return fmt.Errorf("company state: invalid legacy kind %q", value.LegacyKind)
	}
	if err := validateID("migration canonical_id", value.CanonicalID); err != nil {
		return err
	}
	if err := value.SourceHash.Validate(); err != nil {
		return err
	}
	if err := value.ProjectedHash.Validate(); err != nil {
		return err
	}
	if value.Projection != "preserve" || value.SourceHash != value.ProjectedHash {
		return fmt.Errorf("company state: v1 projection must preserve canonical identity and hash without reinterpretation")
	}
	return nil
}

type MigrationManifestBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"manifest_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	OwnerID        contracts.OwnerID        `json:"owner_id"`
	SourceVersion  string                   `json:"source_version"`
	TargetVersion  string                   `json:"target_version"`
	Entries        []MigrationEntry         `json:"entries"`
	CreatedAt      time.Time                `json:"created_at"`
}

func (value MigrationManifestBody) Validate() error {
	if value.SchemaVersion != MigrationManifestSchemaVersion ||
		value.SourceVersion != LegacyStoreSchemaVersion || value.TargetVersion != StoreSchemaVersion {
		return fmt.Errorf("company state: migration schema versions are incompatible")
	}
	if err := validateID("manifest_id", value.ID); err != nil {
		return err
	}
	if err := validateID("organization_id", string(value.OrganizationID)); err != nil {
		return err
	}
	if err := validateID("owner_id", string(value.OwnerID)); err != nil {
		return err
	}
	if len(value.Entries) == 0 || len(value.Entries) > 1_000_000 {
		return fmt.Errorf("company state: migration manifest must contain 1 to 1000000 entries")
	}
	present := make(map[LegacyKind]bool, len(AllLegacyKinds()))
	for index := range value.Entries {
		if err := value.Entries[index].Validate(); err != nil {
			return fmt.Errorf("company state: migration entry %d: %w", index, err)
		}
		present[value.Entries[index].LegacyKind] = true
		if index > 0 && migrationEntryKey(value.Entries[index-1]) >= migrationEntryKey(value.Entries[index]) {
			return fmt.Errorf("company state: migration entries must be sorted and unique by legacy kind and canonical identity")
		}
	}
	for _, required := range []LegacyKind{LegacyOrganization, LegacyDepartment, LegacySeat, LegacyMandate} {
		if !present[required] {
			return fmt.Errorf("company state: migration manifest is missing required %s inventory", required)
		}
	}
	if !validUTC(value.CreatedAt) {
		return fmt.Errorf("company state: migration created_at must be non-zero UTC")
	}
	return nil
}

type MigrationManifest struct {
	Body        MigrationManifestBody `json:"body"`
	ContentHash contracts.ContentHash `json:"content_hash"`
	Signature   contracts.Signature   `json:"signature"`
}

func (value MigrationManifest) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func SignMigrationManifest(value *MigrationManifest, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("company state: migration manifest is required")
	}
	if err := value.Body.Validate(); err != nil {
		return err
	}
	if err := validateSigningAuthority(keyID, privateKey); err != nil {
		return err
	}
	contentHash, err := hashMigrationBody(value.Body)
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

func VerifyMigrationManifest(value MigrationManifest, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	contentHash, err := hashMigrationBody(value.Body)
	if err != nil || contentHash != value.ContentHash {
		return ErrIntegrity
	}
	return verifySignedCanonical(value.Signature, publicKey, func() ([]byte, error) {
		prepared := value
		prepared.Signature = signaturePlaceholder(value.Signature.KeyID)
		return contracts.EncodeCanonical(&prepared)
	})
}

func (store *Store) StageV1Migration(ctx context.Context, manifest MigrationManifest) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := manifest.Validate(); err != nil {
		return false, err
	}
	if manifest.Body.OrganizationID != store.organizationID || manifest.Body.CreatedAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&manifest)
	if err != nil {
		return false, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.migrationAD(manifest.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("company state: seal migration manifest: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("company state: begin migration staging: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return false, err
	}
	publicKey, err := store.resolveOwnerKeyTx(ctx, tx, manifest.Body.OwnerID, manifest.Signature.KeyID)
	if err != nil || VerifyMigrationManifest(manifest, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_company_state_migration_manifests
		WHERE tenant_id=$1 AND organization_id=$2 AND manifest_id=$3
	`, store.tenantID, store.organizationID, manifest.Body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash {
			return false, ErrConflict
		}
		command, stageErr := tx.Exec(ctx, `
			UPDATE workforce_company_state_schema
			SET state='staged',staged_manifest_id=$3,updated_at=$4
			WHERE tenant_id=$1 AND organization_id=$2
			  AND active_version=$5 AND state='active' AND staged_manifest_id IS NULL
		`, store.tenantID, store.organizationID, manifest.Body.ID, now, LegacyStoreSchemaVersion)
		if stageErr != nil {
			return false, fmt.Errorf("company state: restage migration manifest: %w", stageErr)
		}
		if command.RowsAffected() == 0 {
			var activeVersion, state string
			var stagedManifestID *string
			if stageErr := tx.QueryRow(ctx, `
				SELECT active_version,state,staged_manifest_id
				FROM workforce_company_state_schema
				WHERE tenant_id=$1 AND organization_id=$2
				FOR UPDATE
			`, store.tenantID, store.organizationID).Scan(&activeVersion, &state, &stagedManifestID); stageErr != nil {
				return false, fmt.Errorf("company state: inspect replayed migration state: %w", stageErr)
			}
			if activeVersion != LegacyStoreSchemaVersion || state != "staged" ||
				stagedManifestID == nil || *stagedManifestID != manifest.Body.ID {
				return false, ErrSchemaMismatch
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("company state: commit migration replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("company state: inspect migration replay: %w", err)
	}
	var companyStateRows int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_company_state_records
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, store.organizationID).Scan(&companyStateRows); err != nil {
		return false, fmt.Errorf("company state: inspect pre-migration records: %w", err)
	}
	if companyStateRows != 0 {
		return false, ErrSchemaMismatch
	}
	var activeVersion, state string
	var stagedID *string
	err = tx.QueryRow(ctx, `
		SELECT active_version,state,staged_manifest_id
		FROM workforce_company_state_schema
		WHERE tenant_id=$1 AND organization_id=$2
		FOR UPDATE
	`, store.tenantID, store.organizationID).Scan(&activeVersion, &state, &stagedID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_company_state_schema (
				tenant_id,organization_id,active_version,state,staged_manifest_id,activated_at,updated_at
			) VALUES ($1,$2,$3,'staged',$4,NULL,$5)
		`, store.tenantID, store.organizationID, LegacyStoreSchemaVersion, manifest.Body.ID, now)
	case err != nil:
		return false, fmt.Errorf("company state: inspect migration schema: %w", err)
	case activeVersion != LegacyStoreSchemaVersion || state == "staged" && (stagedID == nil || *stagedID != manifest.Body.ID):
		return false, ErrSchemaMismatch
	default:
		_, err = tx.Exec(ctx, `
			UPDATE workforce_company_state_schema
			SET state='staged',staged_manifest_id=$3,updated_at=$4
			WHERE tenant_id=$1 AND organization_id=$2
			  AND active_version=$5 AND state='active'
		`, store.tenantID, store.organizationID, manifest.Body.ID, now, LegacyStoreSchemaVersion)
	}
	if err != nil {
		return false, fmt.Errorf("company state: stage migration schema: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_state_migration_manifests (
			tenant_id,organization_id,manifest_id,source_version,target_version,
			entry_count,content_hash,canonical_hash,signature_key_id,sealed_manifest,staged_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, store.tenantID, store.organizationID, manifest.Body.ID,
		manifest.Body.SourceVersion, manifest.Body.TargetVersion, len(manifest.Body.Entries),
		manifest.ContentHash.Digest, canonicalHash, manifest.Signature.KeyID, sealed, now)
	if err != nil {
		return false, fmt.Errorf("company state: insert migration manifest: %w", err)
	}
	for index := range manifest.Body.Entries {
		entry := manifest.Body.Entries[index]
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_migration_entries (
				tenant_id,organization_id,manifest_id,ordinal,legacy_kind,
				canonical_id,source_hash,projected_hash,projection
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, manifest.Body.ID, index+1,
			entry.LegacyKind, entry.CanonicalID, entry.SourceHash.Digest,
			entry.ProjectedHash.Digest, entry.Projection)
		if err != nil {
			return false, fmt.Errorf("company state: insert migration entry %d: %w", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("company state: commit migration staging: %w", err)
	}
	return false, nil
}

func (store *Store) ActivateV1Migration(
	ctx context.Context,
	manifestID string,
	expectedContentHash contracts.ContentHash,
) error {
	if err := validateID("manifest_id", manifestID); err != nil {
		return err
	}
	if err := expectedContentHash.Validate(); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("company state: begin migration activation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return err
	}
	var contentHash string
	var expectedCount, actualCount int64
	err = tx.QueryRow(ctx, `
		SELECT manifest.content_hash,manifest.entry_count,COUNT(entry.ordinal)
		FROM workforce_company_state_migration_manifests manifest
		JOIN workforce_company_state_migration_entries entry
		  ON entry.tenant_id=manifest.tenant_id AND entry.organization_id=manifest.organization_id
		 AND entry.manifest_id=manifest.manifest_id
		WHERE manifest.tenant_id=$1 AND manifest.organization_id=$2 AND manifest.manifest_id=$3
		  AND manifest.source_version=$4 AND manifest.target_version=$5
		  AND entry.source_hash=entry.projected_hash AND entry.projection='preserve'
		GROUP BY manifest.content_hash,manifest.entry_count
	`, store.tenantID, store.organizationID, manifestID, LegacyStoreSchemaVersion,
		StoreSchemaVersion).Scan(&contentHash, &expectedCount, &actualCount)
	if err != nil || contentHash != expectedContentHash.Digest || expectedCount != actualCount {
		return ErrIntegrity
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_company_state_schema
		SET active_version=$4,state='active',staged_manifest_id=NULL,
		    activated_at=$5,updated_at=$5
		WHERE tenant_id=$1 AND organization_id=$2 AND staged_manifest_id=$3
		  AND active_version=$6 AND state='staged'
	`, store.tenantID, store.organizationID, manifestID, StoreSchemaVersion,
		now, LegacyStoreSchemaVersion)
	if err != nil {
		return fmt.Errorf("company state: activate migration schema: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrSchemaMismatch
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_state_migration_activations (
			tenant_id,organization_id,manifest_id,activated_at
		) VALUES ($1,$2,$3,$4)
	`, store.tenantID, store.organizationID, manifestID, now)
	if err != nil {
		return fmt.Errorf("company state: record migration activation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("company state: commit migration activation: %w", err)
	}
	return nil
}

func (store *Store) RollbackStagedMigration(
	ctx context.Context,
	manifestID string,
	expectedContentHash contracts.ContentHash,
) error {
	if err := validateID("manifest_id", manifestID); err != nil {
		return err
	}
	if err := expectedContentHash.Validate(); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("company state: begin migration rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return err
	}
	var hash string
	err = tx.QueryRow(ctx, `
		SELECT content_hash FROM workforce_company_state_migration_manifests
		WHERE tenant_id=$1 AND organization_id=$2 AND manifest_id=$3
	`, store.tenantID, store.organizationID, manifestID).Scan(&hash)
	if err != nil || hash != expectedContentHash.Digest {
		return ErrIntegrity
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_company_state_schema
		SET state='active',staged_manifest_id=NULL,updated_at=$4
		WHERE tenant_id=$1 AND organization_id=$2 AND staged_manifest_id=$3
		  AND active_version=$5 AND state='staged' AND activated_at IS NULL
	`, store.tenantID, store.organizationID, manifestID, now, LegacyStoreSchemaVersion)
	if err != nil {
		return fmt.Errorf("company state: rollback staged migration: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrSchemaMismatch
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("company state: commit migration rollback: %w", err)
	}
	return nil
}

func (store *Store) resolveOwnerKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	ownerID contracts.OwnerID,
	keyID string,
) (ed25519.PublicKey, error) {
	var publicKey []byte
	err := tx.QueryRow(ctx, `
		SELECT public_key FROM workforce_owner_control_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND owner_id=$3 AND key_id=$4
		  AND revoked_at IS NULL
		FOR SHARE
	`, store.tenantID, store.organizationID, ownerID, keyID).Scan(&publicKey)
	if errors.Is(err, pgx.ErrNoRows) || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("company state: resolve migration owner key: %w", err)
	}
	return ed25519.PublicKey(publicKey), nil
}

func hashMigrationBody(value MigrationManifestBody) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func migrationEntryKey(value MigrationEntry) string {
	return string(value.LegacyKind) + "/" + value.CanonicalID
}

func (store *Store) migrationAD(id string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-state.migration",
		Stream: string(store.organizationID) + "/" + id, Schema: MigrationManifestSchemaVersion,
	}
}
