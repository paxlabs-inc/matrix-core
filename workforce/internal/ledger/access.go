package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"matrix/workforce/internal/contracts"
)

type recordMetadata struct {
	organizationID contracts.OrganizationID
	recordID       contracts.RecordID
	kind           contracts.RecordKind
	authorSeatID   contracts.SeatID
	departmentID   *contracts.DepartmentID
	accessSeatID   *contracts.SeatID
	projectID      *contracts.ProjectID
	purpose        string
	classification contracts.Classification
	schemaVersion  string
	canonicalHash  string
	sealed         []byte
}

// OpenRecord enforces an expiring access grant, authenticates Vault and the
// canonical hash, records the open transactionally, and returns immutable truth.
func (s *Store) OpenRecord(ctx context.Context, request OpenRequest) (contracts.Record, error) {
	if err := validateToken("organization_id", string(request.OrganizationID)); err != nil {
		return contracts.Record{}, err
	}
	if err := validateToken("record_id", string(request.RecordID)); err != nil {
		return contracts.Record{}, err
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return contracts.Record{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return contracts.Record{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return contracts.Record{}, fmt.Errorf("ledger open: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	metadata, err := s.findMetadata(ctx, tx, request.OrganizationID, request.RecordID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return contracts.Record{}, s.commitDenial(ctx, tx, request, now)
		}
		return contracts.Record{}, err
	}
	if err := authorize(metadata, request.Grant, now); err != nil {
		return contracts.Record{}, s.commitDenial(ctx, tx, request, now)
	}
	record, err := s.decryptMetadata(metadata)
	if err != nil {
		return contracts.Record{}, err
	}
	if err := insertAccessEdge(
		ctx, tx, s.tenantID, metadata, nil, AccessOpen,
		request.Grant.SeatID, request.Grant.Purpose, request.IdempotencyKey, now,
	); err != nil {
		return contracts.Record{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Record{}, fmt.Errorf("ledger open: commit: %w", err)
	}
	return record, nil
}

// RecordAccess transactionally records delivery, citation, or derivation and
// adds the corresponding provenance edge when a consumer record is supplied.
func (s *Store) RecordAccess(ctx context.Context, request AccessRequest) error {
	if !request.Action.Valid() || request.Action == AccessOpen {
		return fmt.Errorf("ledger access: action must be delivery, citation, or derivation")
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return err
	}
	now, err := s.currentTime()
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("ledger access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	source, err := s.findMetadata(ctx, tx, request.OrganizationID, request.SourceRecordID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			openRequest := OpenRequest{
				OrganizationID: request.OrganizationID,
				RecordID:       request.SourceRecordID,
				Grant:          request.Grant,
			}
			return s.commitDenial(ctx, tx, openRequest, now)
		}
		return err
	}
	if err := authorize(source, request.Grant, now); err != nil {
		openRequest := OpenRequest{
			OrganizationID: request.OrganizationID,
			RecordID:       request.SourceRecordID,
			Grant:          request.Grant,
		}
		return s.commitDenial(ctx, tx, openRequest, now)
	}
	if err := s.recordAccessTx(ctx, tx, source, request, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ledger access: commit: %w", err)
	}
	return nil
}

func (s *Store) commitDenial(
	ctx context.Context,
	tx pgx.Tx,
	request OpenRequest,
	now time.Time,
) error {
	if err := s.insertDenial(ctx, tx, request, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ledger access: commit denial: %w", err)
	}
	return ErrNotFound
}

func (s *Store) recordAccessTx(
	ctx context.Context,
	tx pgx.Tx,
	source recordMetadata,
	request AccessRequest,
	now time.Time,
) error {
	if (request.Action == AccessCitation || request.Action == AccessDerivation) &&
		request.ConsumerRecordID == nil {
		return fmt.Errorf("ledger access: citation and derivation require a consumer record")
	}
	if request.ConsumerRecordID != nil {
		if _, err := s.findMetadata(ctx, tx, request.OrganizationID, *request.ConsumerRecordID); err != nil {
			return err
		}
	}
	if err := insertAccessEdge(
		ctx, tx, s.tenantID, source, request.ConsumerRecordID, request.Action,
		request.Grant.SeatID, request.Grant.Purpose, request.IdempotencyKey, now,
	); err != nil {
		return err
	}
	if request.ConsumerRecordID == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_provenance_edges (
			tenant_id, organization_id, source_record_id,
			consumer_record_id, edge_kind, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
	`, s.tenantID, source.organizationID, source.recordID,
		*request.ConsumerRecordID, request.Action, now)
	if err != nil {
		return fmt.Errorf("ledger access: insert provenance edge: %w", err)
	}
	return nil
}

func (s *Store) findMetadata(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	recordID contracts.RecordID,
) (recordMetadata, error) {
	var metadata recordMetadata
	var department pgtype.Text
	var accessSeat pgtype.Text
	var project pgtype.Text
	err := tx.QueryRow(ctx, `
		SELECT organization_id, record_id, kind, author_seat_id,
			department_id, access_seat_id, project_id, purpose,
			classification, schema_version, canonical_hash, sealed_record
		FROM workforce_records
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, s.tenantID, organizationID, recordID).Scan(
		&metadata.organizationID, &metadata.recordID, &metadata.kind, &metadata.authorSeatID,
		&department, &accessSeat, &project, &metadata.purpose,
		&metadata.classification, &metadata.schemaVersion,
		&metadata.canonicalHash, &metadata.sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return recordMetadata{}, ErrNotFound
	}
	if err != nil {
		return recordMetadata{}, fmt.Errorf("ledger access: query record: %w", err)
	}
	metadata.departmentID = departmentPointer(department)
	metadata.accessSeatID = seatPointer(accessSeat)
	metadata.projectID = projectPointer(project)
	return metadata, nil
}

func authorize(metadata recordMetadata, grant AccessGrant, now time.Time) error {
	if grant.OrganizationID != metadata.organizationID ||
		grant.SeatID == "" ||
		grant.Purpose != metadata.purpose ||
		grant.ExpiresAt.Location() != time.UTC ||
		!grant.ExpiresAt.After(now) ||
		!allowsClassification(grant.Classifications, metadata.classification) {
		return ErrNotFound
	}
	switch metadata.classification {
	case contracts.ClassificationDepartment:
		if grant.DepartmentID == nil || metadata.departmentID == nil ||
			*grant.DepartmentID != *metadata.departmentID {
			return ErrNotFound
		}
	case contracts.ClassificationSeat:
		if metadata.accessSeatID == nil || grant.SeatID != *metadata.accessSeatID {
			return ErrNotFound
		}
	case contracts.ClassificationProject:
		if grant.ProjectID == nil || metadata.projectID == nil ||
			*grant.ProjectID != *metadata.projectID {
			return ErrNotFound
		}
	case contracts.ClassificationRestricted:
		if !grant.Restricted {
			return ErrNotFound
		}
	}
	return nil
}

func allowsClassification(
	allowed []contracts.Classification,
	required contracts.Classification,
) bool {
	for _, classification := range allowed {
		if classification == required {
			return true
		}
	}
	return false
}

func (s *Store) decryptMetadata(metadata recordMetadata) (contracts.Record, error) {
	record, err := s.openRecord(
		s.recordADFor(
			metadata.organizationID,
			metadata.recordID,
			metadata.kind,
			metadata.projectID,
			metadata.schemaVersion,
		),
		metadata.sealed,
		metadata.canonicalHash,
	)
	if err != nil {
		return contracts.Record{}, err
	}
	if record.OrganizationID != metadata.organizationID || record.ID != metadata.recordID ||
		record.Kind != metadata.kind || record.AuthorSeatID != metadata.authorSeatID {
		return contracts.Record{}, fmt.Errorf("%w: index metadata mismatch", ErrIntegrity)
	}
	return record, nil
}

func (s *Store) insertDenial(
	ctx context.Context,
	tx pgx.Tx,
	request OpenRequest,
	now time.Time,
) error {
	requester := string(request.Grant.SeatID)
	if requester == "" {
		requester = "invalid"
	}
	purpose := request.Grant.Purpose
	if purpose == "" {
		purpose = "unspecified"
	}
	sum := sha256.Sum256([]byte(request.RecordID))
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_access_denials (
			tenant_id, organization_id, requested_record_id_hash,
			requester_seat_id, purpose, reason_code, created_at
		) VALUES ($1, $2, $3, $4, $5, 'denied', $6)
	`, s.tenantID, request.OrganizationID, hex.EncodeToString(sum[:]),
		requester, purpose, now)
	if err != nil {
		return fmt.Errorf("ledger access: audit denial: %w", err)
	}
	return nil
}

func insertAccessEdge(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	source recordMetadata,
	consumerRecordID *contracts.RecordID,
	action AccessAction,
	consumerSeatID contracts.SeatID,
	purpose string,
	idempotencyKey string,
	now time.Time,
) error {
	consumer := optionalRecordID(consumerRecordID)
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_access_edges (
			tenant_id, organization_id, source_record_id, consumer_record_id,
			consumer_seat_id, action, purpose, idempotency_key, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, organization_id, idempotency_key) DO NOTHING
	`, tenantID, source.organizationID, source.recordID, consumer,
		consumerSeatID, action, purpose, idempotencyKey, now)
	if err != nil {
		return fmt.Errorf("ledger access: insert access edge: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var existingSource string
	var existingConsumer pgtype.Text
	var existingSeat string
	var existingAction string
	var existingPurpose string
	err = tx.QueryRow(ctx, `
		SELECT source_record_id, consumer_record_id, consumer_seat_id, action, purpose
		FROM workforce_access_edges
		WHERE tenant_id = $1 AND organization_id = $2 AND idempotency_key = $3
	`, tenantID, source.organizationID, idempotencyKey).Scan(
		&existingSource, &existingConsumer, &existingSeat, &existingAction, &existingPurpose,
	)
	if err != nil {
		return fmt.Errorf("ledger access: inspect idempotency key: %w", err)
	}
	if existingSource != string(source.recordID) ||
		!optionalTextEqual(existingConsumer, consumer) ||
		existingSeat != string(consumerSeatID) ||
		existingAction != string(action) ||
		existingPurpose != purpose {
		return fmt.Errorf("%w: access idempotency key reused", ErrConflict)
	}
	return nil
}

func departmentPointer(value pgtype.Text) *contracts.DepartmentID {
	if !value.Valid {
		return nil
	}
	converted := contracts.DepartmentID(value.String)
	return &converted
}

func seatPointer(value pgtype.Text) *contracts.SeatID {
	if !value.Valid {
		return nil
	}
	converted := contracts.SeatID(value.String)
	return &converted
}

func projectPointer(value pgtype.Text) *contracts.ProjectID {
	if !value.Valid {
		return nil
	}
	converted := contracts.ProjectID(value.String)
	return &converted
}

func optionalRecordID(value *contracts.RecordID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func optionalTextEqual(existing pgtype.Text, expected *string) bool {
	if expected == nil {
		return !existing.Valid
	}
	return existing.Valid && existing.String == *expected
}
