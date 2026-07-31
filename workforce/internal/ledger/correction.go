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

// CreateCorrection atomically appends correction truth, walks transitive
// provenance, creates mandatory notices, and pauses every unsafe target.
func (s *Store) CreateCorrection(
	ctx context.Context,
	request CorrectionRequest,
) (CorrectionStatus, error) {
	if err := validateCorrectionRequest(request); err != nil {
		return CorrectionStatus{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return CorrectionStatus{}, err
	}
	prepared, err := s.prepareRecord(request.CorrectionRecord)
	if err != nil {
		return CorrectionStatus{}, err
	}
	for attempt := 1; attempt <= transactionAttempts; attempt++ {
		status, err := s.createCorrectionInTransaction(ctx, request, prepared, now)
		if err == nil || !retryableTransaction(err) {
			return status, err
		}
	}
	return CorrectionStatus{}, fmt.Errorf("ledger correction: transaction retry budget exhausted")
}

func validateCorrectionRequest(request CorrectionRequest) error {
	if err := validateToken("correction_id", string(request.ID)); err != nil {
		return err
	}
	if err := validateToken("source_record_id", string(request.SourceRecordID)); err != nil {
		return err
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return err
	}
	if request.CorrectionRecord.Kind != contracts.RecordCorrection {
		return fmt.Errorf("ledger correction: record kind must be correction")
	}
	foundSource := false
	for _, source := range request.CorrectionRecord.Provenance {
		if source.ID == request.SourceRecordID {
			foundSource = true
			break
		}
	}
	if !foundSource {
		return fmt.Errorf("ledger correction: source must appear in record provenance")
	}
	return nil
}

func (s *Store) createCorrectionInTransaction(
	ctx context.Context,
	request CorrectionRequest,
	prepared preparedRecord,
	now time.Time,
) (CorrectionStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger correction: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := s.appendPreparedTx(ctx, tx, prepared, request.IdempotencyKey, now); err != nil {
		return CorrectionStatus{}, err
	}
	if err := lockCorrection(ctx, tx, s.tenantID, request); err != nil {
		return CorrectionStatus{}, err
	}
	found, err := s.findCorrection(ctx, tx, request)
	if err != nil {
		return CorrectionStatus{}, err
	}
	if !found {
		if err := s.insertCorrection(ctx, tx, request, now); err != nil {
			return CorrectionStatus{}, err
		}
		if err := s.insertCorrectionTargets(ctx, tx, request, now); err != nil {
			return CorrectionStatus{}, err
		}
	}
	status, err := s.correctionStatusTx(ctx, tx, request.CorrectionRecord.OrganizationID, request.ID)
	if err != nil {
		return CorrectionStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger correction: commit: %w", err)
	}
	return status, nil
}

func lockCorrection(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	request CorrectionRequest,
) error {
	key := tenantID + "|" + string(request.CorrectionRecord.OrganizationID) +
		"|correction|" + string(request.ID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("ledger correction: lock identity: %w", err)
	}
	return nil
}

func (s *Store) findCorrection(
	ctx context.Context,
	tx pgx.Tx,
	request CorrectionRequest,
) (bool, error) {
	var correctionRecordID string
	var sourceRecordID string
	var materiallyUnsafe bool
	err := tx.QueryRow(ctx, `
		SELECT correction_record_id, source_record_id, materially_unsafe
		FROM workforce_corrections
		WHERE tenant_id = $1 AND organization_id = $2 AND correction_id = $3
	`, s.tenantID, request.CorrectionRecord.OrganizationID, request.ID).Scan(
		&correctionRecordID, &sourceRecordID, &materiallyUnsafe,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ledger correction: inspect identity: %w", err)
	}
	if correctionRecordID != string(request.CorrectionRecord.ID) ||
		sourceRecordID != string(request.SourceRecordID) ||
		materiallyUnsafe != request.MateriallyUnsafe {
		return false, fmt.Errorf("%w: correction identity reused", ErrConflict)
	}
	return true, nil
}

func (s *Store) insertCorrection(
	ctx context.Context,
	tx pgx.Tx,
	request CorrectionRequest,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_corrections (
			tenant_id, organization_id, correction_id, correction_record_id,
			source_record_id, status, materially_unsafe, created_at
		) VALUES ($1, $2, $3, $4, $5, 'open', $6, $7)
	`, s.tenantID, request.CorrectionRecord.OrganizationID, request.ID,
		request.CorrectionRecord.ID, request.SourceRecordID,
		request.MateriallyUnsafe, now)
	if err != nil {
		return fmt.Errorf("ledger correction: insert: %w", err)
	}
	return nil
}

func (s *Store) insertCorrectionTargets(
	ctx context.Context,
	tx pgx.Tx,
	request CorrectionRequest,
	now time.Time,
) error {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE affected(record_id) AS (
			VALUES ($3::TEXT)
			UNION
			SELECT edge.consumer_record_id
			FROM workforce_provenance_edges edge
			JOIN affected ON affected.record_id = edge.source_record_id
			JOIN workforce_records consumer
			  ON consumer.tenant_id = edge.tenant_id
			 AND consumer.organization_id = edge.organization_id
			 AND consumer.record_id = edge.consumer_record_id
			WHERE edge.tenant_id = $1
			  AND edge.organization_id = $2
			  AND consumer.kind <> 'correction'
		)
		SELECT record.record_id, record.author_seat_id
		FROM affected
		JOIN workforce_records record
		  ON record.tenant_id = $1
		 AND record.organization_id = $2
		 AND record.record_id = affected.record_id
		ORDER BY record.record_id
	`, s.tenantID, request.CorrectionRecord.OrganizationID, request.SourceRecordID)
	if err != nil {
		return fmt.Errorf("ledger correction: compute affected records: %w", err)
	}
	defer rows.Close()
	targets := make([]correctionTarget, 0)
	for rows.Next() {
		var target correctionTarget
		if err := rows.Scan(&target.recordID, &target.seatID); err != nil {
			return fmt.Errorf("ledger correction: scan affected record: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger correction: iterate affected records: %w", err)
	}
	if len(targets) == 0 {
		return fmt.Errorf("%w: correction source", ErrNotFound)
	}
	return s.persistCorrectionTargets(ctx, tx, request, targets, now)
}

type correctionTarget struct {
	recordID contracts.RecordID
	seatID   contracts.SeatID
}

func (s *Store) persistCorrectionTargets(
	ctx context.Context,
	tx pgx.Tx,
	request CorrectionRequest,
	targets []correctionTarget,
	now time.Time,
) error {
	for _, target := range targets {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_correction_targets (
				tenant_id, organization_id, correction_id, affected_record_id,
				consumer_seat_id, state, materially_unsafe, paused
			) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $6)
		`, s.tenantID, request.CorrectionRecord.OrganizationID, request.ID,
			target.recordID, target.seatID, request.MateriallyUnsafe)
		if err != nil {
			return fmt.Errorf("ledger correction: insert target: %w", err)
		}
		noticeID := correctionNoticeID(s.tenantID, request.ID, target.recordID)
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_correction_notices (
				tenant_id, organization_id, notice_id, correction_id,
				affected_record_id, recipient_seat_id, state, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
		`, s.tenantID, request.CorrectionRecord.OrganizationID, noticeID,
			request.ID, target.recordID, target.seatID, now)
		if err != nil {
			return fmt.Errorf("ledger correction: insert notice: %w", err)
		}
	}
	return nil
}

