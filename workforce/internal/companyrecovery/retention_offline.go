package companyrecovery

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

// ExecuteErasure applies an explicit founder-signed deletion or cryptographic
// erasure directive and leaves a runtime-signed, immutable receipt.
func (store *Store) ExecuteErasure(ctx context.Context, directive ErasureDirective) (ErasureReceipt, bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	if directive.Validate() != nil || directive.Body.OrganizationID != store.organizationID ||
		directive.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyErasureDirective(directive, store.authority.FounderPublicKey) != nil || directive.Body.ExecuteAfter.After(now) {
		return ErasureReceipt{}, false, ErrUnauthorized
	}
	directiveCanonical, err := contracts.EncodeCanonical(&directive)
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	directiveHash := hashBytes(directiveCanonical)
	if existing, existingHash, loadErr := store.loadErasure(ctx, directive.Body.ID); loadErr == nil {
		if existingHash != directiveHash.Digest {
			return ErasureReceipt{}, false, ErrConflict
		}
		return existing, true, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return ErasureReceipt{}, false, loadErr
	}

	var destroyedKeys uint32
	var deletedObjects uint64
	if directive.Body.TargetKind == ErasureScope {
		if store.erasure == nil {
			return ErasureReceipt{}, false, fmt.Errorf("company recovery: scoped erasure backend is unavailable")
		}
		destroyedKeys, deletedObjects, err = store.erasure.Erase(ctx, directive.Body)
		if err != nil {
			return ErasureReceipt{}, false, err
		}
		if directive.Body.Action == RetentionCryptoErase && destroyedKeys == 0 {
			return ErasureReceipt{}, false, fmt.Errorf("company recovery: erasure backend destroyed no keys")
		}
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if directive.Body.TargetKind == ErasureBackup {
		if directive.Body.Class != DataBackup {
			return ErasureReceipt{}, false, ErrUnauthorized
		}
		destroyedKeys, deletedObjects, err = store.eraseBackupTx(ctx, tx, BackupID(directive.Body.TargetID), directive.Body.Action, now)
		if err != nil {
			return ErasureReceipt{}, false, err
		}
	}
	receipt := ErasureReceipt{Body: ErasureReceiptBody{
		SchemaVersion: SchemaVersion, ID: directive.Body.ID, OrganizationID: store.organizationID,
		TargetKind: directive.Body.TargetKind, TargetID: directive.Body.TargetID, Class: directive.Body.Class,
		Action: directive.Body.Action, DestroyedKeys: destroyedKeys, DeletedObjects: deletedObjects, ExecutedAt: now,
	}}
	if err := signSimple(&receipt, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { receipt.Signature = signature }); err != nil {
		return ErasureReceipt{}, false, err
	}
	sealedDirective, err := store.vault.SealRecord(store.erasureDirectiveAD(directive.Body.ID), directiveCanonical)
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	receiptCanonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	receiptHash := hashBytes(receiptCanonical)
	sealedReceipt, err := store.vault.SealRecord(store.erasureReceiptAD(directive.Body.ID), receiptCanonical)
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_erasures (
			tenant_id,organization_id,erasure_id,target_kind,target_id,data_class,action,
			directive_hash,receipt_hash,key_id,sealed_directive,sealed_receipt,
			destroyed_keys,deleted_objects,executed_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)
	`, store.tenantID, store.organizationID, directive.Body.ID, directive.Body.TargetKind,
		directive.Body.TargetID, directive.Body.Class, directive.Body.Action, directiveHash.Digest,
		receiptHash.Digest, receipt.Signature.KeyID, sealedDirective, sealedReceipt,
		destroyedKeys, deletedObjects, now)
	if err != nil {
		return ErasureReceipt{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErasureReceipt{}, false, err
	}
	return receipt, false, nil
}

// ApplyRetention applies the active founder-signed recovery policy to expired
// backup artifacts. It never deletes the signed manifest or erasure audit row.
func (store *Store) ApplyRetention(ctx context.Context) ([]ErasureReceipt, error) {
	policy, err := store.LoadRecoveryPolicy(ctx, true)
	if err != nil {
		return nil, err
	}
	var rule *RetentionRule
	for index := range policy.Body.Rules {
		if policy.Body.Rules[index].Class == DataBackup {
			copy := policy.Body.Rules[index]
			rule = &copy
			break
		}
	}
	if rule == nil || rule.Action == RetentionKeep {
		return nil, nil
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT backup_id FROM workforce_recovery_backups
		WHERE tenant_id=$1 AND organization_id=$2 AND state='completed' AND key_erased=FALSE
		  AND completed_at+$3::interval<=$4 ORDER BY completed_at,backup_id
	`, store.tenantID, store.organizationID, durationInterval(rule.Retention), now)
	if err != nil {
		return nil, err
	}
	var ids []BackupID
	for rows.Next() {
		var id BackupID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	receipts := make([]ErasureReceipt, 0, len(ids))
	for _, id := range ids {
		tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return nil, err
		}
		destroyed, deleted, eraseErr := store.eraseBackupTx(ctx, tx, id, rule.Action, now)
		if eraseErr != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			if errors.Is(eraseErr, ErrArchiveErased) {
				continue
			}
			return nil, eraseErr
		}
		receipt := ErasureReceipt{Body: ErasureReceiptBody{
			SchemaVersion: SchemaVersion, ID: ErasureID(stableID("retention", string(policy.Body.ID), fmt.Sprint(policy.Body.Version), string(id))),
			OrganizationID: store.organizationID, TargetKind: ErasureBackup, TargetID: string(id),
			Class: DataBackup, Action: rule.Action, DestroyedKeys: destroyed, DeletedObjects: deleted, ExecutedAt: now,
		}}
		if err := signSimple(&receipt, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
			func(signature contracts.Signature) { receipt.Signature = signature }); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
		canonical, err := contracts.EncodeCanonical(&receipt)
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
		hash := hashBytes(canonical)
		sealed, err := store.vault.SealRecord(store.erasureReceiptAD(receipt.Body.ID), canonical)
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_retention_executions (
				tenant_id,organization_id,execution_id,policy_id,policy_version,backup_id,
				action,receipt_hash,key_id,sealed_receipt,destroyed_keys,deleted_objects,executed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, receipt.Body.ID, policy.Body.ID, policy.Body.Version,
			id, rule.Action, hash.Digest, receipt.Signature.KeyID, sealed, destroyed, deleted, now)
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (store *Store) eraseBackupTx(ctx context.Context, tx pgx.Tx, id BackupID, action RetentionAction, now time.Time) (uint32, uint64, error) {
	var keyErased bool
	var encryptedBytes, sealedKeyBytes int64
	err := tx.QueryRow(ctx, `SELECT key_erased,octet_length(encrypted_archive),octet_length(sealed_archive_key)
		FROM workforce_recovery_backups WHERE tenant_id=$1 AND organization_id=$2 AND backup_id=$3 FOR UPDATE`,
		store.tenantID, store.organizationID, id).Scan(&keyErased, &encryptedBytes, &sealedKeyBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	if keyErased || sealedKeyBytes == 0 {
		return 0, 0, ErrArchiveErased
	}
	deleted := uint64(1)
	state := "completed"
	if action == RetentionDelete {
		deleted = 2
		state = "deleted"
	}
	if action != RetentionDelete && action != RetentionCryptoErase {
		return 0, 0, ErrUnauthorized
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_backups SET state=$1,key_erased=TRUE,
		sealed_archive_key=NULL,encrypted_archive=CASE WHEN $2 THEN NULL ELSE encrypted_archive END,erased_at=$3
		WHERE tenant_id=$4 AND organization_id=$5 AND backup_id=$6 AND key_erased=FALSE`,
		state, action == RetentionDelete, now, store.tenantID, store.organizationID, id)
	if err != nil {
		return 0, 0, err
	}
	_ = encryptedBytes
	return 1, deleted, nil
}

// CoalesceOfflineBatch verifies a per-machine signed sequence and writes only
// into the offline staging ledger. Authority, leases, approvals, effects, and
// financial records are always queued for explicit reconciliation and are
// never replayed into their live tables.
func (store *Store) CoalesceOfflineBatch(ctx context.Context, batch OfflineBatch) (OfflineReceipt, bool, error) {
	if batch.Validate() != nil || batch.Body.TenantID != store.tenantID || batch.Body.OrganizationID != store.organizationID || store.machines == nil {
		return OfflineReceipt{}, false, ErrUnauthorized
	}
	publicKey, err := store.machines.ResolveMachineKey(ctx, store.tenantID, batch.Body.MachineID)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || VerifyOfflineBatch(batch, publicKey) != nil {
		return OfflineReceipt{}, false, ErrUnauthorized
	}
	batchCanonical, err := contracts.EncodeCanonical(&batch)
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	batchHash := hashBytes(batchCanonical)
	if existing, existingHash, loadErr := store.loadOfflineReceipt(ctx, batch.Body.ID); loadErr == nil {
		if existingHash != batchHash.Digest {
			return OfflineReceipt{}, false, ErrConflict
		}
		return existing, true, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return OfflineReceipt{}, false, loadErr
	}
	var baseHash string
	err = store.pool.QueryRow(ctx, `SELECT archive_hash FROM workforce_recovery_backups
		WHERE tenant_id=$1 AND organization_id=$2 AND backup_id=$3`, store.tenantID, store.organizationID,
		batch.Body.BaseBackupID).Scan(&baseHash)
	if errors.Is(err, pgx.ErrNoRows) || baseHash != batch.Body.BaseArchiveHash.Digest {
		return OfflineReceipt{}, false, ErrUnauthorized
	}
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "offline-machine", batch.Body.MachineID); err != nil {
		return OfflineReceipt{}, false, err
	}
	var lastSequence uint64
	var lastBatchHash string
	err = tx.QueryRow(ctx, `SELECT last_sequence,last_batch_hash FROM workforce_recovery_offline_machine_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND machine_id=$3 FOR UPDATE`, store.tenantID, store.organizationID,
		batch.Body.MachineID).Scan(&lastSequence, &lastBatchHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OfflineReceipt{}, false, err
	}
	if err == nil && batch.Body.Sequence <= lastSequence {
		if batch.Body.Sequence == lastSequence && batchHash.Digest == lastBatchHash {
			return OfflineReceipt{}, false, ErrConflict
		}
		return OfflineReceipt{}, false, ErrOfflineFork
	}
	contiguous := errors.Is(err, pgx.ErrNoRows) && batch.Body.Sequence == 1 || err == nil && batch.Body.Sequence == lastSequence+1
	results := make([]OfflineItemResult, 0, len(batch.Body.Items))
	for _, item := range batch.Body.Items {
		result, err := store.coalesceOfflineItemTx(ctx, tx, batch, item, contiguous, now)
		if err != nil {
			return OfflineReceipt{}, false, err
		}
		results = append(results, result)
	}
	receipt := OfflineReceipt{Body: OfflineReceiptBody{SchemaVersion: OfflineSchemaVersion,
		BatchID: batch.Body.ID, OrganizationID: store.organizationID, MachineID: batch.Body.MachineID,
		Sequence: batch.Body.Sequence, Results: results, ReconciledAt: now}}
	if err := signSimple(&receipt, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { receipt.Signature = signature }); err != nil {
		return OfflineReceipt{}, false, err
	}
	sealedBatch, err := store.vault.SealRecord(store.offlineBatchAD(batch.Body.ID), batchCanonical)
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	receiptCanonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	receiptHash := hashBytes(receiptCanonical)
	sealedReceipt, err := store.vault.SealRecord(store.offlineReceiptAD(batch.Body.ID), receiptCanonical)
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_offline_batches (
			tenant_id,organization_id,batch_id,machine_id,sequence,base_backup_id,base_archive_hash,
			batch_hash,receipt_hash,machine_key_id,runtime_key_id,sealed_batch,sealed_receipt,contiguous,created_at,reconciled_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	`, store.tenantID, store.organizationID, batch.Body.ID, batch.Body.MachineID, batch.Body.Sequence,
		batch.Body.BaseBackupID, batch.Body.BaseArchiveHash.Digest, batchHash.Digest, receiptHash.Digest,
		batch.Signature.KeyID, receipt.Signature.KeyID, sealedBatch, sealedReceipt, contiguous, batch.Body.CreatedAt, now)
	if err != nil {
		return OfflineReceipt{}, false, err
	}
	if contiguous {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_offline_machine_heads (
				tenant_id,organization_id,machine_id,last_sequence,last_batch_id,last_batch_hash,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (tenant_id,organization_id,machine_id) DO UPDATE SET
				last_sequence=EXCLUDED.last_sequence,last_batch_id=EXCLUDED.last_batch_id,
				last_batch_hash=EXCLUDED.last_batch_hash,updated_at=EXCLUDED.updated_at
		`, store.tenantID, store.organizationID, batch.Body.MachineID, batch.Body.Sequence, batch.Body.ID, batchHash.Digest, now)
		if err != nil {
			return OfflineReceipt{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return OfflineReceipt{}, false, err
	}
	if !contiguous {
		return receipt, false, ErrReconciliationRequired
	}
	return receipt, false, nil
}

// ResolveOfflineReconciliation records a founder-signed disposition for one
// staged conflict. It never applies the staged payload to live authority,
// effect, lease, approval, or financial tables.
func (store *Store) ResolveOfflineReconciliation(ctx context.Context, value OfflineReconciliationResolution) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyOfflineReconciliationResolution(value, store.authority.FounderPublicKey) != nil || value.ResolvedAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash := hashBytes(canonical)
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var machineID, batchID, recordKind, recordID, offlineHash, state string
	var version uint64
	err = tx.QueryRow(ctx, `
		SELECT machine_id,batch_id,record_kind,record_id,version,offline_hash,state
		FROM workforce_recovery_offline_reconciliation
		WHERE tenant_id=$1 AND organization_id=$2 AND reconciliation_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, value.ReconciliationID).Scan(&machineID, &batchID,
		&recordKind, &recordID, &version, &offlineHash, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if machineID != value.MachineID || batchID != string(value.BatchID) || recordKind != value.RecordKind ||
		recordID != value.RecordID || version != value.Version || offlineHash != value.OfflineHash.Digest {
		return false, ErrConflict
	}
	if state != "open" {
		var existingHash string
		err := tx.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_offline_resolutions
			WHERE tenant_id=$1 AND organization_id=$2 AND reconciliation_id=$3`, store.tenantID,
			store.organizationID, value.ReconciliationID).Scan(&existingHash)
		if err != nil || existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	sealed, err := store.vault.SealRecord(store.offlineResolutionAD(value.ID), canonical)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_offline_resolutions (
			tenant_id,organization_id,resolution_id,reconciliation_id,batch_id,machine_id,
			record_kind,record_id,version,offline_hash,decision,evidence_hash,
			canonical_hash,key_id,sealed_resolution,resolved_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
	`, store.tenantID, store.organizationID, value.ID, value.ReconciliationID, value.BatchID,
		value.MachineID, value.RecordKind, value.RecordID, value.Version, value.OfflineHash.Digest,
		value.Decision, value.EvidenceHash.Digest, canonicalHash.Digest, value.Signature.KeyID, sealed, value.ResolvedAt)
	if err != nil {
		return false, err
	}
	resolutionState := "resolved"
	if value.Decision == OfflineResolutionReject {
		resolutionState = "rejected"
	}
	command, err := tx.Exec(ctx, `UPDATE workforce_recovery_offline_reconciliation
		SET state=$1,resolution_id=$2,evidence_hash=$3,resolved_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND reconciliation_id=$7 AND state='open'`,
		resolutionState, value.ID, value.EvidenceHash.Digest, value.ResolvedAt,
		store.tenantID, store.organizationID, value.ReconciliationID)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) coalesceOfflineItemTx(ctx context.Context, tx pgx.Tx, batch OfflineBatch, item OfflineItem, contiguous bool, now time.Time) (OfflineItemResult, error) {
	result := OfflineItemResult{RecordKind: item.RecordKind, RecordID: item.RecordID, Version: item.Version}
	var currentVersion uint64
	var currentHash string
	err := tx.QueryRow(ctx, `SELECT version,content_hash FROM workforce_recovery_offline_record_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND machine_id=$3 AND record_kind=$4 AND record_id=$5 FOR UPDATE`,
		store.tenantID, store.organizationID, batch.Body.MachineID, item.RecordKind, item.RecordID).Scan(&currentVersion, &currentHash)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return result, err
	}
	if !missing {
		hash := contracts.ContentHash{Algorithm: "sha256", Digest: currentHash}
		result.CurrentHash = &hash
	}
	switch {
	case !contiguous:
		result.Disposition = OfflineNeedsReconciliation
	case !missing && item.Version < currentVersion:
		result.Disposition = OfflineStale
	case !missing && item.Version == currentVersion && item.Hash.Digest == currentHash:
		result.Disposition = OfflineDuplicate
	case !missing && item.Version == currentVersion:
		result.Disposition = OfflineConflict
	case missing && item.Version != 1, !missing && item.Version != currentVersion+1:
		result.Disposition = OfflineNeedsReconciliation
	case requiresAuthoritativeReconciliation(item.RecordKind):
		result.Disposition = OfflineNeedsReconciliation
	default:
		result.Disposition = OfflineAccepted
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_offline_records (
			tenant_id,organization_id,machine_id,batch_id,record_kind,record_id,version,
			class,content_hash,payload,disposition,observed_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, batch.Body.MachineID, batch.Body.ID, item.RecordKind,
		item.RecordID, item.Version, item.Class, item.Hash.Digest, item.Payload, result.Disposition, item.ObservedAt, now)
	if err != nil {
		return result, err
	}
	if result.Disposition == OfflineAccepted || result.Disposition == OfflineNeedsReconciliation && contiguous &&
		(missing && item.Version == 1 || !missing && item.Version == currentVersion+1) {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_offline_record_heads (
				tenant_id,organization_id,machine_id,record_kind,record_id,version,content_hash,batch_id,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id,organization_id,machine_id,record_kind,record_id) DO UPDATE SET
				version=EXCLUDED.version,content_hash=EXCLUDED.content_hash,batch_id=EXCLUDED.batch_id,updated_at=EXCLUDED.updated_at
		`, store.tenantID, store.organizationID, batch.Body.MachineID, item.RecordKind, item.RecordID,
			item.Version, item.Hash.Digest, batch.Body.ID, now)
		if err != nil {
			return result, err
		}
	}
	if result.Disposition == OfflineConflict || result.Disposition == OfflineNeedsReconciliation {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_offline_reconciliation (
				tenant_id,organization_id,reconciliation_id,machine_id,batch_id,record_kind,
				record_id,version,offline_hash,current_hash,state,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'open',$11) ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID,
			stableID("offline-reconciliation", batch.Body.MachineID, string(batch.Body.ID), item.RecordKind, item.RecordID, fmt.Sprint(item.Version)),
			batch.Body.MachineID, batch.Body.ID, item.RecordKind, item.RecordID, item.Version,
			item.Hash.Digest, nullableHash(result.CurrentHash), now)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func requiresAuthoritativeReconciliation(recordKind string) bool {
	value := strings.ToLower(recordKind)
	for _, marker := range []string{"authority", "lease", "approval", "policy", "effect", "financial", "connection", "credential", "mandate"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (store *Store) loadOfflineReceipt(ctx context.Context, id OfflineBatchID) (OfflineReceipt, string, error) {
	var batchHash, runtimeKeyID string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `SELECT batch_hash,runtime_key_id,sealed_receipt FROM workforce_recovery_offline_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3`, store.tenantID, store.organizationID, id).Scan(
		&batchHash, &runtimeKeyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return OfflineReceipt{}, "", ErrNotFound
	}
	if err != nil {
		return OfflineReceipt{}, "", err
	}
	opened, err := store.vault.OpenRecord(store.offlineReceiptAD(id), sealed)
	if err != nil {
		return OfflineReceipt{}, "", ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[OfflineReceipt, *OfflineReceipt](opened)
	if err != nil || receipt.Signature.KeyID != runtimeKeyID ||
		VerifyOfflineReceipt(receipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return OfflineReceipt{}, "", ErrUnauthorized
	}
	return receipt, batchHash, nil
}

func (store *Store) loadErasure(ctx context.Context, id ErasureID) (ErasureReceipt, string, error) {
	var directiveHash, runtimeKeyID string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `SELECT directive_hash,key_id,sealed_receipt FROM workforce_recovery_erasures
		WHERE tenant_id=$1 AND organization_id=$2 AND erasure_id=$3`, store.tenantID, store.organizationID, id).Scan(
		&directiveHash, &runtimeKeyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErasureReceipt{}, "", ErrNotFound
	}
	if err != nil {
		return ErasureReceipt{}, "", err
	}
	opened, err := store.vault.OpenRecord(store.erasureReceiptAD(id), sealed)
	if err != nil {
		return ErasureReceipt{}, "", ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[ErasureReceipt, *ErasureReceipt](opened)
	if err != nil || receipt.Signature.KeyID != runtimeKeyID ||
		VerifyErasureReceipt(receipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return ErasureReceipt{}, "", ErrUnauthorized
	}
	return receipt, directiveHash, nil
}

func (store *Store) OpenCircuit(ctx context.Context, scope ScopeRef, resource ResourceKind, reasonCode string) error {
	return store.setCircuit(ctx, scope, resource, "open", reasonCode)
}

func (store *Store) CloseCircuit(ctx context.Context, scope ScopeRef, resource ResourceKind, reasonCode string) error {
	return store.setCircuit(ctx, scope, resource, "closed", reasonCode)
}

func (store *Store) setCircuit(ctx context.Context, scope ScopeRef, resource ResourceKind, state, reasonCode string) error {
	if scope.Validate() != nil || !resource.Valid() || validateToken("circuit reason", reasonCode) != nil ||
		(state != "open" && state != "closed") {
		return ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO workforce_recovery_circuits (
			tenant_id,organization_id,scope_kind,scope_id,resource,state,reason_code,opened_at,closed_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $6='closed' THEN $8 ELSE NULL END,$8)
		ON CONFLICT (tenant_id,organization_id,scope_kind,scope_id,resource) DO UPDATE SET
			state=EXCLUDED.state,reason_code=EXCLUDED.reason_code,
			opened_at=CASE WHEN EXCLUDED.state='open' THEN EXCLUDED.opened_at ELSE workforce_recovery_circuits.opened_at END,
			closed_at=CASE WHEN EXCLUDED.state='closed' THEN EXCLUDED.closed_at ELSE NULL END,
			updated_at=EXCLUDED.updated_at,version=workforce_recovery_circuits.version+1
	`, store.tenantID, store.organizationID, scope.Kind, scope.ID, resource, state, reasonCode, now)
	return err
}

func nullableHash(value *contracts.ContentHash) any {
	if value == nil {
		return nil
	}
	return value.Digest
}

func durationInterval(value time.Duration) string {
	return fmt.Sprintf("%d microseconds", value.Microseconds())
}

func (store *Store) erasureDirectiveAD(id ErasureID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.erasure-directive", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func (store *Store) erasureReceiptAD(id ErasureID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.erasure-receipt", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func (store *Store) offlineBatchAD(id OfflineBatchID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.offline-batch", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: OfflineSchemaVersion}
}

func (store *Store) offlineReceiptAD(id OfflineBatchID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.offline-receipt", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: OfflineSchemaVersion}
}

func (store *Store) offlineResolutionAD(id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.offline-resolution", Stream: strings.Join([]string{string(store.organizationID), id}, "/"), Schema: OfflineSchemaVersion}
}
