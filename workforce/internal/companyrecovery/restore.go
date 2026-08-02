package companyrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

type neutralizationCounts struct {
	CancelledRuntimeLeases     uint64
	InvalidatedAuthorityLeases uint64
	CoalescedWakes             uint64
	QuarantinedEffects         uint64
	QuarantinedExternalState   uint64
}

// Restore performs either a clean deterministic restore or an authorized PITR
// restore. A successful restore remains quarantined until founder-authorized
// reconciliation is recorded with AcknowledgeRestore.
func (store *Store) Restore(ctx context.Context, authorization RestoreAuthorization) (RestoreReceipt, bool, error) {
	startedAt, err := store.currentTime()
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	if authorization.Validate() != nil || authorization.Body.OrganizationID != store.organizationID ||
		authorization.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyRestoreAuthorization(authorization, store.authority.FounderPublicKey) != nil ||
		authorization.Body.RequestedAt.After(startedAt) || !authorization.Body.ExpiresAt.After(startedAt) {
		return RestoreReceipt{}, false, ErrUnauthorized
	}
	if existing, authorizationHash, loadErr := store.loadRestore(ctx, authorization.Body.ID); loadErr == nil {
		providedHash, hashErr := contracts.HashCanonical(&authorization)
		if hashErr != nil || authorizationHash != providedHash.Digest {
			return RestoreReceipt{}, false, ErrConflict
		}
		return existing, true, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return RestoreReceipt{}, false, loadErr
	}
	bundle, err := store.LoadBackup(ctx, authorization.Body.BackupID)
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	manifest := bundle.Manifest
	if manifest.Body.ArchiveHash != authorization.Body.ArchiveHash ||
		manifest.Body.OrganizationID != store.organizationID || manifest.Body.TenantID != store.tenantID ||
		manifest.Body.Scope.Kind != BackupOrganization || manifest.Body.Scope.ID != string(store.organizationID) {
		return RestoreReceipt{}, false, ErrUnauthorized
	}
	if authorization.Body.Mode == RestoreClean && !authorization.Body.TargetAt.Equal(manifest.Body.SnapshotAt) {
		return RestoreReceipt{}, false, ErrUnauthorized
	}
	if authorization.Body.Mode == RestorePITR {
		if store.pitr == nil || strings.TrimSpace(manifest.Body.PITRPoint) == "" ||
			authorization.Body.TargetAt.Before(manifest.Body.SnapshotAt) {
			return RestoreReceipt{}, false, ErrUnauthorized
		}
		if err := store.pitr.Restore(ctx, store.tenantID, store.organizationID, manifest.Body.PITRPoint, authorization.Body.TargetAt); err != nil {
			return RestoreReceipt{}, false, fmt.Errorf("company recovery: PITR restore: %w", err)
		}
	}
	archive, plaintext, err := store.openArchive(bundle)
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	defer zeroBytes(plaintext)
	if err := validateArchiveManifest(archive, manifest); err != nil {
		return RestoreReceipt{}, false, err
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "restore"); err != nil {
		return RestoreReceipt{}, false, err
	}
	if authorization.Body.Mode == RestoreClean {
		if err := store.assertCleanRestoreTargetTx(ctx, tx, archive); err != nil {
			return RestoreReceipt{}, false, err
		}
	}
	if err := store.assertArchiveSchemasTx(ctx, tx, archive); err != nil {
		return RestoreReceipt{}, false, err
	}
	var restoredRows uint64
	if authorization.Body.Mode == RestoreClean {
		orderedTables, err := restoreTableOrder(ctx, tx, archive.Tables)
		if err != nil {
			return RestoreReceipt{}, false, err
		}
		for _, table := range orderedTables {
			inserted, err := store.restoreTableRowsTx(ctx, tx, table)
			if err != nil {
				return RestoreReceipt{}, false, err
			}
			restoredRows += inserted
		}
	} else {
		for _, table := range archive.Tables {
			restoredRows += uint64(len(table.Rows))
		}
	}
	counts, err := store.neutralizeEphemeralTx(ctx, tx, "restore_quarantine", string(authorization.Body.ID), startedAt)
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	released, err := store.releaseAllReservationsTx(ctx, tx, startedAt, "restore_quarantine")
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	_ = released
	completedAt, err := store.currentTime()
	if err != nil {
		return RestoreReceipt{}, false, err
	}
	rtoStatus := "met"
	if completedAt.Sub(startedAt) > manifest.Body.RTO {
		rtoStatus = "breached"
	}
	receipt := RestoreReceipt{Body: RestoreReceiptBody{
		SchemaVersion: SchemaVersion, ID: authorization.Body.ID, OrganizationID: store.organizationID,
		BackupID: authorization.Body.BackupID, ArchiveHash: authorization.Body.ArchiveHash,
		State: RestoreReconciliationRequired, RestoredTables: uint32(len(archive.Tables)), RestoredRows: restoredRows,
		CancelledRuntimeLeases:     counts.CancelledRuntimeLeases,
		InvalidatedAuthorityLeases: counts.InvalidatedAuthorityLeases,
		CoalescedWakes:             counts.CoalescedWakes, QuarantinedEffects: counts.QuarantinedEffects,
		QuarantinedExternalState: counts.QuarantinedExternalState,
		StartedAt:                startedAt, CompletedAt: completedAt, RTO: manifest.Body.RTO, RTOStatus: rtoStatus,
	}}
	if err := signSimple(&receipt, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { receipt.Signature = signature }); err != nil {
		return RestoreReceipt{}, false, err
	}
	if err := store.persistRestoreTx(ctx, tx, authorization, receipt); err != nil {
		return RestoreReceipt{}, false, err
	}
	if rtoStatus == "breached" {
		incident := Incident{SchemaVersion: SchemaVersion,
			ID:             IncidentID(stableID("incident", string(authorization.Body.ID), "rto_breach")),
			OrganizationID: store.organizationID, Kind: IncidentRTOBreach,
			Scope: ScopeRef{Kind: ScopeCompany, ID: string(store.organizationID)}, Resource: ResourceLatencyMicros,
			SafeCode: "rto_breached", RecordKind: "restore", RecordID: string(authorization.Body.ID),
			Observed: uint64(completedAt.Sub(startedAt).Microseconds()), Limit: uint64(manifest.Body.RTO.Microseconds()), CreatedAt: completedAt,
		}
		if err := store.recordIncidentTx(ctx, tx, incident); err != nil {
			return RestoreReceipt{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RestoreReceipt{}, false, err
	}
	return receipt, false, nil
}

type restoreColumn struct {
	name       string
	isIdentity bool
}

func (store *Store) restoreTableRowsTx(ctx context.Context, tx pgx.Tx, table TableArchive) (uint64, error) {
	rows, err := tx.Query(ctx, `
		SELECT column_name,is_identity='YES'
		FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name=$1 AND is_generated='NEVER'
		ORDER BY ordinal_position
	`, table.Name)
	if err != nil {
		return 0, err
	}
	var columns []restoreColumn
	for rows.Next() {
		var column restoreColumn
		if err := rows.Scan(&column.name, &column.isIdentity); err != nil {
			rows.Close()
			return 0, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(columns) == 0 {
		return 0, fmt.Errorf("%w: table %s has no writable columns", ErrSchemaMismatch, table.Name)
	}
	identifier := pgx.Identifier{table.Name}.Sanitize()
	columnNames := make([]string, 0, len(columns))
	selectNames := make([]string, 0, len(columns))
	hasIdentity := false
	for _, column := range columns {
		quoted := pgx.Identifier{column.name}.Sanitize()
		columnNames = append(columnNames, quoted)
		selectNames = append(selectNames, "(jsonb_populate_record(NULL::"+identifier+",$1::jsonb))."+quoted)
		hasIdentity = hasIdentity || column.isIdentity
	}
	overriding := ""
	if hasIdentity {
		overriding = " OVERRIDING SYSTEM VALUE"
	}
	query := fmt.Sprintf(`INSERT INTO %s (%s)%s SELECT %s`, identifier,
		strings.Join(columnNames, ","), overriding, strings.Join(selectNames, ","))
	var inserted uint64
	for _, row := range table.Rows {
		if err := validateArchivedIdentity(row, store.tenantID, store.organizationID); err != nil {
			return 0, err
		}
		command, err := tx.Exec(ctx, query, row)
		if err != nil {
			return 0, fmt.Errorf("company recovery: restore table %s: %w", table.Name, err)
		}
		if command.RowsAffected() != 1 {
			return 0, ErrConflict
		}
		inserted++
	}
	for _, column := range columns {
		if !column.isIdentity {
			continue
		}
		quoted := pgx.Identifier{column.name}.Sanitize()
		sequenceQuery := fmt.Sprintf(`
			SELECT setval(
				pg_get_serial_sequence($1,$2),
				GREATEST(COALESCE(MAX(%s),1),1),
				EXISTS (SELECT 1 FROM %s)
			) FROM %s
		`, quoted, identifier, identifier)
		if _, err := tx.Exec(ctx, sequenceQuery, table.Name, column.name); err != nil {
			return 0, fmt.Errorf("company recovery: advance identity for %s.%s: %w", table.Name, column.name, err)
		}
	}
	return inserted, nil
}

// AcknowledgeRestore clears the fail-closed restore quarantine only after a
// founder-signed restore authorization is presented again with durable,
// non-empty reconciliation evidence.
func (store *Store) AcknowledgeRestore(ctx context.Context, authorization RestoreAuthorization, evidenceHash contracts.ContentHash) (RestoreReceipt, error) {
	if authorization.Validate() != nil || authorization.Body.OrganizationID != store.organizationID ||
		authorization.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyRestoreAuthorization(authorization, store.authority.FounderPublicKey) != nil || evidenceHash.Validate() != nil {
		return RestoreReceipt{}, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RestoreReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var authorizationHash string
	var sealedAuthorizationHash *string
	var sealed []byte
	err = tx.QueryRow(ctx, `
		SELECT authorization_hash,reconciliation_evidence_hash,sealed_receipt
		FROM workforce_recovery_restores
		WHERE tenant_id=$1 AND organization_id=$2 AND restore_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, authorization.Body.ID).Scan(&authorizationHash, &sealedAuthorizationHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return RestoreReceipt{}, ErrNotFound
	}
	if err != nil {
		return RestoreReceipt{}, err
	}
	providedHash, err := contracts.HashCanonical(&authorization)
	if err != nil || providedHash.Digest != authorizationHash {
		return RestoreReceipt{}, ErrUnauthorized
	}
	opened, err := store.vault.OpenRecord(store.restoreReceiptAD(authorization.Body.ID), sealed)
	if err != nil {
		return RestoreReceipt{}, ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[RestoreReceipt, *RestoreReceipt](opened)
	if err != nil || receipt.Signature.KeyID != store.authority.RuntimeKeyID ||
		VerifyRestoreReceipt(receipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return RestoreReceipt{}, ErrUnauthorized
	}
	if receipt.Body.State == RestoreReady {
		if sealedAuthorizationHash == nil || *sealedAuthorizationHash != evidenceHash.Digest {
			return RestoreReceipt{}, ErrConflict
		}
		return receipt, tx.Commit(ctx)
	}
	if receipt.Body.State != RestoreReconciliationRequired {
		return RestoreReceipt{}, ErrConflict
	}
	receipt.Body.State = RestoreReady
	receipt.Body.ReconciliationEvidenceHash = &evidenceHash
	if err := signSimple(&receipt, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { receipt.Signature = signature }); err != nil {
		return RestoreReceipt{}, err
	}
	canonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return RestoreReceipt{}, err
	}
	hash := hashBytes(canonical)
	sealed, err = store.vault.SealRecord(store.restoreReceiptAD(authorization.Body.ID), canonical)
	if err != nil {
		return RestoreReceipt{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return RestoreReceipt{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_restores SET state='ready',receipt_hash=$1,
		sealed_receipt=$2,reconciliation_evidence_hash=$3,reconciled_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND restore_id=$7 AND state='reconciliation_required'`,
		hash.Digest, sealed, evidenceHash.Digest, now, store.tenantID, store.organizationID, authorization.Body.ID)
	if err != nil {
		return RestoreReceipt{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_restore_heads SET state='ready',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND restore_id=$4 AND state='reconciliation_required'`,
		now, store.tenantID, store.organizationID, authorization.Body.ID)
	if err != nil {
		return RestoreReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RestoreReceipt{}, err
	}
	return receipt, nil
}

func (store *Store) openArchive(bundle RecoveryBundle) (RecoveryArchive, []byte, error) {
	key, err := store.vault.OpenRecord(store.archiveKeyAD(bundle.Manifest.Body.BackupID), bundle.SealedArchiveKey)
	if err != nil || len(key) != 32 {
		return RecoveryArchive{}, nil, ErrArchiveErased
	}
	defer zeroBytes(key)
	plaintext, err := decryptArchive(key, store.archiveAAD(bundle.Manifest.Body.BackupID), bundle.EncryptedArchive)
	if err != nil || hashBytes(plaintext) != bundle.Manifest.Body.ArchiveHash {
		zeroBytes(plaintext)
		return RecoveryArchive{}, nil, ErrUnauthorized
	}
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	var archive RecoveryArchive
	if err := decoder.Decode(&archive); err != nil || archive.Validate() != nil {
		zeroBytes(plaintext)
		return RecoveryArchive{}, nil, ErrUnauthorized
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		zeroBytes(plaintext)
		return RecoveryArchive{}, nil, ErrUnauthorized
	}
	return archive, plaintext, nil
}

func validateArchiveManifest(archive RecoveryArchive, manifest BackupManifest) error {
	if archive.BackupID != manifest.Body.BackupID || archive.TenantID != manifest.Body.TenantID ||
		archive.OrganizationID != manifest.Body.OrganizationID || archive.Scope != manifest.Body.Scope ||
		!archive.SnapshotAt.Equal(manifest.Body.SnapshotAt) || archive.WALLSN != manifest.Body.WALLSN ||
		archive.TXSnapshot != manifest.Body.TXSnapshot || len(archive.Tables) != len(manifest.Body.Tables) {
		return ErrSchemaMismatch
	}
	for index := range archive.Tables {
		table, summary := archive.Tables[index], manifest.Body.Tables[index]
		if table.Name != summary.Name || table.SchemaHash != summary.SchemaHash || table.RowsHash != summary.RowsHash ||
			uint64(len(table.Rows)) != summary.RowCount {
			return ErrSchemaMismatch
		}
	}
	return nil
}

func (store *Store) assertCleanRestoreTargetTx(ctx context.Context, tx pgx.Tx, archive RecoveryArchive) error {
	for _, table := range archive.Tables {
		identifier := pgx.Identifier{table.Name}.Sanitize()
		var exists bool
		query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s)`, identifier)
		if err := tx.QueryRow(ctx, query).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: table %s is not empty", ErrRestoreTargetNotClean, table.Name)
		}
	}
	return nil
}

func (store *Store) assertArchiveSchemasTx(ctx context.Context, tx pgx.Tx, archive RecoveryArchive) error {
	for _, table := range archive.Tables {
		schemaJSON, err := tableSchemaDescriptor(ctx, tx, table.Name)
		if err != nil || hashBytes([]byte(schemaJSON)) != table.SchemaHash {
			return fmt.Errorf("%w: table %s", ErrSchemaMismatch, table.Name)
		}
	}
	return nil
}

func restoreTableOrder(ctx context.Context, tx pgx.Tx, tables []TableArchive) ([]TableArchive, error) {
	byName := make(map[string]TableArchive, len(tables))
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		byName[table.Name] = table
		names = append(names, table.Name)
	}
	rows, err := tx.Query(ctx, `
		SELECT child.relname,parent.relname
		FROM pg_constraint constraint_record
		JOIN pg_class child ON child.oid=constraint_record.conrelid
		JOIN pg_class parent ON parent.oid=constraint_record.confrelid
		JOIN pg_namespace namespace_record ON namespace_record.oid=child.relnamespace
		WHERE constraint_record.contype='f' AND namespace_record.nspname=current_schema()
		  AND child.relname=ANY($1::text[]) AND parent.relname=ANY($1::text[])
	`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dependencies := make(map[string]map[string]struct{}, len(tables))
	for _, name := range names {
		dependencies[name] = make(map[string]struct{})
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		if child != parent {
			dependencies[child][parent] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]TableArchive, 0, len(tables))
	remaining := make(map[string]struct{}, len(tables))
	for _, name := range names {
		remaining[name] = struct{}{}
	}
	for len(remaining) > 0 {
		ready := make([]string, 0)
		for name := range remaining {
			blocked := false
			for dependency := range dependencies[name] {
				if _, present := remaining[dependency]; present {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("%w: archive contains cyclic foreign keys", ErrSchemaMismatch)
		}
		slices.Sort(ready)
		for _, name := range ready {
			result = append(result, byName[name])
			delete(remaining, name)
		}
	}
	return result, nil
}

func validateArchivedIdentity(row []byte, tenantID string, organizationID contracts.OrganizationID) error {
	var identity struct {
		TenantID       string                   `json:"tenant_id"`
		OrganizationID contracts.OrganizationID `json:"organization_id"`
	}
	if err := json.Unmarshal(row, &identity); err != nil || identity.TenantID != tenantID || identity.OrganizationID != organizationID {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) persistRestoreTx(ctx context.Context, tx pgx.Tx, authorization RestoreAuthorization, receipt RestoreReceipt) error {
	authorizationCanonical, err := contracts.EncodeCanonical(&authorization)
	if err != nil {
		return err
	}
	authorizationHash := hashBytes(authorizationCanonical)
	sealedAuthorization, err := store.vault.SealRecord(store.restoreAuthorizationAD(authorization.Body.ID), authorizationCanonical)
	if err != nil {
		return err
	}
	receiptCanonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return err
	}
	receiptHash := hashBytes(receiptCanonical)
	sealedReceipt, err := store.vault.SealRecord(store.restoreReceiptAD(authorization.Body.ID), receiptCanonical)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_restores (
			tenant_id,organization_id,restore_id,backup_id,archive_hash,mode,target_at,state,
			authorization_hash,receipt_hash,key_id,sealed_authorization,sealed_receipt,
			restored_tables,restored_rows,cancelled_runtime_leases,invalidated_authority_leases,
			coalesced_wakes,quarantined_effects,quarantined_external_state,rto_micros,rto_status,
			started_at,completed_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'reconciliation_required',$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$23)
	`, store.tenantID, store.organizationID, authorization.Body.ID, authorization.Body.BackupID,
		authorization.Body.ArchiveHash.Digest, authorization.Body.Mode, authorization.Body.TargetAt,
		authorizationHash.Digest, receiptHash.Digest, receipt.Signature.KeyID, sealedAuthorization, sealedReceipt,
		receipt.Body.RestoredTables, receipt.Body.RestoredRows, receipt.Body.CancelledRuntimeLeases,
		receipt.Body.InvalidatedAuthorityLeases, receipt.Body.CoalescedWakes, receipt.Body.QuarantinedEffects,
		receipt.Body.QuarantinedExternalState, receipt.Body.RTO.Microseconds(), receipt.Body.RTOStatus,
		receipt.Body.StartedAt, receipt.Body.CompletedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_restore_heads (tenant_id,organization_id,restore_id,state,updated_at)
		VALUES ($1,$2,$3,'reconciliation_required',$4)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			restore_id=EXCLUDED.restore_id,state='reconciliation_required',updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, authorization.Body.ID, receipt.Body.CompletedAt)
	return err
}

func (store *Store) loadRestore(ctx context.Context, id RestoreID) (RestoreReceipt, string, error) {
	var authorizationHash, keyID string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `SELECT authorization_hash,key_id,sealed_receipt FROM workforce_recovery_restores
		WHERE tenant_id=$1 AND organization_id=$2 AND restore_id=$3`, store.tenantID, store.organizationID, id).Scan(
		&authorizationHash, &keyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return RestoreReceipt{}, "", ErrNotFound
	}
	if err != nil {
		return RestoreReceipt{}, "", err
	}
	opened, err := store.vault.OpenRecord(store.restoreReceiptAD(id), sealed)
	if err != nil {
		return RestoreReceipt{}, "", ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[RestoreReceipt, *RestoreReceipt](opened)
	if err != nil || receipt.Signature.KeyID != keyID ||
		VerifyRestoreReceipt(receipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return RestoreReceipt{}, "", ErrUnauthorized
	}
	return receipt, authorizationHash, nil
}

func (store *Store) neutralizeEphemeralTx(ctx context.Context, tx pgx.Tx, reason, eventID string, now time.Time) (neutralizationCounts, error) {
	if validateToken("neutralization reason", reason) != nil || validateToken("neutralization event_id", eventID) != nil {
		return neutralizationCounts{}, ErrUnauthorized
	}
	var counts neutralizationCounts
	command, err := tx.Exec(ctx, `UPDATE workforce_runtime_leases SET state='cancelled',cancellation_reason=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND state='active'`, reason, store.tenantID, store.organizationID)
	if err != nil {
		return counts, err
	}
	counts.CancelledRuntimeLeases = uint64(command.RowsAffected())
	command, err = tx.Exec(ctx, `
		INSERT INTO workforce_authority_lease_invalidations (
			tenant_id,organization_id,lease_id,authority_kind,authority_id,authority_version,reason,invalidated_at
		)
		SELECT tenant_id,organization_id,lease_id,'mandate',mandate_id,mandate_version,$1,$2
		FROM workforce_authority_leases lease_record
		WHERE tenant_id=$3 AND organization_id=$4 AND expires_at>$2
		  AND NOT EXISTS (SELECT 1 FROM workforce_authority_lease_invalidations invalidation
			WHERE invalidation.tenant_id=lease_record.tenant_id AND invalidation.organization_id=lease_record.organization_id
			  AND invalidation.lease_id=lease_record.lease_id)
		ON CONFLICT DO NOTHING
	`, reason, now, store.tenantID, store.organizationID)
	if err != nil {
		return counts, err
	}
	counts.InvalidatedAuthorityLeases = uint64(command.RowsAffected())
	command, err = tx.Exec(ctx, `
		WITH changed AS (
			UPDATE workforce_scheduled_wakes SET state='coalesced',completed_at=$1,last_error=$2,updated_at=$1
			WHERE tenant_id=$3 AND organization_id=$4 AND state IN ('queued','dispatched') RETURNING wake_id
		)
		INSERT INTO workforce_wake_events (tenant_id,organization_id,wake_id,event_id,event_kind,detail,created_at)
		SELECT $3,$4,wake_id,$5 || ':coalesced:' || wake_id,'coalesced',$2,$1 FROM changed
	`, now, reason, store.tenantID, store.organizationID, eventID)
	if err != nil {
		return counts, err
	}
	counts.CoalescedWakes += uint64(command.RowsAffected())
	command, err = tx.Exec(ctx, `UPDATE workforce_wake_requests SET state='coalesced'
		WHERE tenant_id=$1 AND organization_id=$2 AND state IN ('queued','dispatched')`, store.tenantID, store.organizationID)
	if err != nil {
		return counts, err
	}
	counts.CoalescedWakes += uint64(command.RowsAffected())
	command, err = tx.Exec(ctx, `UPDATE workforce_effect_operations SET state='externally_ambiguous',safe_error_code=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND state IN ('prepared','dispatching')`, reason, now, store.tenantID, store.organizationID)
	if err != nil {
		return counts, err
	}
	counts.QuarantinedEffects = uint64(command.RowsAffected())
	externalCount, err := store.quarantineExternalStateTx(ctx, tx, reason, now)
	if err != nil {
		return counts, err
	}
	counts.QuarantinedExternalState = externalCount
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_shutdown_heads SET state='stopped',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND state='draining'`, now, store.tenantID, store.organizationID)
	return counts, err
}

func (store *Store) quarantineExternalStateTx(ctx context.Context, tx pgx.Tx, reason string, now time.Time) (uint64, error) {
	var total uint64
	updates := []string{
		`UPDATE workforce_external_operation_attempts SET state='ambiguous',safe_code=$1,finished_at=$2 WHERE tenant_id=$3 AND organization_id=$4 AND state='in_flight'`,
		`UPDATE workforce_customer_effect_attempts SET state='ambiguous',safe_code=$1,finished_at=$2 WHERE tenant_id=$3 AND organization_id=$4 AND state='in_flight'`,
		`UPDATE workforce_financial_attempts SET state='ambiguous',safe_code=$1,finished_at=$2 WHERE tenant_id=$3 AND organization_id=$4 AND state='in_flight'`,
		`UPDATE workforce_financial_reservations SET state='ambiguous',updated_at=$2 WHERE tenant_id=$3 AND organization_id=$4 AND state='reserved' AND $1<>''`,
	}
	for _, query := range updates {
		command, err := tx.Exec(ctx, query, reason, now, store.tenantID, store.organizationID)
		if err != nil {
			return total, err
		}
		total += uint64(command.RowsAffected())
	}
	for _, query := range []string{
		`DELETE FROM workforce_external_inflight WHERE tenant_id=$1 AND organization_id=$2`,
		`DELETE FROM workforce_customer_effect_inflight WHERE tenant_id=$1 AND organization_id=$2`,
	} {
		if _, err := tx.Exec(ctx, query, store.tenantID, store.organizationID); err != nil {
			return total, err
		}
	}
	for _, query := range []string{
		`UPDATE workforce_external_connection_heads SET state='scheduled',updated_at=$1 WHERE tenant_id=$2 AND organization_id=$3 AND state='active'`,
		`UPDATE workforce_customer_connection_heads SET state='scheduled',updated_at=$1 WHERE tenant_id=$2 AND organization_id=$3 AND state='active'`,
		`UPDATE workforce_financial_connection_heads SET state='scheduled',updated_at=$1 WHERE tenant_id=$2 AND organization_id=$3 AND state='active'`,
	} {
		command, err := tx.Exec(ctx, query, now, store.tenantID, store.organizationID)
		if err != nil {
			return total, err
		}
		total += uint64(command.RowsAffected())
	}
	return total, nil
}

func (store *Store) restoreAuthorizationAD(id RestoreID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.restore-authorization", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func (store *Store) restoreReceiptAD(id RestoreID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.restore-receipt", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