// ReconcileCorrection records one consumer's apply, evidence-backed rejection,
// or escalation and closes the correction only after every target responds.
func (s *Store) ReconcileCorrection(
	ctx context.Context,
	request ReconcileRequest,
) (CorrectionStatus, error) {
	if err := validateReconcileRequest(request); err != nil {
		return CorrectionStatus{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return CorrectionStatus{}, err
	}
	for attempt := 1; attempt <= transactionAttempts; attempt++ {
		status, err := s.reconcileInTransaction(ctx, request, now)
		if err == nil || !retryableTransaction(err) {
			return status, err
		}
	}
	return CorrectionStatus{}, fmt.Errorf("ledger reconciliation: transaction retry budget exhausted")
}

func validateReconcileRequest(request ReconcileRequest) error {
	if err := validateToken("organization_id", string(request.OrganizationID)); err != nil {
		return err
	}
	if err := validateToken("correction_id", string(request.CorrectionID)); err != nil {
		return err
	}
	if err := validateToken("affected_record_id", string(request.AffectedRecordID)); err != nil {
		return err
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return err
	}
	if !request.State.Valid() {
		return fmt.Errorf("ledger reconciliation: invalid state %q", request.State)
	}
	if request.State != ReconciliationApplied && request.EvidenceRecordID == nil {
		return fmt.Errorf("ledger reconciliation: rejection and escalation require evidence")
	}
	return nil
}

func (s *Store) reconcileInTransaction(
	ctx context.Context,
	request ReconcileRequest,
	now time.Time,
) (CorrectionStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger reconciliation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if request.EvidenceRecordID != nil {
		if _, err := s.findMetadata(ctx, tx, request.OrganizationID, *request.EvidenceRecordID); err != nil {
			return CorrectionStatus{}, err
		}
	}
	if err := s.applyResolution(ctx, tx, request, now); err != nil {
		return CorrectionStatus{}, err
	}
	if err := s.closeResolvedCorrection(ctx, tx, request, now); err != nil {
		return CorrectionStatus{}, err
	}
	status, err := s.correctionStatusTx(ctx, tx, request.OrganizationID, request.CorrectionID)
	if err != nil {
		return CorrectionStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger reconciliation: commit: %w", err)
	}
	return status, nil
}

func (s *Store) applyResolution(
	ctx context.Context,
	tx pgx.Tx,
	request ReconcileRequest,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		UPDATE workforce_correction_targets
		SET state = $5, paused = FALSE, evidence_record_id = $6,
			resolution_idempotency_key = $7, resolved_at = $8
		WHERE tenant_id = $1 AND organization_id = $2
		  AND correction_id = $3 AND affected_record_id = $4
		  AND state = 'pending'
	`, s.tenantID, request.OrganizationID, request.CorrectionID,
		request.AffectedRecordID, request.State,
		optionalRecordID(request.EvidenceRecordID), request.IdempotencyKey, now)
	if err != nil {
		return fmt.Errorf("ledger reconciliation: update target: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	return s.verifyExistingResolution(ctx, tx, request)
}

func (s *Store) verifyExistingResolution(
	ctx context.Context,
	tx pgx.Tx,
	request ReconcileRequest,
) error {
	var state string
	var evidence pgtype.Text
	var idempotencyKey pgtype.Text
	err := tx.QueryRow(ctx, `
		SELECT state, evidence_record_id, resolution_idempotency_key
		FROM workforce_correction_targets
		WHERE tenant_id = $1 AND organization_id = $2
		  AND correction_id = $3 AND affected_record_id = $4
	`, s.tenantID, request.OrganizationID, request.CorrectionID,
		request.AffectedRecordID).Scan(&state, &evidence, &idempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("ledger reconciliation: inspect target: %w", err)
	}
	if state == string(request.State) &&
		optionalTextEqual(evidence, optionalRecordID(request.EvidenceRecordID)) &&
		idempotencyKey.Valid && idempotencyKey.String == request.IdempotencyKey {
		return nil
	}
	return ErrCorrectionClosed
}

func (s *Store) closeResolvedCorrection(
	ctx context.Context,
	tx pgx.Tx,
	request ReconcileRequest,
	now time.Time,
) error {
	var pending int
	var escalated int
	err := tx.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE state = 'pending'),
			COUNT(*) FILTER (WHERE state = 'escalated')
		FROM workforce_correction_targets
		WHERE tenant_id = $1 AND organization_id = $2 AND correction_id = $3
	`, s.tenantID, request.OrganizationID, request.CorrectionID).Scan(&pending, &escalated)
	if err != nil {
		return fmt.Errorf("ledger reconciliation: count targets: %w", err)
	}
	if pending != 0 {
		return nil
	}
	status := "closed"
	if escalated != 0 {
		status = "escalated"
	}
	_, err = tx.Exec(ctx, `
		UPDATE workforce_corrections
		SET status = $4, closed_at = $5
		WHERE tenant_id = $1 AND organization_id = $2 AND correction_id = $3
		  AND status = 'open'
	`, s.tenantID, request.OrganizationID, request.CorrectionID, status, now)
	if err != nil {
		return fmt.Errorf("ledger reconciliation: close correction: %w", err)
	}
	return nil
}

