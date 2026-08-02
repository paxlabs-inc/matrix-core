package companyrecovery

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

// CommitRecoveryQualification persists an explicitly runtime-signed recovery
// release record only when every bound durable artifact exactly matches current
// PostgreSQL state. It does not infer or manufacture a qualification.
func (store *Store) CommitRecoveryQualification(ctx context.Context, value RecoveryQualification) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.authority.RuntimeKeyID ||
		VerifyRecoveryQualification(value, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil ||
		value.QualifiedAt.After(now) || !value.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "recovery-qualification"); err != nil {
		return false, err
	}
	if err := store.validateRecoveryQualificationTx(ctx, tx, value, now); err != nil {
		return false, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.recoveryQualificationAD(value.ID), canonical)
	if err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_recovery_qualifications (
			tenant_id,organization_id,qualification_id,recovery_policy_id,recovery_policy_version,
			recovery_policy_hash,backup_id,backup_manifest_hash,archive_hash,restore_id,
			restore_receipt_hash,offline_batch_id,offline_receipt_hash,restored_tables,restored_rows,
			cancelled_runtime_leases,invalidated_authority_leases,coalesced_wakes,
			quarantined_effects,quarantined_external_state,offline_result_count,
			offline_reconciliation_count,rpo_micros,rpo_status,rto_micros,rto_status,
			canonical_hash,key_id,sealed_qualification,qualified_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$30)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.ID, value.RecoveryPolicyID,
		value.RecoveryPolicyVersion, value.RecoveryPolicyHash.Digest, value.BackupID,
		value.BackupManifestHash.Digest, value.ArchiveHash.Digest, value.RestoreID,
		value.RestoreReceiptHash.Digest, value.OfflineBatchID, value.OfflineReceiptHash.Digest,
		value.RestoredTables, value.RestoredRows, value.CancelledRuntimeLeases,
		value.InvalidatedAuthorityLeases, value.CoalescedWakes, value.QuarantinedEffects,
		value.QuarantinedExternalState, value.OfflineResultCount, value.OfflineReconciliationCount,
		value.RPO.Microseconds(), value.RPOStatus, value.RTO.Microseconds(), value.RTOStatus,
		canonicalHash.Digest, value.Signature.KeyID, sealed, value.QualifiedAt, value.ExpiresAt)
	if err != nil {
		return false, err
	}
	replayed := command.RowsAffected() == 0
	if replayed {
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_qualifications
			WHERE tenant_id=$1 AND organization_id=$2 AND qualification_id=$3`, store.tenantID,
			store.organizationID, value.ID).Scan(&existingHash); err != nil || existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	headCommand, err := tx.Exec(ctx, `
		INSERT INTO workforce_recovery_qualification_heads (
			tenant_id,organization_id,qualification_id,canonical_hash,recovery_policy_id,
			recovery_policy_version,recovery_policy_hash,qualified_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			qualification_id=EXCLUDED.qualification_id,canonical_hash=EXCLUDED.canonical_hash,
			recovery_policy_id=EXCLUDED.recovery_policy_id,
			recovery_policy_version=EXCLUDED.recovery_policy_version,
			recovery_policy_hash=EXCLUDED.recovery_policy_hash,
			qualified_at=EXCLUDED.qualified_at,expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
		WHERE workforce_recovery_qualification_heads.qualified_at<EXCLUDED.qualified_at
	`, store.tenantID, store.organizationID, value.ID, canonicalHash.Digest,
		value.RecoveryPolicyID, value.RecoveryPolicyVersion, value.RecoveryPolicyHash.Digest,
		value.QualifiedAt, value.ExpiresAt, now)
	if err != nil {
		return false, err
	}
	if headCommand.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) CurrentRecoveryQualification(ctx context.Context) (RecoveryQualification, error) {
	now, err := store.currentTime()
	if err != nil {
		return RecoveryQualification{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecoveryQualification{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var id RecoveryQualificationID
	var expectedHash string
	err = tx.QueryRow(ctx, `SELECT qualification_id,canonical_hash
		FROM workforce_recovery_qualification_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND expires_at>$3`, store.tenantID,
		store.organizationID, now).Scan(&id, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryQualification{}, ErrNotFound
	}
	if err != nil {
		return RecoveryQualification{}, err
	}
	value, err := store.loadRecoveryQualificationTx(ctx, tx, id, expectedHash)
	if err != nil {
		return RecoveryQualification{}, err
	}
	if err := store.validateRecoveryQualificationTx(ctx, tx, value, now); err != nil {
		return RecoveryQualification{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RecoveryQualification{}, err
	}
	return value, nil
}

func (store *Store) validateRecoveryQualificationTx(ctx context.Context, tx pgx.Tx, value RecoveryQualification, now time.Time) error {
	var policyID RecoveryPolicyID
	var policyVersion uint64
	var policyHash, policyCanonicalHash, policyKeyID string
	var policySealed []byte
	var policyExpiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT head.policy_id,head.version,head.content_hash,policy.canonical_hash,
		       policy.key_id,policy.sealed_policy,head.expires_at
		FROM workforce_recovery_policy_heads head
		JOIN workforce_recovery_policies policy
		  ON policy.tenant_id=head.tenant_id AND policy.organization_id=head.organization_id
		 AND policy.policy_id=head.policy_id AND policy.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.effective_at<=$3 AND head.expires_at>$3
	`, store.tenantID, store.organizationID, now).Scan(&policyID, &policyVersion, &policyHash,
		&policyCanonicalHash, &policyKeyID, &policySealed, &policyExpiresAt)
	if err != nil || policyID != value.RecoveryPolicyID || policyVersion != value.RecoveryPolicyVersion ||
		policyHash != value.RecoveryPolicyHash.Digest || value.ExpiresAt.After(policyExpiresAt.UTC()) {
		return ErrConflict
	}
	opened, err := store.vault.OpenRecord(store.recoveryPolicyAD(policyID, policyVersion), policySealed)
	if err != nil {
		return ErrUnauthorized
	}
	policy, err := contracts.DecodeCanonical[RecoveryPolicy, *RecoveryPolicy](opened)
	if err != nil || policy.Signature.KeyID != policyKeyID ||
		VerifyRecoveryPolicy(policy, store.authority.FounderPublicKey) != nil {
		return ErrUnauthorized
	}
	canonical, err := contracts.HashCanonical(&policy)
	if err != nil || canonical.Digest != policyCanonicalHash || policy.ContentHash.Digest != policyHash {
		return ErrUnauthorized
	}

	var manifestHash, archiveHash, backupKeyID, rpoStatus string
	var sealedManifest []byte
	var keyErased bool
	err = tx.QueryRow(ctx, `
		SELECT manifest_hash,archive_hash,key_id,sealed_manifest,key_erased,rpo_status
		FROM workforce_recovery_backups
		WHERE tenant_id=$1 AND organization_id=$2 AND backup_id=$3 AND state='completed'
	`, store.tenantID, store.organizationID, value.BackupID).Scan(&manifestHash, &archiveHash,
		&backupKeyID, &sealedManifest, &keyErased, &rpoStatus)
	if err != nil || keyErased || manifestHash != value.BackupManifestHash.Digest ||
		archiveHash != value.ArchiveHash.Digest || rpoStatus != "met" || value.RPOStatus != rpoStatus {
		return ErrConflict
	}
	opened, err = store.vault.OpenRecord(store.backupManifestAD(value.BackupID), sealedManifest)
	if err != nil {
		return ErrUnauthorized
	}
	manifest, err := contracts.DecodeCanonical[BackupManifest, *BackupManifest](opened)
	if err != nil || manifest.Signature.KeyID != backupKeyID ||
		VerifyBackupManifest(manifest, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil ||
		manifest.Body.ArchiveHash != value.ArchiveHash || manifest.Body.RPO != value.RPO ||
		manifest.Body.RPO != policy.Body.RPO || manifest.Body.RTO != value.RTO || manifest.Body.RTO != policy.Body.RTO {
		return ErrConflict
	}
	manifestCanonical, err := contracts.HashCanonical(&manifest)
	if err != nil || manifestCanonical.Digest != manifestHash {
		return ErrUnauthorized
	}

	var restoreBackupID BackupID
	var restoreMode RestoreMode
	var restoreState RestoreState
	var restoreReceiptHash, restoreKeyID, restoreRTOStatus, reconciliationEvidenceHash string
	var restoreSealed []byte
	var reconciledAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT backup_id,mode,state,receipt_hash,key_id,sealed_receipt,rto_status,
		       reconciliation_evidence_hash,reconciled_at
		FROM workforce_recovery_restores
		WHERE tenant_id=$1 AND organization_id=$2 AND restore_id=$3
	`, store.tenantID, store.organizationID, value.RestoreID).Scan(&restoreBackupID, &restoreMode,
		&restoreState, &restoreReceiptHash, &restoreKeyID, &restoreSealed, &restoreRTOStatus,
		&reconciliationEvidenceHash, &reconciledAt)
	if err != nil || restoreBackupID != value.BackupID || restoreMode != RestoreClean ||
		restoreState != RestoreReady || restoreReceiptHash != value.RestoreReceiptHash.Digest ||
		restoreRTOStatus != "met" || value.RTOStatus != restoreRTOStatus || !value.CleanRestoreReady ||
		value.QualifiedAt.Before(reconciledAt.UTC()) {
		return ErrConflict
	}
	opened, err = store.vault.OpenRecord(store.restoreReceiptAD(value.RestoreID), restoreSealed)
	if err != nil {
		return ErrUnauthorized
	}
	restoreReceipt, err := contracts.DecodeCanonical[RestoreReceipt, *RestoreReceipt](opened)
	if err != nil || restoreReceipt.Signature.KeyID != restoreKeyID ||
		VerifyRestoreReceipt(restoreReceipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil ||
		restoreReceipt.Body.State != RestoreReady || restoreReceipt.Body.BackupID != value.BackupID ||
		restoreReceipt.Body.ArchiveHash != value.ArchiveHash || restoreReceipt.Body.RestoredTables != value.RestoredTables ||
		restoreReceipt.Body.RestoredRows != value.RestoredRows ||
		restoreReceipt.Body.CancelledRuntimeLeases != value.CancelledRuntimeLeases ||
		restoreReceipt.Body.InvalidatedAuthorityLeases != value.InvalidatedAuthorityLeases ||
		restoreReceipt.Body.CoalescedWakes != value.CoalescedWakes ||
		restoreReceipt.Body.QuarantinedEffects != value.QuarantinedEffects ||
		restoreReceipt.Body.QuarantinedExternalState != value.QuarantinedExternalState ||
		restoreReceipt.Body.ReconciliationEvidenceHash == nil ||
		restoreReceipt.Body.ReconciliationEvidenceHash.Digest != reconciliationEvidenceHash {
		return ErrConflict
	}
	restoreCanonical, err := contracts.HashCanonical(&restoreReceipt)
	if err != nil || restoreCanonical.Digest != restoreReceiptHash {
		return ErrUnauthorized
	}

	var offlineReceiptHash, offlineRuntimeKeyID, baseArchiveHash string
	var offlineSealed []byte
	var baseBackupID BackupID
	var contiguous bool
	var offlineReconciledAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT base_backup_id,base_archive_hash,receipt_hash,runtime_key_id,sealed_receipt,contiguous,reconciled_at
		FROM workforce_recovery_offline_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, store.tenantID, store.organizationID, value.OfflineBatchID).Scan(&baseBackupID,
		&baseArchiveHash, &offlineReceiptHash, &offlineRuntimeKeyID, &offlineSealed,
		&contiguous, &offlineReconciledAt)
	if err != nil || !contiguous || baseBackupID != value.BackupID || baseArchiveHash != value.ArchiveHash.Digest ||
		offlineReceiptHash != value.OfflineReceiptHash.Digest || value.QualifiedAt.Before(offlineReconciledAt.UTC()) {
		return ErrConflict
	}
	opened, err = store.vault.OpenRecord(store.offlineReceiptAD(value.OfflineBatchID), offlineSealed)
	if err != nil {
		return ErrUnauthorized
	}
	offlineReceipt, err := contracts.DecodeCanonical[OfflineReceipt, *OfflineReceipt](opened)
	if err != nil || offlineReceipt.Signature.KeyID != offlineRuntimeKeyID ||
		VerifyOfflineReceipt(offlineReceipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil ||
		uint32(len(offlineReceipt.Body.Results)) != value.OfflineResultCount {
		return ErrConflict
	}
	offlineCanonical, err := contracts.HashCanonical(&offlineReceipt)
	if err != nil || offlineCanonical.Digest != offlineReceiptHash {
		return ErrUnauthorized
	}
	var openReconciliations uint32
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workforce_recovery_offline_reconciliation
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3 AND state='open'`, store.tenantID,
		store.organizationID, value.OfflineBatchID).Scan(&openReconciliations); err != nil ||
		openReconciliations != value.OfflineReconciliationCount || openReconciliations != 0 {
		return ErrReconciliationRequired
	}
	return nil
}

