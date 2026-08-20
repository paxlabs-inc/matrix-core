package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/policy"
)

type MigrationBasis struct {
	LegacyOrganizationVersion uint64
	LegacyOrganizationDigest  contracts.ContentHash
	LegacyAuthoritySetDigest  contracts.ContentHash
	LegacyReceiptSetDigest    contracts.ContentHash
}

type digestSetEnvelope struct {
	SchemaVersion string   `json:"schema_version"`
	Kind          string   `json:"kind"`
	Entries       []string `json:"entries"`
}

func (value digestSetEnvelope) Validate() error {
	if value.SchemaVersion != "workforce.organization-migration-digest-set.v1" {
		return fmt.Errorf("organization: migration digest-set schema is invalid")
	}
	if err := validateID("digest-set kind", value.Kind); err != nil {
		return err
	}
	if len(value.Entries) > 100000 || !slices.IsSorted(value.Entries) {
		return fmt.Errorf("organization: migration digest-set entries are not bounded and sorted")
	}
	for index, entry := range value.Entries {
		if strings.TrimSpace(entry) == "" || len(entry) > 512 {
			return fmt.Errorf("organization: migration digest-set entry is invalid")
		}
		if index > 0 && value.Entries[index-1] == entry {
			return fmt.Errorf("organization: migration digest-set contains duplicates")
		}
	}
	return nil
}