// CorrectionStatus returns the durable reconciliation and pause projection.
func (s *Store) CorrectionStatus(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	correctionID contracts.CorrectionID,
) (CorrectionStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger correction status: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	status, err := s.correctionStatusTx(ctx, tx, organizationID, correctionID)
	if err != nil {
		return CorrectionStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger correction status: commit: %w", err)
	}
	return status, nil
}

func (s *Store) correctionStatusTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	correctionID contracts.CorrectionID,
) (CorrectionStatus, error) {
	status := CorrectionStatus{ID: correctionID}
	err := tx.QueryRow(ctx, `
		SELECT correction.status,
			COUNT(*) FILTER (WHERE target.state = 'pending'),
			COUNT(*) FILTER (WHERE target.state = 'applied'),
			COUNT(*) FILTER (WHERE target.state = 'rejected'),
			COUNT(*) FILTER (WHERE target.state = 'escalated'),
			COUNT(*) FILTER (WHERE target.paused)
		FROM workforce_corrections correction
		JOIN workforce_correction_targets target
		  ON target.tenant_id = correction.tenant_id
		 AND target.organization_id = correction.organization_id
		 AND target.correction_id = correction.correction_id
		WHERE correction.tenant_id = $1
		  AND correction.organization_id = $2
		  AND correction.correction_id = $3
		GROUP BY correction.status
	`, s.tenantID, organizationID, correctionID).Scan(
		&status.Status, &status.Pending, &status.Applied,
		&status.Rejected, &status.Escalated, &status.Paused,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CorrectionStatus{}, ErrNotFound
	}
	if err != nil {
		return CorrectionStatus{}, fmt.Errorf("ledger correction status: query: %w", err)
	}
	return status, nil
}

func correctionNoticeID(
	tenantID string,
	correctionID contracts.CorrectionID,
	recordID contracts.RecordID,
) string {
	sum := sha256.Sum256([]byte(tenantID + "|" + string(correctionID) + "|" + string(recordID)))
	return "notice-" + hex.EncodeToString(sum[:16])
}
