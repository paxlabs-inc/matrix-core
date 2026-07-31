package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"matrix/workforce/internal/contracts"
)

const transactionAttempts = 3

// AppendRecord seals and atomically appends an immutable record, its
// idempotency identity, and every declared derivation edge.
func (s *Store) AppendRecord(ctx context.Context, request AppendRequest) (AppendResult, error) {
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return AppendResult{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return AppendResult{}, err
	}
	prepared, err := s.prepareRecord(request.Record)
	if err != nil {
		return AppendResult{}, err
	}
	for attempt := 1; attempt <= transactionAttempts; attempt++ {
		result, err := s.appendInTransaction(ctx, prepared, request.IdempotencyKey, now)
		if err == nil || !retryableTransaction(err) {
			return result, err
		}
	}
	return AppendResult{}, fmt.Errorf("ledger append: transaction retry budget exhausted")
}

func (s *Store) appendInTransaction(
	ctx context.Context,
	prepared preparedRecord,
	idempotencyKey string,
	now time.Time,
) (AppendResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AppendResult{}, fmt.Errorf("ledger append: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := s.appendPreparedTx(ctx, tx, prepared, idempotencyKey, now)
	if err != nil {
		return AppendResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AppendResult{}, fmt.Errorf("ledger append: commit: %w", err)
	}
	return result, nil
}

func (s *Store) appendPreparedTx(
	ctx context.Context,
	tx pgx.Tx,
	prepared preparedRecord,
	idempotencyKey string,
	now time.Time,
) (AppendResult, error) {
	record := prepared.record
	if err := lockAppendIdentities(ctx, tx, s.tenantID, record, idempotencyKey); err != nil {
		return AppendResult{}, err
	}
	result, found, err := findAppendKey(ctx, tx, s.tenantID, record.OrganizationID, idempotencyKey)
	if err != nil {
		return AppendResult{}, err
	}
	if found {
		if result.RecordID != record.ID || result.CanonicalHash != prepared.canonicalHash {
			return AppendResult{}, fmt.Errorf("%w: idempotency key reused", ErrConflict)
		}
		result.Deduplicated = true
		return result, nil
	}
	found, err = s.findRecordIdentity(ctx, tx, prepared)
	if err != nil {
		return AppendResult{}, err
	}
	if !found {
		if err := s.insertRecord(ctx, tx, prepared); err != nil {
			return AppendResult{}, err
		}
		if err := s.insertProvenance(ctx, tx, prepared.record, now); err != nil {
			return AppendResult{}, err
		}
	}
	if err := insertAppendKey(ctx, tx, s.tenantID, record, prepared, idempotencyKey, now); err != nil {
		return AppendResult{}, err
	}
	return AppendResult{
		RecordID:      record.ID,
		CanonicalHash: prepared.canonicalHash,
		Deduplicated:  found,
	}, nil
}

func lockAppendIdentities(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	record contracts.Record,
	idempotencyKey string,
) error {
	recordLock := tenantID + "|" + string(record.OrganizationID) + "|record|" + string(record.ID)
	keyLock := tenantID + "|" + string(record.OrganizationID) + "|append|" + idempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, recordLock); err != nil {
		return fmt.Errorf("ledger append: lock record identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, keyLock); err != nil {
		return fmt.Errorf("ledger append: lock idempotency identity: %w", err)
	}
	return nil
}

func findAppendKey(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	organizationID contracts.OrganizationID,
	idempotencyKey string,
) (AppendResult, bool, error) {
	var recordID string
	var canonicalHash string
	err := tx.QueryRow(ctx, `
		SELECT record_id, canonical_hash
		FROM workforce_append_keys
		WHERE tenant_id = $1 AND organization_id = $2 AND idempotency_key = $3
	`, tenantID, organizationID, idempotencyKey).Scan(&recordID, &canonicalHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppendResult{}, false, nil
	}
	if err != nil {
		return AppendResult{}, false, fmt.Errorf("ledger append: inspect idempotency key: %w", err)
	}
	return AppendResult{
		RecordID:      contracts.RecordID(recordID),
		CanonicalHash: contracts.ContentHash{Algorithm: "sha256", Digest: canonicalHash},
	}, true, nil
}

func (s *Store) findRecordIdentity(
	ctx context.Context,
	tx pgx.Tx,
	prepared preparedRecord,
) (bool, error) {
	record := prepared.record
	var canonicalHash string
	err := tx.QueryRow(ctx, `
		SELECT canonical_hash
		FROM workforce_records
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, s.tenantID, record.OrganizationID, record.ID).Scan(&canonicalHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ledger append: inspect record identity: %w", err)
	}
	if canonicalHash != prepared.canonicalHash.Digest {
		return false, fmt.Errorf("%w: record identity reused", ErrConflict)
	}
	return true, nil
}

func (s *Store) insertRecord(ctx context.Context, tx pgx.Tx, prepared preparedRecord) error {
	record := prepared.record
	departmentID := optionalDepartmentID(record.DepartmentID)
	accessSeatID := optionalSeatID(record.AccessSeatID)
	projectID := optionalProjectID(record.ProjectID)
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_records (
			tenant_id, organization_id, record_id, kind, author_seat_id,
			department_id, access_seat_id, project_id, purpose, parent_intent_id,
			classification, validity, schema_version,
			content_hash_algorithm, content_hash_digest, canonical_hash,
			sealed_record, created_at, effective_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16, $17, $18, $19
		)
	`, s.tenantID, record.OrganizationID, record.ID, record.Kind, record.AuthorSeatID,
		departmentID, accessSeatID, projectID, record.Purpose, record.ParentIntentID,
		record.Classification, record.Validity, record.SchemaVersion,
		record.ContentHash.Algorithm, record.ContentHash.Digest, prepared.canonicalHash.Digest,
		prepared.sealed, record.CreatedAt, record.EffectiveAt)
	if err != nil {
		return fmt.Errorf("ledger append: insert record: %w", err)
	}
	return nil
}

func (s *Store) insertProvenance(
	ctx context.Context,
	tx pgx.Tx,
	record contracts.Record,
	now time.Time,
) error {
	for _, source := range record.Provenance {
		var algorithm string
		var digest string
		err := tx.QueryRow(ctx, `
			SELECT content_hash_algorithm, content_hash_digest
			FROM workforce_records
			WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
		`, s.tenantID, record.OrganizationID, source.ID).Scan(&algorithm, &digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: provenance source", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("ledger append: inspect provenance source: %w", err)
		}
		if algorithm != source.Hash.Algorithm || digest != source.Hash.Digest {
			return fmt.Errorf("%w: provenance hash mismatch", ErrIntegrity)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_provenance_edges (
				tenant_id, organization_id, source_record_id,
				consumer_record_id, edge_kind, created_at
			) VALUES ($1, $2, $3, $4, 'derivation', $5)
			ON CONFLICT DO NOTHING
		`, s.tenantID, record.OrganizationID, source.ID, record.ID, now); err != nil {
			return fmt.Errorf("ledger append: insert provenance edge: %w", err)
		}
	}
	return nil
}

func insertAppendKey(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	record contracts.Record,
	prepared preparedRecord,
	idempotencyKey string,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_append_keys (
			tenant_id, organization_id, idempotency_key,
			record_id, canonical_hash, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, tenantID, record.OrganizationID, idempotencyKey, record.ID,
		prepared.canonicalHash.Digest, now)
	if err != nil {
		return fmt.Errorf("ledger append: insert idempotency key: %w", err)
	}
	return nil
}

func retryableTransaction(err error) bool {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return false
	}
	return databaseError.Code == "40001" || databaseError.Code == "40P01"
}

func optionalDepartmentID(value *contracts.DepartmentID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func optionalSeatID(value *contracts.SeatID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func optionalProjectID(value *contracts.ProjectID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}