func (store *Store) CurrentMigrationBasis(ctx context.Context) (MigrationBasis, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: begin migration basis read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	basis, err := store.migrationBasisTx(ctx, tx)
	if err != nil {
		return MigrationBasis{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: commit migration basis read: %w", err)
	}
	return basis, nil
}

func (store *Store) StageMigration(
	ctx context.Context,
	value MigrationManifest,
) (bool, error) {
	if value.OrganizationID != store.owner.OrganizationID || value.OwnerID != store.owner.OwnerID ||
		VerifyMigrationManifest(value, store.owner.KeyID, store.owner.PublicKey) != nil {
		return false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.PreparedAt.After(now) || !value.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	template, err := store.LoadTemplate(ctx, value.ToTemplateID, value.ToTemplateVersion)
	if err != nil {
		return false, err
	}
	registry, err := store.LoadRegistry(ctx, template.EffectiveAt)
	if err != nil {
		return false, err
	}
	if err := store.validateTemplate(ctx, template, registry, template.EffectiveAt, false); err != nil {
		return false, err
	}
	if err := validateManifestTemplate(value, template); err != nil {
		return false, err
	}
	canonical, hash, sealed, err := store.prepareMigrationManifest(value)
	if err != nil {
		return false, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("organization: begin migration stage: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return false, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, value.Signature.KeyID, value.PreparedAt,
	); err != nil {
		return false, err
	}
	if err := store.lock(ctx, tx, "migration|"+string(value.ID)); err != nil {
		return false, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_organization_migrations
		WHERE tenant_id=$1 AND organization_id=$2 AND migration_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return false, ErrConflict
		}
		var state MigrationState
		if err := tx.QueryRow(ctx, `
			SELECT state FROM workforce_organization_migration_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND migration_id=$3
		`, store.owner.TenantID, store.owner.OrganizationID, value.ID).Scan(&state); err != nil {
			return false, fmt.Errorf("organization: load replayed migration state: %w", err)
		}
		if state != MigrationStaged {
			return false, ErrMigrationState
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("organization: inspect migration identity: %w", err)
	}
	basis, err := store.migrationBasisTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if basis.LegacyOrganizationVersion != value.FromOrganizationVersion ||
		basis.LegacyOrganizationDigest != value.LegacyOrganizationDigest ||
		basis.LegacyAuthoritySetDigest != value.LegacyAuthoritySetDigest ||
		basis.LegacyReceiptSetDigest != value.LegacyReceiptSetDigest {
		return false, ErrConflict
	}
	if err := store.requireLegacyProjectionTx(ctx, tx, value); err != nil {
		return false, err
	}
	if err := store.validateNoAuthorityWideningTx(
		ctx, tx, template, value.FromOrganizationVersion,
	); err != nil {
		return false, err
	}
	var activeMigration bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_organization_migration_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND state='staged'
		)
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(&activeMigration); err != nil {
		return false, fmt.Errorf("organization: inspect staged migration: %w", err)
	}
	if activeMigration {
		return false, ErrMigrationState
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migrations (
			tenant_id,organization_id,migration_id,version,owner_id,from_template_id,
			from_template_version,to_template_id,to_template_version,
			manifest_digest,capability_registry_digest,canonical_hash,signature_key_id,
			sealed_manifest,prepared_at,activate_not_before,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
		value.OwnerID, value.FromTemplateID, value.FromTemplateVersion,
		value.ToTemplateID, value.ToTemplateVersion, hash,
		value.CapabilityRegistryDigest.Digest, hash, value.Signature.KeyID,
		sealed, value.PreparedAt, value.ActivateNotBefore, value.ExpiresAt, now); err != nil {
		return false, fmt.Errorf("organization: insert migration manifest: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_heads (
			tenant_id,organization_id,migration_id,version,state,updated_at
		) VALUES ($1,$2,$3,$4,'staged',$5)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version, now); err != nil {
		return false, fmt.Errorf("organization: insert migration head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_events (
			tenant_id,organization_id,migration_id,migration_version,event_id,
			event_kind,event_hash,occurred_at
		) VALUES ($1,$2,$3,$4,$5,'staged',$6,$7)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
		"migration-stage:"+string(value.ID)+":"+strconv.FormatUint(value.Version, 10), hash, now); err != nil {
		return false, fmt.Errorf("organization: insert migration stage event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("organization: commit migration stage: %w", err)
	}
	return false, nil
}

type MigrationResult struct {
	MigrationID       MigrationID
	MigrationVersion  uint64
	State             MigrationState
	TemplateID        TemplateID
	TemplateVersion   uint64
	ProjectionVersion uint64
	Deduplicated      bool
}

func (store *Store) ActivateMigration(
	ctx context.Context,
	activation MigrationActivation,
) (MigrationResult, error) {
	if activation.OrganizationID != store.owner.OrganizationID || activation.OwnerID != store.owner.OwnerID ||
		VerifyMigrationActivation(activation, store.owner.KeyID, store.owner.PublicKey) != nil {
		return MigrationResult{}, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return MigrationResult{}, err
	}
	if activation.ActivatedAt.After(now) {
		return MigrationResult{}, ErrUnauthorized
	}
	canonical, activationHash, sealedActivation, err := store.prepareMigrationActivation(activation)
	if err != nil {
		return MigrationResult{}, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MigrationResult{}, fmt.Errorf("organization: begin migration activation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return MigrationResult{}, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, activation.Signature.KeyID, activation.ActivatedAt,
	); err != nil {
		return MigrationResult{}, err
	}
	if err := store.lock(ctx, tx, "migration|"+string(activation.MigrationID)); err != nil {
		return MigrationResult{}, err
	}
	if err := store.lock(ctx, tx, "template-activation"); err != nil {
		return MigrationResult{}, err
	}
	if err := store.lock(ctx, tx, "squad-assignment-plan"); err != nil {
		return MigrationResult{}, err
	}
	manifest, manifestHash, state, err := store.loadMigrationTx(
		ctx, tx, activation.MigrationID, activation.MigrationVersion,
	)
	if err != nil {
		return MigrationResult{}, err
	}
	if manifestHash != activation.ManifestDigest.Digest {
		return MigrationResult{}, ErrConflict
	}
	if state == MigrationActivated {
		var existingHash string
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_organization_migration_activations
			WHERE tenant_id=$1 AND organization_id=$2 AND activation_id=$3
		`, store.owner.TenantID, store.owner.OrganizationID, activation.ID).Scan(&existingHash); err != nil ||
			existingHash != activationHash {
			return MigrationResult{}, ErrMigrationState
		}
		return MigrationResult{
			MigrationID: manifest.ID, MigrationVersion: manifest.Version,
			State: MigrationActivated, TemplateID: manifest.ToTemplateID,
			TemplateVersion:   manifest.ToTemplateVersion,
			ProjectionVersion: activation.ExpectedProjectionVersion + 1,
			Deduplicated:      true,
		}, tx.Commit(ctx)
	}
	if state != MigrationStaged || now.Before(manifest.ActivateNotBefore) || !manifest.ExpiresAt.After(now) ||
		activation.ActivatedAt.Before(manifest.ActivateNotBefore) ||
		activation.ActivatedAt.After(manifest.ExpiresAt) {
		return MigrationResult{}, ErrMigrationState
	}
	template, err := store.loadTemplateQuery(ctx, tx, manifest.ToTemplateID, manifest.ToTemplateVersion)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := validateManifestTemplate(manifest, template); err != nil {
		return MigrationResult{}, err
	}
	registry, err := store.loadRegistryQuery(ctx, tx, now)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := store.validateTemplateQuery(ctx, tx, template, registry, now, false); err != nil {
		return MigrationResult{}, err
	}
	basis, err := store.migrationBasisTx(ctx, tx)
	if err != nil {
		return MigrationResult{}, err
	}
	if basis.LegacyOrganizationVersion != manifest.FromOrganizationVersion ||
		basis.LegacyOrganizationDigest != manifest.LegacyOrganizationDigest ||
		basis.LegacyAuthoritySetDigest != manifest.LegacyAuthoritySetDigest ||
		basis.LegacyReceiptSetDigest != manifest.LegacyReceiptSetDigest {
		return MigrationResult{}, ErrConflict
	}
	if err := store.validateNoAuthorityWideningTx(
		ctx, tx, template, manifest.FromOrganizationVersion,
	); err != nil {
		return MigrationResult{}, err
	}
	var currentTemplateID TemplateID
	var currentTemplateVersion, projectionVersion uint64
	var projectionState string
	err = tx.QueryRow(ctx, `
		SELECT template_id,template_version,organization_v2_version,state
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
		FOR UPDATE
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(
		&currentTemplateID, &currentTemplateVersion, &projectionVersion, &projectionState,
	)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("organization: lock executable projection: %w", err)
	}
	if currentTemplateID != manifest.FromTemplateID || currentTemplateVersion != manifest.FromTemplateVersion ||
		projectionVersion != activation.ExpectedProjectionVersion || projectionState != "active" {
		return MigrationResult{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_organization_v2_projection
		SET template_id=$3,template_version=$4,organization_v2_version=$5,activated_at=$6
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.owner.TenantID, store.owner.OrganizationID, manifest.ToTemplateID,
		manifest.ToTemplateVersion, projectionVersion+1, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: activate executable projection: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_active_organization_template (
			tenant_id,organization_id,schema_version,template_id,template_version,
			activation_kind,activation_id,migration_id,migration_version,
			projection_version,activated_at
		) VALUES ($1,$2,$3,$4,$5,'migration',$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			schema_version=EXCLUDED.schema_version,template_id=EXCLUDED.template_id,
			template_version=EXCLUDED.template_version,migration_id=EXCLUDED.migration_id,
			migration_version=EXCLUDED.migration_version,
			activation_kind=EXCLUDED.activation_kind,activation_id=EXCLUDED.activation_id,
			projection_version=EXCLUDED.projection_version,activated_at=EXCLUDED.activated_at
	`, store.owner.TenantID, store.owner.OrganizationID, TemplateSchemaVersion,
		manifest.ToTemplateID, manifest.ToTemplateVersion, activation.ID,
		manifest.ID, manifest.Version, projectionVersion+1, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: set active template: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_activations (
			tenant_id,organization_id,activation_id,migration_id,migration_version,
			manifest_digest,canonical_hash,signature_key_id,sealed_activation,activated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.owner.TenantID, store.owner.OrganizationID, activation.ID,
		activation.MigrationID, activation.MigrationVersion, activation.ManifestDigest.Digest,
		activationHash, activation.Signature.KeyID, sealedActivation, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: insert migration activation: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_organization_migration_heads
		SET state='activated',updated_at=$5
		WHERE tenant_id=$1 AND organization_id=$2 AND migration_id=$3 AND version=$4 AND state='staged'
	`, store.owner.TenantID, store.owner.OrganizationID, manifest.ID, manifest.Version, now)
	if err != nil || command.RowsAffected() != 1 {
		return MigrationResult{}, ErrMigrationState
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_events (
			tenant_id,organization_id,migration_id,migration_version,event_id,
			event_kind,event_hash,occurred_at
		) VALUES ($1,$2,$3,$4,$5,'activated',$6,$7)
	`, store.owner.TenantID, store.owner.OrganizationID, manifest.ID, manifest.Version,
		activation.ID, activationHash, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: insert migration activation event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: commit migration activation: %w", err)
	}
	return MigrationResult{
		MigrationID: manifest.ID, MigrationVersion: manifest.Version,
		State: MigrationActivated, TemplateID: manifest.ToTemplateID,
		TemplateVersion:   manifest.ToTemplateVersion,
		ProjectionVersion: projectionVersion + 1,
	}, nil
}

func (store *Store) RollbackMigration(
	ctx context.Context,
	rollback MigrationRollback,
) (MigrationResult, error) {
	if rollback.OrganizationID != store.owner.OrganizationID || rollback.OwnerID != store.owner.OwnerID ||
		VerifyMigrationRollback(rollback, store.owner.KeyID, store.owner.PublicKey) != nil {
		return MigrationResult{}, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return MigrationResult{}, err
	}
	if rollback.RolledBackAt.After(now) {
		return MigrationResult{}, ErrUnauthorized
	}
	canonical, rollbackHash, sealedRollback, err := store.prepareMigrationRollback(rollback)
	if err != nil {
		return MigrationResult{}, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MigrationResult{}, fmt.Errorf("organization: begin migration rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return MigrationResult{}, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, rollback.Signature.KeyID, rollback.RolledBackAt,
	); err != nil {
		return MigrationResult{}, err
	}
	if err := store.lock(ctx, tx, "migration|"+string(rollback.MigrationID)); err != nil {
		return MigrationResult{}, err
	}
	manifest, manifestHash, state, err := store.loadMigrationTx(
		ctx, tx, rollback.MigrationID, rollback.MigrationVersion,
	)
	if err != nil {
		return MigrationResult{}, err
	}
	if manifestHash != rollback.ManifestDigest.Digest {
		return MigrationResult{}, ErrConflict
	}
	if state == MigrationRolledBack {
		var existingHash string
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_organization_migration_rollbacks
			WHERE tenant_id=$1 AND organization_id=$2 AND rollback_id=$3
		`, store.owner.TenantID, store.owner.OrganizationID, rollback.ID).Scan(&existingHash); err != nil ||
			existingHash != rollbackHash {
			return MigrationResult{}, ErrMigrationState
		}
		return MigrationResult{
			MigrationID: manifest.ID, MigrationVersion: manifest.Version,
			State: MigrationRolledBack, TemplateID: manifest.FromTemplateID,
			TemplateVersion: manifest.FromTemplateVersion, Deduplicated: true,
		}, tx.Commit(ctx)
	}
	if state != MigrationStaged {
		return MigrationResult{}, ErrMigrationState
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_rollbacks (
			tenant_id,organization_id,rollback_id,migration_id,migration_version,
			manifest_digest,reason,canonical_hash,signature_key_id,sealed_rollback,rolled_back_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, store.owner.TenantID, store.owner.OrganizationID, rollback.ID,
		rollback.MigrationID, rollback.MigrationVersion, rollback.ManifestDigest.Digest,
		rollback.Reason, rollbackHash, rollback.Signature.KeyID, sealedRollback, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: insert migration rollback: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_organization_migration_heads
		SET state='rolled_back',updated_at=$5
		WHERE tenant_id=$1 AND organization_id=$2 AND migration_id=$3 AND version=$4 AND state='staged'
	`, store.owner.TenantID, store.owner.OrganizationID, manifest.ID, manifest.Version, now)
	if err != nil || command.RowsAffected() != 1 {
		return MigrationResult{}, ErrMigrationState
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_migration_events (
			tenant_id,organization_id,migration_id,migration_version,event_id,
			event_kind,event_hash,occurred_at
		) VALUES ($1,$2,$3,$4,$5,'rolled_back',$6,$7)
	`, store.owner.TenantID, store.owner.OrganizationID, manifest.ID, manifest.Version,
		rollback.ID, rollbackHash, now); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: insert migration rollback event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationResult{}, fmt.Errorf("organization: commit migration rollback: %w", err)
	}
	return MigrationResult{
		MigrationID: manifest.ID, MigrationVersion: manifest.Version,
		State: MigrationRolledBack, TemplateID: manifest.FromTemplateID,
		TemplateVersion: manifest.FromTemplateVersion,
	}, nil
}

func validateManifestTemplate(value MigrationManifest, template OrganizationTemplate) error {
	digest, err := TemplateDigest(template)
	if err != nil {
		return err
	}
	if template.ID != value.ToTemplateID || template.Version != value.ToTemplateVersion ||
		digest != value.ToTemplateDigest || template.CapabilityRegistryDigest != value.CapabilityRegistryDigest ||
		template.LegacyOrganizationVersion != value.FromOrganizationVersion ||
		len(template.Departments) != int(value.TopologyDepartmentCount) ||
		len(template.Departments)*3 != int(value.TopologySeatCount) ||
		!slices.Equal(template.ReceiptSchemaVersions, value.ReceiptSchemaVersions) ||
		template.Mode != TemplateLegacyProjection {
		return fmt.Errorf("organization: migration manifest does not match its target template")
	}
	return nil
}

func (store *Store) migrationBasisTx(ctx context.Context, tx pgx.Tx) (MigrationBasis, error) {
	var version uint64
	var organizationHash string
	err := tx.QueryRow(ctx, `
		SELECT legacy_organization_version FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationBasis{}, ErrNotFound
	}
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: load legacy organization version: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT record.canonical_hash FROM workforce_authority_records record
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.authority_kind='organization'
		  AND record.authority_id=$2 AND record.version=$3
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=record.tenant_id
			  AND revocation.organization_id=record.organization_id
			  AND revocation.authority_kind=record.authority_kind
			  AND revocation.authority_id=record.authority_id
			  AND revocation.version=record.version
		  )
	`, store.owner.TenantID, store.owner.OrganizationID, version).Scan(&organizationHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationBasis{}, ErrNotFound
	}
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: load legacy organization digest: %w", err)
	}
	authorityEntries, err := queryDigestEntries(ctx, tx, `
		SELECT authority_kind || ':' || authority_id || ':' || version::text || ':' || canonical_hash
		FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY authority_kind,authority_id,version
	`, store.owner.TenantID, store.owner.OrganizationID)
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: load legacy authority set: %w", err)
	}
	revocationEntries, err := queryDigestEntries(ctx, tx, `
		SELECT 'revocation:' || authority_kind || ':' || authority_id || ':' || version::text || ':' || signature
		FROM workforce_authority_revocations
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY authority_kind,authority_id,version
	`, store.owner.TenantID, store.owner.OrganizationID)
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: load legacy authority revocations: %w", err)
	}
	authorityEntries = append(authorityEntries, revocationEntries...)
	slices.Sort(authorityEntries)
	receiptEntries, err := queryDigestEntries(ctx, tx, `
		SELECT 'execution:' || receipt_id || ':' || content_hash
		FROM workforce_execution_receipts
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY receipt_id
	`, store.owner.TenantID, store.owner.OrganizationID)
	if err != nil {
		return MigrationBasis{}, fmt.Errorf("organization: load legacy receipt set: %w", err)
	}
	for _, query := range []string{
		`SELECT 'policy_change:' || receipt_id || ':' || lease_family || ':' || lease_id || ':' || authority_kind || ':' || authority_id || ':' || authority_version::text
		 FROM workforce_policy_change_receipts
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY receipt_id`,
		`SELECT 'company_authority:' || receipt_id || ':' || authority_kind || ':' || authority_id || ':' || authority_version::text || ':' || affected_lease_count::text || ':' || affected_authority_lease_count::text || ':' || affected_runtime_lease_count::text || ':' || affected_queued_wake_count::text || ':' || affected_dispatched_wake_count::text || ':' || affected_effect_count::text
		 FROM workforce_company_authority_change_receipts
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY receipt_id`,
		`SELECT 'lifecycle:' || receipt_id || ':' || receipt_hash
		 FROM workforce_lifecycle_decision_receipts
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY initiative_id,receipt_id`,
		`SELECT 'portfolio:' || decision_id || ':' || canonical_hash
		 FROM workforce_portfolio_decisions
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY decision_id`,
		`SELECT 'executive:' || decision_id || ':' || canonical_hash
		 FROM workforce_executive_decisions
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY decision_id`,
		`SELECT 'preserved:' || mutation_id || ':' || node_id || ':' || receipt_id
		 FROM workforce_company_plan_preserved_receipts
		 WHERE tenant_id=$1 AND organization_id=$2 ORDER BY mutation_id,node_id,receipt_id`,
	} {
		entries, err := queryDigestEntries(
			ctx, tx, query, store.owner.TenantID, store.owner.OrganizationID,
		)
		if err != nil {
			return MigrationBasis{}, fmt.Errorf("organization: load compatible receipt family: %w", err)
		}
		receiptEntries = append(receiptEntries, entries...)
	}
	slices.Sort(receiptEntries)
	authorityDigest, err := hashDigestSet("legacy_authority", authorityEntries)
	if err != nil {
		return MigrationBasis{}, err
	}
	receiptDigest, err := hashDigestSet("legacy_receipt", receiptEntries)
	if err != nil {
		return MigrationBasis{}, err
	}
	return MigrationBasis{
		LegacyOrganizationVersion: version,
		LegacyOrganizationDigest:  contracts.ContentHash{Algorithm: "sha256", Digest: organizationHash},
		LegacyAuthoritySetDigest:  authorityDigest,
		LegacyReceiptSetDigest:    receiptDigest,
	}, nil
}

func queryDigestEntries(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	arguments ...any,
) ([]string, error) {
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]string, 0)
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func hashDigestSet(kind string, entries []string) (contracts.ContentHash, error) {
	envelope := digestSetEnvelope{
		SchemaVersion: "workforce.organization-migration-digest-set.v1",
		Kind:          kind, Entries: append([]string(nil), entries...),
	}
	canonical, err := contracts.EncodeCanonical(&envelope)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func (store *Store) requireLegacyProjectionTx(
	ctx context.Context,
	tx pgx.Tx,
	manifest MigrationManifest,
) error {
	var templateID TemplateID
	var templateVersion, legacyVersion uint64
	var state string
	err := tx.QueryRow(ctx, `
		SELECT template_id,template_version,legacy_organization_version,state
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
		FOR UPDATE
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(
		&templateID, &templateVersion, &legacyVersion, &state,
	)
	if err != nil {
		return fmt.Errorf("organization: lock legacy projection: %w", err)
	}
	if templateID != manifest.FromTemplateID || templateVersion != manifest.FromTemplateVersion ||
		legacyVersion != manifest.FromOrganizationVersion || state != "active" {
		return ErrConflict
	}
	return nil
}

func (store *Store) validateNoAuthorityWideningTx(
	ctx context.Context,
	tx pgx.Tx,
	template OrganizationTemplate,
	legacyOrganizationVersion uint64,
) error {
	if template.Mode != TemplateLegacyProjection {
		return fmt.Errorf("organization: v1 migration target is not a legacy projection")
	}
	for _, department := range template.Departments {
		for _, projected := range department.Mandates {
			if projected.LegacyAuthority == nil || projected.Origin != MandateLegacyProjection {
				return fmt.Errorf("organization: migration contains owner-native authority")
			}
			legacy := *projected.LegacyAuthority
			if legacy.OrganizationVersion != legacyOrganizationVersion {
				return fmt.Errorf("organization: projected mandate changes the legacy organization version")
			}
			var seatHash string
			var mandateID contracts.MandateID
			var mandateVersion uint64
			err := tx.QueryRow(ctx, `
				SELECT authority.canonical_hash,projection.mandate_id,projection.mandate_version
				FROM workforce_authority_records authority
				JOIN workforce_organization_seats projection
				  ON projection.tenant_id=authority.tenant_id
				 AND projection.organization_id=authority.organization_id
				 AND projection.seat_id=authority.authority_id
				WHERE authority.tenant_id=$1 AND authority.organization_id=$2
				  AND authority.authority_kind='seat' AND authority.authority_id=$3
				  AND authority.version=$4 AND projection.department_id=$5
				  AND projection.seat_role=$6 AND projection.active=true
			`, store.owner.TenantID, store.owner.OrganizationID, projected.SeatID,
				legacy.SeatVersion, projected.DepartmentID, projected.Role).Scan(
				&seatHash, &mandateID, &mandateVersion,
			)
			if err != nil || seatHash != legacy.SeatDigest.Digest ||
				mandateID != projected.ID || mandateVersion != projected.Version {
				return fmt.Errorf("organization: projected seat authority does not match v1")
			}
			legacySeat, err := store.loadLegacySeatTx(ctx, tx, projected.SeatID, legacy.SeatVersion)
			if err != nil {
				return err
			}
			legacyMandate, err := store.loadLegacyMandateTx(ctx, tx, projected.ID, projected.Version)
			if err != nil {
				return err
			}
			mandateDigest, err := contracts.HashCanonical(&legacyMandate)
			if err != nil {
				return err
			}
			if mandateDigest != legacy.MandateDigest || projected.SeatDID != legacySeat.DID ||
				legacyMandate.DepartmentKind != contracts.DepartmentKind(department.Key) ||
				legacyMandate.SeatRole != projected.Role || legacySeat.BindingID != projected.ModelBinding.ID ||
				legacySeat.BindingVersion != projected.ModelBinding.Version ||
				projected.EffectiveAt.Before(legacySeat.EffectiveAt) ||
				projected.EffectiveAt.Before(legacyMandate.EffectiveAt) ||
				expiryWidens(projected.ExpiresAt, legacyMandate.ExpiresAt) ||
				!projectedSkillsSubset(projected.AllowedSkills, legacyMandate.AllowedSkills) ||
				!projectedScopesSubset(projected.DataScopes, legacyMandate.DataScopes) ||
				!sameEscalations(projected.EscalationRules, legacyMandate.EscalationRules) ||
				!projectedProhibitionsPreserve(projected.Prohibitions, legacyMandate.Prohibitions) {
				return fmt.Errorf("organization: projected mandate widens or reinterprets v1 authority")
			}
		}
	}
	return nil
}

func (store *Store) loadLegacySeatTx(
	ctx context.Context,
	tx pgx.Tx,
	id contracts.SeatID,
	version uint64,
) (contracts.Seat, error) {
	var expectedHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='seat'
		  AND authority_id=$3 AND version=$4
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=workforce_authority_records.tenant_id
			  AND revocation.organization_id=workforce_authority_records.organization_id
			  AND revocation.authority_kind=workforce_authority_records.authority_kind
			  AND revocation.authority_id=workforce_authority_records.authority_id
			  AND revocation.version=workforce_authority_records.version
		  )
	`, store.owner.TenantID, store.owner.OrganizationID, id, version).Scan(&expectedHash, &sealed)
	if err != nil {
		return contracts.Seat{}, ErrNotFound
	}
	canonical, err := store.vault.OpenRecord(store.legacyAuthorityAD("seat", string(id), version), sealed)
	if err != nil || digestBytes(canonical) != expectedHash {
		return contracts.Seat{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[contracts.Seat, *contracts.Seat](canonical)
	if err != nil {
		return contracts.Seat{}, ErrIntegrity
	}
	publicKey, err := store.resolveOwnerPublicKey(ctx, tx, value.Signature.KeyID, value.EffectiveAt)
	if err != nil || policy.VerifySeatAuthority(value, value.Signature.KeyID, publicKey) != nil {
		return contracts.Seat{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) loadLegacyMandateTx(
	ctx context.Context,
	tx pgx.Tx,
	id contracts.MandateID,
	version uint64,
) (contracts.Mandate, error) {
	var expectedHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='mandate'
		  AND authority_id=$3 AND version=$4
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=workforce_authority_records.tenant_id
			  AND revocation.organization_id=workforce_authority_records.organization_id
			  AND revocation.authority_kind=workforce_authority_records.authority_kind
			  AND revocation.authority_id=workforce_authority_records.authority_id
			  AND revocation.version=workforce_authority_records.version
		  )
	`, store.owner.TenantID, store.owner.OrganizationID, id, version).Scan(&expectedHash, &sealed)
	if err != nil {
		return contracts.Mandate{}, ErrNotFound
	}
	canonical, err := store.vault.OpenRecord(store.legacyAuthorityAD("mandate", string(id), version), sealed)
	if err != nil || digestBytes(canonical) != expectedHash {
		return contracts.Mandate{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[contracts.Mandate, *contracts.Mandate](canonical)
	if err != nil {
		return contracts.Mandate{}, ErrIntegrity
	}
	publicKey, err := store.resolveOwnerPublicKey(ctx, tx, value.Signature.KeyID, value.EffectiveAt)
	if err != nil || policy.VerifyMandateAuthority(value, value.Signature.KeyID, publicKey) != nil {
		return contracts.Mandate{}, ErrIntegrity
	}
	return value, nil
}

func projectedSkillsSubset(projected []contracts.SkillRef, legacy []contracts.SkillID) bool {
	for _, skill := range projected {
		if !slices.Contains(legacy, skill.ID) {
			return false
		}
	}
	return true
}

func projectedScopesSubset(projected, legacy []contracts.DataScope) bool {
	for _, scope := range projected {
		if !slices.Contains(legacy, scope) {
			return false
		}
	}
	return true
}

func sameEscalations(projected, legacy []contracts.EscalationRule) bool {
	if len(projected) != len(legacy) {
		return false
	}
	for _, rule := range legacy {
		if !slices.Contains(projected, rule) {
			return false
		}
	}
	return true
}

func expiryWidens(projected, legacy *time.Time) bool {
	if legacy == nil {
		return false
	}
	return projected == nil || projected.After(*legacy)
}

func projectedProhibitionsPreserve(projected, legacy []contracts.Prohibition) bool {
	for _, prohibition := range legacy {
		if !slices.Contains(projected, prohibition) {
			return false
		}
	}
	return true
}

func (store *Store) loadMigrationTx(
	ctx context.Context,
	tx pgx.Tx,
	id MigrationID,
	version uint64,
) (MigrationManifest, string, MigrationState, error) {
	var expectedHash string
	var sealed []byte
	var state MigrationState
	err := tx.QueryRow(ctx, `
		SELECT migration.canonical_hash,migration.sealed_manifest,head.state
		FROM workforce_organization_migrations migration
		JOIN workforce_organization_migration_heads head
		  ON head.tenant_id=migration.tenant_id AND head.organization_id=migration.organization_id
		 AND head.migration_id=migration.migration_id AND head.version=migration.version
		WHERE migration.tenant_id=$1 AND migration.organization_id=$2
		  AND migration.migration_id=$3 AND migration.version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, id, version).Scan(
		&expectedHash, &sealed, &state,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationManifest{}, "", "", ErrNotFound
	}
	if err != nil {
		return MigrationManifest{}, "", "", fmt.Errorf("organization: load migration: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.migrationManifestAD(id, version), sealed)
	if err != nil || digestBytes(canonical) != expectedHash {
		return MigrationManifest{}, "", "", ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[MigrationManifest, *MigrationManifest](canonical)
	if err != nil || VerifyMigrationManifest(value, store.owner.KeyID, store.owner.PublicKey) != nil ||
		!state.Valid() {
		return MigrationManifest{}, "", "", ErrIntegrity
	}
	return value, expectedHash, state, nil
}

func (store *Store) prepareMigrationManifest(
	value MigrationManifest,
) ([]byte, string, []byte, error) {
	return prepareMigrationRecord(
		store,
		&value, store.migrationManifestAD(value.ID, value.Version), "manifest",
	)
}

func (store *Store) prepareMigrationActivation(
	value MigrationActivation,
) ([]byte, string, []byte, error) {
	return prepareMigrationRecord(
		store,
		&value, store.migrationActivationAD(value.ID), "activation",
	)
}

func (store *Store) prepareMigrationRollback(
	value MigrationRollback,
) ([]byte, string, []byte, error) {
	return prepareMigrationRecord(
		store,
		&value, store.migrationRollbackAD(value.ID), "rollback",
	)
}

func prepareCanonical[T contracts.Validatable](value T) ([]byte, string, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return nil, "", err
	}
	return canonical, digestBytes(canonical), nil
}

func prepareMigrationRecord[T contracts.Validatable](
	store *Store,
	value T,
	associatedData vault.AD,
	kind string,
) ([]byte, string, []byte, error) {
	canonical, hash, err := prepareCanonical(value)
	if err != nil {
		return nil, "", nil, err
	}
	sealed, err := store.vault.SealRecord(associatedData, canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("organization: seal migration %s: %w", kind, err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) migrationManifestAD(id MigrationID, version uint64) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.migration.manifest",
		Stream: string(store.owner.OrganizationID) + "/" + string(id),
		Schema: MigrationManifestSchemaVersion + ".v" + strconv.FormatUint(version, 10),
	}
}

func (store *Store) migrationActivationAD(id string) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.migration.activation",
		Stream: string(store.owner.OrganizationID) + "/" + id,
		Schema: MigrationActivationSchemaVersion,
	}
}

func (store *Store) migrationRollbackAD(id string) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.migration.rollback",
		Stream: string(store.owner.OrganizationID) + "/" + id,
		Schema: MigrationRollbackSchemaVersion,
	}
}

func (store *Store) legacyAuthorityAD(kind, id string, version uint64) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.authority." + kind,
		Stream: string(store.owner.OrganizationID) + "/" + id + "/" + strconv.FormatUint(version, 10),
		Schema: contracts.SchemaVersionV1,
	}
}