func (store *Store) loadRecoveryQualificationTx(ctx context.Context, tx pgx.Tx, id RecoveryQualificationID, expectedHash string) (RecoveryQualification, error) {
	var keyID, canonicalHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `SELECT canonical_hash,key_id,sealed_qualification
		FROM workforce_recovery_qualifications
		WHERE tenant_id=$1 AND organization_id=$2 AND qualification_id=$3`, store.tenantID,
		store.organizationID, id).Scan(&canonicalHash, &keyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryQualification{}, ErrNotFound
	}
	if err != nil || canonicalHash != expectedHash {
		return RecoveryQualification{}, ErrConflict
	}
	opened, err := store.vault.OpenRecord(store.recoveryQualificationAD(id), sealed)
	if err != nil {
		return RecoveryQualification{}, ErrUnauthorized
	}
	value, err := contracts.DecodeCanonical[RecoveryQualification, *RecoveryQualification](opened)
	if err != nil || value.Signature.KeyID != keyID ||
		VerifyRecoveryQualification(value, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return RecoveryQualification{}, ErrUnauthorized
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil || hash.Digest != canonicalHash {
		return RecoveryQualification{}, ErrUnauthorized
	}
	return value, nil
}

func (store *Store) recoveryQualificationAD(id RecoveryQualificationID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.qualification", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: RecoveryQualificationSchemaVersion}
}
