package organization

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

type TemplateActivationResult struct {
	ActivationID      string
	TemplateID        TemplateID
	TemplateVersion   uint64
	ProjectionVersion uint64
	Deduplicated      bool
}

func (store *Store) ActivateTemplate(
	ctx context.Context,
	activation TemplateActivation,
) (TemplateActivationResult, error) {
	if activation.OrganizationID != store.owner.OrganizationID ||
		activation.OwnerID != store.owner.OwnerID ||
		VerifyTemplateActivation(activation, store.owner.KeyID, store.owner.PublicKey) != nil {
		return TemplateActivationResult{}, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return TemplateActivationResult{}, err
	}
	canonical, err := contracts.EncodeCanonical(&activation)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	hash := digestBytes(canonical)
	var replayHash string
	err = store.pool.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_organization_template_activations
		WHERE tenant_id=$1 AND organization_id=$2 AND activation_id=$3
	`, store.owner.TenantID, store.owner.OrganizationID, activation.ID).Scan(&replayHash)
	if err == nil {
		if replayHash != hash {
			return TemplateActivationResult{}, ErrConflict
		}
		var activeID TemplateID
		var activeVersion, projectionVersion uint64
		if err := store.pool.QueryRow(ctx, `
			SELECT template_id,template_version,projection_version
			FROM workforce_active_organization_template
			WHERE tenant_id=$1 AND organization_id=$2
		`, store.owner.TenantID, store.owner.OrganizationID).Scan(
			&activeID, &activeVersion, &projectionVersion,
		); err != nil || activeID != activation.ToTemplateID ||
			activeVersion != activation.ToTemplateVersion ||
			projectionVersion != activation.ExpectedProjectionVersion+1 {
			return TemplateActivationResult{}, ErrConflict
		}
		return TemplateActivationResult{
			ActivationID: activation.ID, TemplateID: activeID,
			TemplateVersion: activeVersion, ProjectionVersion: projectionVersion,
			Deduplicated: true,
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TemplateActivationResult{}, fmt.Errorf("organization: inspect template activation replay: %w", err)
	}
	if activation.EffectiveAt.After(now) || !activation.ExpiresAt.After(now) {
		return TemplateActivationResult{}, ErrUnauthorized
	}
	current, err := store.LoadActiveTemplate(ctx)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	target, err := store.LoadTemplate(ctx, activation.ToTemplateID, activation.ToTemplateVersion)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	currentDigest, err := TemplateDigest(current)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	targetDigest, err := TemplateDigest(target)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	if current.ID != activation.FromTemplateID || current.Version != activation.FromTemplateVersion ||
		currentDigest != activation.FromTemplateDigest || targetDigest != activation.ToTemplateDigest {
		return TemplateActivationResult{}, ErrConflict
	}
	for _, receiptVersion := range current.ReceiptSchemaVersions {
		if !slices.Contains(target.ReceiptSchemaVersions, receiptVersion) {
			return TemplateActivationResult{}, fmt.Errorf("organization: target template drops receipt compatibility")
		}
	}
	registry, err := store.LoadRegistry(ctx, now)
	if err != nil {
		return TemplateActivationResult{}, err
	}
	if err := store.validateTemplate(ctx, target, registry, now, true); err != nil {
		return TemplateActivationResult{}, err
	}
	sealed, err := store.vault.SealRecord(store.templateActivationAD(activation.ID), canonical)
	if err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: seal template activation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: begin template activation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return TemplateActivationResult{}, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, activation.Signature.KeyID, activation.EffectiveAt,
	); err != nil {
		return TemplateActivationResult{}, err
	}
	if err := store.lock(ctx, tx, "template-activation"); err != nil {
		return TemplateActivationResult{}, err
	}
	if err := store.lock(ctx, tx, "squad-assignment-plan"); err != nil {
		return TemplateActivationResult{}, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_organization_template_activations
		WHERE tenant_id=$1 AND organization_id=$2 AND activation_id=$3
	`, store.owner.TenantID, store.owner.OrganizationID, activation.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return TemplateActivationResult{}, ErrConflict
		}
		return TemplateActivationResult{
			ActivationID: activation.ID, TemplateID: target.ID,
			TemplateVersion:   target.Version,
			ProjectionVersion: activation.ExpectedProjectionVersion + 1,
			Deduplicated:      true,
		}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TemplateActivationResult{}, fmt.Errorf("organization: inspect template activation: %w", err)
	}
	var activeTemplateID TemplateID
	var activeTemplateVersion, projectionVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT template_id,template_version,projection_version
		FROM workforce_active_organization_template
		WHERE tenant_id=$1 AND organization_id=$2
		FOR UPDATE
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(
		&activeTemplateID, &activeTemplateVersion, &projectionVersion,
	)
	if err != nil || activeTemplateID != current.ID || activeTemplateVersion != current.Version ||
		projectionVersion != activation.ExpectedProjectionVersion {
		return TemplateActivationResult{}, ErrConflict
	}
	var projectionState string
	var authorityProjectionVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT state,organization_v2_version
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
		FOR UPDATE
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(
		&projectionState, &authorityProjectionVersion,
	)
	if err != nil || projectionState != "active" || authorityProjectionVersion != projectionVersion {
		return TemplateActivationResult{}, ErrConflict
	}
	cancellationReason := "owner-signed organization template activation " + activation.ID
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET state='cancelled',cancellation_reason=$3
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
	`, store.owner.TenantID, store.owner.OrganizationID, cancellationReason); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: cancel affected runtime leases: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_lease_invalidations (
			tenant_id,organization_id,lease_id,authority_kind,authority_id,
			authority_version,reason,invalidated_at
		)
		SELECT lease.tenant_id,lease.organization_id,lease.lease_id,
		       'organization_template',$3,$4,$5,$6
		FROM workforce_authority_leases lease
		WHERE lease.tenant_id=$1 AND lease.organization_id=$2 AND lease.expires_at>$6
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_lease_invalidations invalidation
			WHERE invalidation.tenant_id=lease.tenant_id
			  AND invalidation.organization_id=lease.organization_id
			  AND invalidation.lease_id=lease.lease_id
			  AND invalidation.authority_kind='organization_template'
			  AND invalidation.authority_id=$3 AND invalidation.authority_version=$4
		  )
	`, store.owner.TenantID, store.owner.OrganizationID, current.ID, current.Version,
		cancellationReason, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: invalidate affected authority leases: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_wake_requests
		SET state='coalesced'
		WHERE tenant_id=$1 AND organization_id=$2 AND state='queued'
	`, store.owner.TenantID, store.owner.OrganizationID); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: coalesce queued wake requests: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_scheduled_wakes
		SET state='coalesced',last_error=$3,updated_at=$4
		WHERE tenant_id=$1 AND organization_id=$2 AND state='queued'
	`, store.owner.TenantID, store.owner.OrganizationID, cancellationReason, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: coalesce scheduled wakes: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_template_activations (
			tenant_id,organization_id,activation_id,owner_id,from_template_id,
			from_template_version,to_template_id,to_template_version,
			expected_projection_version,canonical_hash,signature_key_id,
			sealed_activation,effective_at,expires_at,activated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, store.owner.TenantID, store.owner.OrganizationID, activation.ID,
		activation.OwnerID, activation.FromTemplateID, activation.FromTemplateVersion,
		activation.ToTemplateID, activation.ToTemplateVersion,
		activation.ExpectedProjectionVersion, hash, activation.Signature.KeyID,
		sealed, activation.EffectiveAt, activation.ExpiresAt, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: insert template activation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_squad_assignment_revocations (
			tenant_id,organization_id,assignment_id,activation_id,reason,revoked_at
		)
		SELECT assignment.tenant_id,assignment.organization_id,assignment.assignment_id,
		       $3,$4,$5
		FROM workforce_squad_assignments assignment
		WHERE assignment.tenant_id=$1 AND assignment.organization_id=$2
		  AND assignment.expires_at>$5
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_squad_assignment_revocations revocation
			WHERE revocation.tenant_id=assignment.tenant_id
			  AND revocation.organization_id=assignment.organization_id
			  AND revocation.assignment_id=assignment.assignment_id
		  )
	`, store.owner.TenantID, store.owner.OrganizationID, activation.ID,
		cancellationReason, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: revoke affected squad assignments: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_organization_v2_projection
		SET template_id=$3,template_version=$4,organization_v2_version=$5,activated_at=$6
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.owner.TenantID, store.owner.OrganizationID, target.ID, target.Version,
		projectionVersion+1, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: update executable template projection: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_active_organization_template
		SET template_id=$3,template_version=$4,projection_version=$5,
			activation_kind='template',activation_id=$6,
			migration_id=NULL,migration_version=NULL,activated_at=$7
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.owner.TenantID, store.owner.OrganizationID, target.ID, target.Version,
		projectionVersion+1, activation.ID, now); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: update active template: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TemplateActivationResult{}, fmt.Errorf("organization: commit template activation: %w", err)
	}
	return TemplateActivationResult{
		ActivationID: activation.ID, TemplateID: target.ID, TemplateVersion: target.Version,
		ProjectionVersion: projectionVersion + 1,
	}, nil
}

func (store *Store) templateActivationAD(id string) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.template-activation",
		Stream: string(store.owner.OrganizationID) + "/" + id,
		Schema: TemplateActivationSchemaVersion,
	}
}
