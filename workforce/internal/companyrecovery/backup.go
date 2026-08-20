package companyrecovery

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type archivedTable struct {
	name       string
	scopeField string
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// CreateBackup captures one transactionally consistent, tenant-scoped snapshot,
// encrypts it under a one-use data key, and persists the signed recovery bundle.
func (store *Store) CreateBackup(ctx context.Context, authorization BackupAuthorization) (RecoveryBundle, bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	if authorization.Validate() != nil || authorization.Body.OrganizationID != store.organizationID ||
		authorization.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyBackupAuthorization(authorization, store.authority.FounderPublicKey) != nil ||
		authorization.Body.RequestedAt.After(now) || !authorization.Body.ExpiresAt.After(now) {
		return RecoveryBundle{}, false, ErrUnauthorized
	}
	if authorization.Body.Scope.Kind == BackupOrganization &&
		authorization.Body.Scope.ID != string(store.organizationID) {
		return RecoveryBundle{}, false, ErrUnauthorized
	}
	if existing, existingAuthorizationHash, loadErr := store.loadBackup(ctx, authorization.Body.ID, true); loadErr == nil {
		authorizationHash, hashErr := contracts.HashCanonical(&authorization)
		if hashErr != nil || authorizationHash.Digest != existingAuthorizationHash {
			return RecoveryBundle{}, false, ErrConflict
		}
		if err := store.recordRPOStatus(ctx, existing.Manifest, now, existing.Manifest.Body.RPOObserved); err != nil {
			return RecoveryBundle{}, false, err
		}
		return existing, true, nil
	} else if !errors.Is(loadErr, ErrNotFound) && !errors.Is(loadErr, ErrArchiveErased) {
		return RecoveryBundle{}, false, loadErr
	} else if errors.Is(loadErr, ErrArchiveErased) {
		return RecoveryBundle{}, false, ErrArchiveErased
	}
	policy, err := store.LoadRecoveryPolicy(ctx, true)
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	if policy.Body.PITRRequired && store.pitr == nil {
		return RecoveryBundle{}, false, fmt.Errorf("%w: PITR backend is unavailable", ErrUnauthorized)
	}

	startedAt := now
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var walLSN, txSnapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text, pg_current_snapshot()::text`).Scan(&walLSN, &txSnapshot); err != nil {
		return RecoveryBundle{}, false, fmt.Errorf("company recovery: capture database snapshot identity: %w", err)
	}
	tables, err := store.discoverArchiveTables(ctx, tx, authorization.Body.Scope)
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	archive := RecoveryArchive{
		SchemaVersion:  ArchiveSchemaVersion,
		BackupID:       authorization.Body.ID,
		TenantID:       store.tenantID,
		OrganizationID: store.organizationID,
		Scope:          authorization.Body.Scope,
		SnapshotAt:     startedAt,
		WALLSN:         walLSN,
		TXSnapshot:     txSnapshot,
		Tables:         make([]TableArchive, 0, len(tables)),
	}
	var archiveBytes uint64
	for _, table := range tables {
		captured, captureErr := store.captureTable(ctx, tx, table, authorization.Body.Scope,
			policy.Body.MaximumArchiveBytes-archiveBytes)
		if captureErr != nil {
			return RecoveryBundle{}, false, captureErr
		}
		for _, row := range captured.Rows {
			if archiveBytes > ^uint64(0)-uint64(len(row)) {
				return RecoveryBundle{}, false, fmt.Errorf("company recovery: archive byte count overflow")
			}
			archiveBytes += uint64(len(row))
		}
		if archiveBytes > policy.Body.MaximumArchiveBytes {
			return RecoveryBundle{}, false, fmt.Errorf("company recovery: archive exceeds policy maximum")
		}
		archive.Tables = append(archive.Tables, captured)
	}
	if archive.Validate() != nil {
		return RecoveryBundle{}, false, ErrSchemaMismatch
	}
	plaintext, err := json.Marshal(&archive)
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	if uint64(len(plaintext)) > policy.Body.MaximumArchiveBytes {
		return RecoveryBundle{}, false, fmt.Errorf("company recovery: encoded archive exceeds policy maximum")
	}
	if err := tx.Commit(ctx); err != nil {
		return RecoveryBundle{}, false, err
	}

	archiveHash := hashBytes(plaintext)
	dataKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return RecoveryBundle{}, false, err
	}
	defer zeroBytes(dataKey)
	encryptedArchive, err := encryptArchive(dataKey, store.archiveAAD(authorization.Body.ID), plaintext)
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	sealedArchiveKey, err := store.vault.SealRecord(store.archiveKeyAD(authorization.Body.ID), dataKey)
	if err != nil {
		return RecoveryBundle{}, false, err
	}

	pitrPoint := ""
	if store.pitr != nil {
		pitrPoint, err = store.pitr.Capture(ctx, store.tenantID, store.organizationID, walLSN, startedAt)
		if err != nil {
			return RecoveryBundle{}, false, fmt.Errorf("company recovery: capture PITR point: %w", err)
		}
		if strings.TrimSpace(pitrPoint) == "" {
			return RecoveryBundle{}, false, fmt.Errorf("company recovery: PITR backend returned an empty point")
		}
	}
	completedAt, err := store.currentTime()
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	summaries := make([]TableSummary, 0, len(archive.Tables))
	for _, table := range archive.Tables {
		summaries = append(summaries, TableSummary{
			Name: table.Name, SchemaHash: table.SchemaHash, RowsHash: table.RowsHash, RowCount: uint64(len(table.Rows)),
		})
	}
	rpoStatus, rpoElapsed, err := store.evaluateRPO(ctx, startedAt, policy.Body.RPO, authorization.Body.ID)
	if err != nil {
		return RecoveryBundle{}, false, err
	}
	manifest := BackupManifest{Body: BackupManifestBody{
		SchemaVersion: SchemaVersion, BackupID: authorization.Body.ID, TenantID: store.tenantID,
		OrganizationID: store.organizationID, Scope: authorization.Body.Scope, ArchiveHash: archiveHash,
		WALLSN: walLSN, TXSnapshot: txSnapshot, Tables: summaries, PITRPoint: pitrPoint,
		SnapshotAt: startedAt, CompletedAt: completedAt, RPO: policy.Body.RPO, RPOStatus: rpoStatus,
		RPOObserved: rpoElapsed, RTO: policy.Body.RTO,
	}}
	if err := signSimple(&manifest, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { manifest.Signature = signature }); err != nil {
		return RecoveryBundle{}, false, err
	}
	bundle := RecoveryBundle{Manifest: manifest, EncryptedArchive: encryptedArchive, SealedArchiveKey: sealedArchiveKey}
	if bundle.Validate() != nil {
		return RecoveryBundle{}, false, ErrUnauthorized
	}
	if err := store.persistBackup(ctx, authorization, bundle); err != nil {
		return RecoveryBundle{}, false, err
	}
	if err := store.recordRPOStatus(ctx, manifest, completedAt, rpoElapsed); err != nil {
		return RecoveryBundle{}, false, err
	}
	return bundle, false, nil
}

func (store *Store) LoadBackup(ctx context.Context, id BackupID) (RecoveryBundle, error) {
	bundle, _, err := store.loadBackup(ctx, id, false)
	return bundle, err
}

// ImportBackup installs a previously exported recovery bundle into a clean
// target. The original founder authorization, runtime-signed manifest,
// tenant-bound sealed archive key, and plaintext archive hash are all verified
// before any recovery metadata is written.
func (store *Store) ImportBackup(ctx context.Context, authorization BackupAuthorization, bundle RecoveryBundle) (bool, error) {
	if authorization.Validate() != nil || bundle.Validate() != nil ||
		authorization.Body.OrganizationID != store.organizationID ||
		authorization.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyBackupAuthorization(authorization, store.authority.FounderPublicKey) != nil ||
		authorization.Body.ID != bundle.Manifest.Body.BackupID ||
		authorization.Body.Scope != bundle.Manifest.Body.Scope ||
		bundle.Manifest.Body.TenantID != store.tenantID || bundle.Manifest.Body.OrganizationID != store.organizationID ||
		bundle.Manifest.Signature.KeyID != store.authority.RuntimeKeyID ||
		VerifyBackupManifest(bundle.Manifest, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil ||
		bundle.Manifest.Body.SnapshotAt.Before(authorization.Body.RequestedAt) ||
		!authorization.Body.ExpiresAt.After(bundle.Manifest.Body.SnapshotAt) {
		return false, ErrUnauthorized
	}
	archive, plaintext, err := store.openArchive(bundle)
	if err != nil {
		return false, err
	}
	defer zeroBytes(plaintext)
	if err := validateArchiveManifest(archive, bundle.Manifest); err != nil {
		return false, err
	}
	if existing, authorizationHash, loadErr := store.loadBackup(ctx, authorization.Body.ID, true); loadErr == nil {
		providedHash, hashErr := contracts.HashCanonical(&authorization)
		existingManifestHash, existingManifestErr := contracts.HashCanonical(&existing.Manifest)
		providedManifestHash, providedManifestErr := contracts.HashCanonical(&bundle.Manifest)
		if hashErr != nil || providedHash.Digest != authorizationHash ||
			existingManifestErr != nil || providedManifestErr != nil || existingManifestHash != providedManifestHash {
			return false, ErrConflict
		}
		return true, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return false, loadErr
	}
	if err := store.persistBackup(ctx, authorization, bundle); err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) loadBackup(ctx context.Context, id BackupID, allowMissingKey bool) (RecoveryBundle, string, error) {
	if validateToken("backup_id", string(id)) != nil {
		return RecoveryBundle{}, "", ErrUnauthorized
	}
	var authorizationHash, keyID, state string
	var sealedManifest, encryptedArchive, sealedArchiveKey []byte
	var keyErased bool
	err := store.pool.QueryRow(ctx, `
		SELECT authorization_hash,key_id,sealed_manifest,encrypted_archive,sealed_archive_key,key_erased,state
		FROM workforce_recovery_backups
		WHERE tenant_id=$1 AND organization_id=$2 AND backup_id=$3
	`, store.tenantID, store.organizationID, id).Scan(
		&authorizationHash, &keyID, &sealedManifest, &encryptedArchive, &sealedArchiveKey, &keyErased, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryBundle{}, "", ErrNotFound
	}
	if err != nil {
		return RecoveryBundle{}, "", err
	}
	if state != "completed" || keyErased || len(sealedArchiveKey) == 0 {
		if allowMissingKey {
			return RecoveryBundle{}, authorizationHash, ErrArchiveErased
		}
		return RecoveryBundle{}, "", ErrArchiveErased
	}
	opened, err := store.vault.OpenRecord(store.backupManifestAD(id), sealedManifest)
	if err != nil {
		return RecoveryBundle{}, "", ErrUnauthorized
	}
	manifest, err := contracts.DecodeCanonical[BackupManifest, *BackupManifest](opened)
	if err != nil || manifest.Body.BackupID != id || manifest.Signature.KeyID != keyID ||
		VerifyBackupManifest(manifest, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return RecoveryBundle{}, "", ErrUnauthorized
	}
	bundle := RecoveryBundle{Manifest: manifest, EncryptedArchive: encryptedArchive, SealedArchiveKey: sealedArchiveKey}
	if bundle.Validate() != nil {
		return RecoveryBundle{}, "", ErrUnauthorized
	}
	return bundle, authorizationHash, nil
}

func (store *Store) persistBackup(ctx context.Context, authorization BackupAuthorization, bundle RecoveryBundle) error {
	authorizationCanonical, err := contracts.EncodeCanonical(&authorization)
	if err != nil {
		return err
	}
	authorizationHash := hashBytes(authorizationCanonical)
	sealedAuthorization, err := store.vault.SealRecord(store.backupAuthorizationAD(authorization.Body.ID), authorizationCanonical)
	if err != nil {
		return err
	}
	manifestCanonical, err := contracts.EncodeCanonical(&bundle.Manifest)
	if err != nil {
		return err
	}
	manifestHash := hashBytes(manifestCanonical)
	sealedManifest, err := store.vault.SealRecord(store.backupManifestAD(authorization.Body.ID), manifestCanonical)
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_recovery_backups (
			tenant_id,organization_id,backup_id,scope_kind,scope_id,state,authorization_hash,
			manifest_hash,archive_hash,key_id,sealed_authorization,sealed_manifest,
			encrypted_archive,sealed_archive_key,key_erased,wal_lsn,tx_snapshot,pitr_point,
		snapshot_at,completed_at,rpo_status,created_at
		) VALUES ($1,$2,$3,$4,$5,'completed',$6,$7,$8,$9,$10,$11,$12,$13,FALSE,$14,$15,$16,$17,$18,$19,$18)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, authorization.Body.ID, authorization.Body.Scope.Kind,
		authorization.Body.Scope.ID, authorizationHash.Digest, manifestHash.Digest,
		bundle.Manifest.Body.ArchiveHash.Digest, bundle.Manifest.Signature.KeyID,
		sealedAuthorization, sealedManifest, bundle.EncryptedArchive, bundle.SealedArchiveKey,
		bundle.Manifest.Body.WALLSN, bundle.Manifest.Body.TXSnapshot, nullableString(bundle.Manifest.Body.PITRPoint),
		bundle.Manifest.Body.SnapshotAt, bundle.Manifest.Body.CompletedAt, bundle.Manifest.Body.RPOStatus)
	if err != nil {
		return fmt.Errorf("company recovery: persist backup: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingAuthorizationHash, existingArchiveHash, existingManifestHash string
		if err := store.pool.QueryRow(ctx, `SELECT authorization_hash,archive_hash,manifest_hash FROM workforce_recovery_backups
			WHERE tenant_id=$1 AND organization_id=$2 AND backup_id=$3`, store.tenantID, store.organizationID,
			authorization.Body.ID).Scan(&existingAuthorizationHash, &existingArchiveHash, &existingManifestHash); err != nil {
			return err
		}
		if existingAuthorizationHash != authorizationHash.Digest || existingArchiveHash != bundle.Manifest.Body.ArchiveHash.Digest ||
			existingManifestHash != manifestHash.Digest {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) discoverArchiveTables(ctx context.Context, tx pgx.Tx, scope BackupScope) ([]archivedTable, error) {
	rows, err := tx.Query(ctx, `
		SELECT table_name,
		       bool_or(column_name='tenant_id'), bool_or(column_name='organization_id'),
		       bool_or(column_name='initiative_id'), bool_or(column_name='customer_id'),
		       bool_or(column_name='project_id')
		FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name LIKE 'workforce_%'
		GROUP BY table_name ORDER BY table_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archivedTable
	for rows.Next() {
		var name string
		var hasTenant, hasOrganization, hasInitiative, hasCustomer, hasProject bool
		if err := rows.Scan(&name, &hasTenant, &hasOrganization, &hasInitiative, &hasCustomer, &hasProject); err != nil {
			return nil, err
		}
		if !hasTenant || !hasOrganization || name == "workforce_recovery_backups" ||
			name == "workforce_recovery_restores" || name == "workforce_recovery_restore_heads" ||
			name == "workforce_recovery_qualifications" || name == "workforce_recovery_qualification_heads" {
			continue
		}
		table := archivedTable{name: name}
		switch scope.Kind {
		case BackupOrganization:
		case BackupInitiative:
			if !hasInitiative {
				continue
			}
			table.scopeField = "initiative_id"
		case BackupCustomer:
			if !hasCustomer {
				continue
			}
			table.scopeField = "customer_id"
		case BackupProject:
			if !hasProject {
				continue
			}
			table.scopeField = "project_id"
		default:
			return nil, ErrUnauthorized
		}
		result = append(result, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 || len(result) > MaximumArchiveTables {
		return nil, fmt.Errorf("company recovery: no bounded archive tables matched scope")
	}
	return result, nil
}

func (store *Store) captureTable(ctx context.Context, tx pgx.Tx, table archivedTable, scope BackupScope, maximumBytes uint64) (TableArchive, error) {
	identifier := pgx.Identifier{table.name}.Sanitize()
	schemaJSON, err := tableSchemaDescriptor(ctx, tx, table.name)
	if err != nil {
		return TableArchive{}, err
	}
	query := fmt.Sprintf(`SELECT to_jsonb(row_value)::text FROM %s AS row_value WHERE tenant_id=$1 AND organization_id=$2`, identifier)
	arguments := []any{store.tenantID, store.organizationID}
	if table.scopeField != "" {
		query += " AND " + pgx.Identifier{table.scopeField}.Sanitize() + "=$3"
		arguments = append(arguments, scope.ID)
	}
	query += ` ORDER BY to_jsonb(row_value)::text`
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return TableArchive{}, fmt.Errorf("company recovery: capture %s: %w", table.name, err)
	}
	defer rows.Close()
	values := make([][]byte, 0)
	var capturedBytes uint64
	for rows.Next() {
		var row []byte
		if err := rows.Scan(&row); err != nil {
			return TableArchive{}, err
		}
		rowBytes := uint64(len(row))
		if len(row) == 0 || len(row) > contracts.MaxCanonicalBytes || len(values) >= MaximumRowsPerTable ||
			rowBytes > maximumBytes || capturedBytes > maximumBytes-rowBytes {
			return TableArchive{}, fmt.Errorf("company recovery: table %s exceeds row bounds", table.name)
		}
		capturedBytes += rowBytes
		values = append(values, slices.Clone(row))
	}
	if err := rows.Err(); err != nil {
		return TableArchive{}, err
	}
	return TableArchive{Name: table.name, SchemaHash: hashBytes([]byte(schemaJSON)), RowsHash: hashRows(values), Rows: values}, nil
}

func tableSchemaDescriptor(ctx context.Context, querier queryRower, tableName string) (string, error) {
	var descriptor string
	err := querier.QueryRow(ctx, `
		SELECT jsonb_build_object(
			'columns',COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'ordinal',ordinal_position,'name',column_name,'data_type',data_type,
					'udt',udt_name,'nullable',is_nullable,'default',column_default,
					'is_generated',is_generated,'generation_expression',generation_expression,
					'is_identity',is_identity,'identity_generation',identity_generation
				) ORDER BY ordinal_position)
				FROM information_schema.columns
				WHERE table_schema=current_schema() AND table_name=$1
			),'[]'::jsonb),
			'constraints',COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'name',constraint_record.conname,
					'definition',pg_get_constraintdef(constraint_record.oid,TRUE)
				) ORDER BY constraint_record.conname)
				FROM pg_constraint constraint_record
				JOIN pg_class table_record ON table_record.oid=constraint_record.conrelid
				JOIN pg_namespace namespace_record ON namespace_record.oid=table_record.relnamespace
				WHERE namespace_record.nspname=current_schema() AND table_record.relname=$1
			),'[]'::jsonb),
			'indexes',COALESCE((
				SELECT jsonb_agg(pg_get_indexdef(index_record.indexrelid) ORDER BY index_name.relname)
				FROM pg_index index_record
				JOIN pg_class table_record ON table_record.oid=index_record.indrelid
				JOIN pg_class index_name ON index_name.oid=index_record.indexrelid
				JOIN pg_namespace namespace_record ON namespace_record.oid=table_record.relnamespace
				WHERE namespace_record.nspname=current_schema() AND table_record.relname=$1
			),'[]'::jsonb),
			'triggers',COALESCE((
				SELECT jsonb_agg(pg_get_triggerdef(trigger_record.oid,TRUE) ORDER BY trigger_record.tgname)
				FROM pg_trigger trigger_record
				JOIN pg_class table_record ON table_record.oid=trigger_record.tgrelid
				JOIN pg_namespace namespace_record ON namespace_record.oid=table_record.relnamespace
				WHERE namespace_record.nspname=current_schema() AND table_record.relname=$1
				  AND NOT trigger_record.tgisinternal
			),'[]'::jsonb)
		)::text
	`, tableName).Scan(&descriptor)
	if err != nil {
		return "", err
	}
	return descriptor, nil
}

func (store *Store) evaluateRPO(ctx context.Context, snapshotAt time.Time, rpo time.Duration, backupID BackupID) (string, time.Duration, error) {
	var previous time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT snapshot_at FROM workforce_recovery_backups
		WHERE tenant_id=$1 AND organization_id=$2 AND backup_id<>$3 AND state='completed'
		ORDER BY completed_at DESC LIMIT 1
	`, store.tenantID, store.organizationID, backupID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "baseline", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	elapsed := snapshotAt.Sub(previous.UTC())
	if elapsed <= rpo {
		return "met", elapsed, nil
	}
	return "breached", elapsed, nil
}

func (store *Store) recordRPOStatus(ctx context.Context, manifest BackupManifest, at time.Time, elapsed time.Duration) error {
	if manifest.Body.RPOStatus != "breached" {
		return nil
	}
	incident := Incident{SchemaVersion: SchemaVersion,
		ID:             IncidentID(stableID("incident", string(manifest.Body.BackupID), "rpo_breach")),
		OrganizationID: store.organizationID, Kind: IncidentRPOBreach,
		Scope: ScopeRef{Kind: ScopeCompany, ID: string(store.organizationID)}, Resource: ResourceStorageBytes,
		SafeCode: "rpo_breached", RecordKind: "backup", RecordID: string(manifest.Body.BackupID),
		Observed: uint64(elapsed.Microseconds()),
		Limit:    uint64(manifest.Body.RPO.Microseconds()), CreatedAt: at,
	}
	_, err := store.RecordIncident(ctx, incident)
	return err
}

func encryptArchive(key, associatedData, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plaintext, associatedData)...), nil
}

func decryptArchive(key, associatedData, encrypted []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(encrypted) <= aead.NonceSize() {
		return nil, ErrUnauthorized
	}
	nonce := encrypted[:aead.NonceSize()]
	return aead.Open(nil, nonce, encrypted[aead.NonceSize():], associatedData)
}

func runtimePublicKey(privateKey ed25519.PrivateKey) ed25519.PublicKey {
	return ed25519.PublicKey(privateKey[ed25519.PrivateKeySize-ed25519.PublicKeySize:])
}

func (store *Store) archiveAAD(id BackupID) []byte {
	return []byte(strings.Join([]string{ArchiveSchemaVersion, store.tenantID, string(store.organizationID), string(id)}, "\x00"))
}

func (store *Store) archiveKeyAD(id BackupID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.archive-key", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: ArchiveSchemaVersion}
}

func (store *Store) backupAuthorizationAD(id BackupID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.backup-authorization", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func (store *Store) backupManifestAD(id BackupID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.backup-manifest", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
