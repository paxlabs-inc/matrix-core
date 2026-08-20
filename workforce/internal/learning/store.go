package learning

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	ErrConflict     = errors.New("learning: immutable conflict")
	ErrNotFound     = errors.New("learning: record not found")
	ErrUnauthorized = errors.New("learning: unauthorized")
	ErrIntegrity    = errors.New("learning: integrity failure")
)

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	runtimeKeyID   string
	runtimeKey     ed25519.PublicKey
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	runtimeKeyID string,
	runtimeKey ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" ||
		token(runtimeKeyID) != nil || len(runtimeKey) != ed25519.PublicKeySize || now == nil ||
		userVault.User() != tenantID {
		return nil, fmt.Errorf("learning: durable store dependencies are required")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, organizationID: organizationID,
		runtimeKeyID: runtimeKeyID, runtimeKey: append(ed25519.PublicKey(nil), runtimeKey...), now: now,
	}, nil
}

func (store *Store) RegisterHypothesis(ctx context.Context, value Hypothesis) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.RegisteredAt.After(now) || !value.MaximumDurationAt.After(now) {
		return false, fmt.Errorf("learning: hypothesis is not current and complete")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	key, role, err := store.seatKey(ctx, tx, value.RegistrarSeatID, value.Signature.KeyID, now)
	if err != nil || role == "auditor" || VerifyHypothesis(value, key) != nil {
		return false, ErrUnauthorized
	}
	replay, err := store.persistTx(ctx, tx, "hypothesis", value.ID, value.ID,
		value.InitiativeID, value.RegistrarSeatID, value.Signature.KeyID,
		value.RegisteredAt, HypothesisSchemaVersion, &value)
	if err != nil {
		return false, err
	}
	if !replay {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_learning_hypothesis_heads (
				tenant_id,organization_id,hypothesis_id,initiative_id,state,
				inconclusive_count,review_at,maximum_duration_at,updated_at
			) VALUES ($1,$2,$3,$4,'open',0,$5,$6,$7)
		`, store.tenantID, store.organizationID, value.ID, value.InitiativeID,
			value.ReviewAt, value.MaximumDurationAt, now)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replay, nil
}

func (store *Store) RecordObservation(ctx context.Context, value Observation) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.ValidateAt(now) != nil || value.OrganizationID != store.organizationID {
		return false, fmt.Errorf("learning: observation is invalid")
	}
	hypothesis, err := store.LoadHypothesis(ctx, value.HypothesisID)
	if err != nil || hypothesis.InitiativeID != value.InitiativeID ||
		hypothesis.RegistrarSeatID == value.ProducerSeatID ||
		!thresholdMatches(hypothesis, value) || !sourceAllowed(hypothesis, value.Authority) {
		return false, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	key, role, err := store.seatKey(ctx, tx, value.ProducerSeatID, value.Signature.KeyID, now)
	if err != nil || role == "auditor" || VerifyObservation(value, key, now) != nil {
		return false, ErrUnauthorized
	}
	replay, err := store.persistTx(ctx, tx, "observation", value.ID, value.HypothesisID,
		value.InitiativeID, value.ProducerSeatID, value.Signature.KeyID,
		value.ObservedAt, ObservationSchemaVersion, &value)
	if err == nil && !replay {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_learning_observation_index (
				tenant_id,organization_id,observation_id,hypothesis_id,initiative_id,
				metric_id,metric_version,evidence_id,evidence_hash,authority,
				observed_at,fresh_until,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		`, store.tenantID, store.organizationID, value.ID, value.HypothesisID,
			value.InitiativeID, value.MetricID, value.MetricVersion,
			value.Evidence.ID, value.Evidence.Hash.Digest, value.Authority,
			value.ObservedAt, value.FreshUntil, now)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replay, nil
}

func (store *Store) CommitEvaluation(ctx context.Context, value Evaluation) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.runtimeKeyID || VerifyEvaluation(value, store.runtimeKey) != nil ||
		value.EvaluatedAt.After(now) {
		return false, ErrUnauthorized
	}
	hypothesis, err := store.LoadHypothesis(ctx, value.HypothesisID)
	if err != nil || hypothesis.InitiativeID != value.InitiativeID {
		return false, ErrConflict
	}
	hash, err := contracts.HashCanonical(&hypothesis)
	if err != nil || hash != value.HypothesisHash {
		return false, ErrConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	replay, err := store.persistTx(ctx, tx, "evaluation", value.ID, value.HypothesisID,
		value.InitiativeID, "company-controller", value.Signature.KeyID,
		value.EvaluatedAt, EvaluationSchemaVersion, &value)
	if err == nil && !replay && value.Result == ResultInconclusive {
		_, err = tx.Exec(ctx, `
			UPDATE workforce_learning_hypothesis_heads
			SET inconclusive_count=inconclusive_count+1,updated_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND hypothesis_id=$4 AND state='open'
		`, now, store.tenantID, store.organizationID, value.HypothesisID)
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replay, nil
}

func (store *Store) CommitReview(ctx context.Context, value IndependentReview) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.ReviewedAt.After(now) {
		return false, fmt.Errorf("learning: review is invalid")
	}
	hypothesis, err := store.LoadHypothesis(ctx, value.HypothesisID)
	if err != nil || hypothesis.RegistrarSeatID == value.AuditorSeatID {
		return false, ErrUnauthorized
	}
	evaluation, err := store.LoadEvaluation(ctx, value.EvaluationID)
	hash, hashErr := contracts.HashCanonical(&evaluation)
	if err != nil || hashErr != nil || hash != value.EvaluationHash ||
		evaluation.HypothesisID != value.HypothesisID {
		return false, ErrConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	key, role, err := store.seatKey(ctx, tx, value.AuditorSeatID, value.Signature.KeyID, now)
	if err != nil || role != "auditor" || VerifyReview(value, key) != nil {
		return false, ErrUnauthorized
	}
	replay, err := store.persistTx(ctx, tx, "review", value.ID, value.HypothesisID,
		value.InitiativeID, value.AuditorSeatID, value.Signature.KeyID,
		value.ReviewedAt, ReviewSchemaVersion, &value)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replay, nil
}

func (store *Store) CommitConclusion(ctx context.Context, value Conclusion) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.runtimeKeyID || VerifyConclusion(value, store.runtimeKey) != nil ||
		value.CommittedAt.After(now) {
		return false, ErrUnauthorized
	}
	hypothesis, err := store.LoadHypothesis(ctx, value.HypothesisID)
	evaluation, evaluationErr := store.LoadEvaluation(ctx, value.EvaluationID)
	review, reviewErr := store.LoadReview(ctx, value.ReviewID)
	if err != nil || evaluationErr != nil || reviewErr != nil ||
		hypothesis.InitiativeID != value.InitiativeID ||
		evaluation.HypothesisID != value.HypothesisID || review.HypothesisID != value.HypothesisID ||
		review.EvaluationID != value.EvaluationID {
		return false, ErrConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	replay, err := store.persistTx(ctx, tx, "conclusion", value.ID, value.HypothesisID,
		value.InitiativeID, "company-controller", value.Signature.KeyID,
		value.CommittedAt, ConclusionSchemaVersion, &value)
	if err == nil && !replay {
		command, updateErr := tx.Exec(ctx, `
			UPDATE workforce_learning_hypothesis_heads
			SET state='concluded',conclusion_id=$1,updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4 AND hypothesis_id=$5 AND state='open'
		`, value.ID, now, store.tenantID, store.organizationID, value.HypothesisID)
		if updateErr != nil || command.RowsAffected() != 1 {
			return false, ErrConflict
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_learning_next_cycles (
				tenant_id,organization_id,conclusion_id,hypothesis_id,initiative_id,
				next_action,portfolio_feedback_id,due_at,state,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'planned',$9)
		`, store.tenantID, store.organizationID, value.ID, value.HypothesisID,
			value.InitiativeID, value.NextAction, value.PortfolioFeedbackID,
			value.NextReviewAt, now)
		for _, recordID := range value.SupersededRecordIDs {
			if err != nil {
				break
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO workforce_learning_supersessions (
					tenant_id,organization_id,conclusion_id,record_id,state,created_at
				) VALUES ($1,$2,$3,$4,'pending',$5)
			`, store.tenantID, store.organizationID, value.ID, recordID, now)
		}
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return replay, nil
}

func (store *Store) LoadHypothesis(ctx context.Context, id string) (Hypothesis, error) {
	return loadTyped[Hypothesis, *Hypothesis](ctx, store, "hypothesis", id, HypothesisSchemaVersion)
}

func (store *Store) LoadEvaluation(ctx context.Context, id string) (Evaluation, error) {
	return loadTyped[Evaluation, *Evaluation](ctx, store, "evaluation", id, EvaluationSchemaVersion)
}

func (store *Store) LoadReview(ctx context.Context, id string) (IndependentReview, error) {
	return loadTyped[IndependentReview, *IndependentReview](ctx, store, "review", id, ReviewSchemaVersion)
}

func (store *Store) LoadConclusion(ctx context.Context, id string) (Conclusion, error) {
	return loadTyped[Conclusion, *Conclusion](ctx, store, "conclusion", id, ConclusionSchemaVersion)
}

func (store *Store) ListObservations(ctx context.Context, hypothesisID string) ([]Observation, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT observation_id FROM workforce_learning_observation_index
		WHERE tenant_id=$1 AND organization_id=$2 AND hypothesis_id=$3
		ORDER BY observed_at,observation_id
	`, store.tenantID, store.organizationID, hypothesisID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Observation
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		value, err := loadTyped[Observation, *Observation](ctx, store, "observation", id, ObservationSchemaVersion)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (store *Store) ContaminatedEvidence(
	ctx context.Context,
) (map[contracts.EvidenceID]bool, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT derivation.evidence_id
		FROM workforce_company_state_contamination contamination
		JOIN workforce_company_state_derivations derivation
		  ON derivation.tenant_id=contamination.tenant_id
		 AND derivation.organization_id=contamination.organization_id
		 AND derivation.consumer_record_id=contamination.affected_record_id
		 AND derivation.consumer_version=contamination.affected_version
		WHERE contamination.tenant_id=$1 AND contamination.organization_id=$2
		  AND contamination.state='open'
	`, store.tenantID, store.organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[contracts.EvidenceID]bool)
	for rows.Next() {
		var id contracts.EvidenceID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

type DueHypothesis struct {
	Hypothesis        Hypothesis
	InconclusiveCount uint16
}

func (store *Store) DueHypothesis(ctx context.Context, id string) (DueHypothesis, error) {
	if token(id) != nil {
		return DueHypothesis{}, ErrNotFound
	}
	now, err := store.currentTime()
	if err != nil {
		return DueHypothesis{}, err
	}
	var count uint16
	err = store.pool.QueryRow(ctx, `
		SELECT inconclusive_count FROM workforce_learning_hypothesis_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND hypothesis_id=$3
		  AND state='open' AND review_at<=$4
	`, store.tenantID, store.organizationID, id, now).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return DueHypothesis{}, ErrNotFound
	}
	if err != nil {
		return DueHypothesis{}, err
	}
	hypothesis, err := store.LoadHypothesis(ctx, id)
	if err != nil {
		return DueHypothesis{}, err
	}
	return DueHypothesis{Hypothesis: hypothesis, InconclusiveCount: count}, nil
}

func (store *Store) ListDueHypotheses(ctx context.Context, limit int) ([]DueHypothesis, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("learning: due hypothesis limit is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT hypothesis_id,inconclusive_count
		FROM workforce_learning_hypothesis_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='open' AND review_at<=$3
		ORDER BY review_at,hypothesis_id LIMIT $4
	`, store.tenantID, store.organizationID, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DueHypothesis
	for rows.Next() {
		var id string
		var count uint16
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		hypothesis, err := store.LoadHypothesis(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, DueHypothesis{Hypothesis: hypothesis, InconclusiveCount: count})
	}
	return result, rows.Err()
}

type NextCycle struct {
	ConclusionID        string
	HypothesisID        string
	InitiativeID        string
	Action              NextAction
	PortfolioFeedbackID string
	DueAt               time.Time
}

func (store *Store) ClaimDueNextCycles(ctx context.Context, limit int) ([]NextCycle, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("learning: next-cycle claim limit is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `
		SELECT conclusion_id,hypothesis_id,initiative_id,next_action,
		       portfolio_feedback_id,due_at
		FROM workforce_learning_next_cycles
		WHERE tenant_id=$1 AND organization_id=$2 AND state='planned' AND due_at<=$3
		ORDER BY due_at,conclusion_id LIMIT $4 FOR UPDATE SKIP LOCKED
	`, store.tenantID, store.organizationID, now, limit)
	if err != nil {
		return nil, err
	}
	var result []NextCycle
	for rows.Next() {
		var value NextCycle
		if err := rows.Scan(&value.ConclusionID, &value.HypothesisID,
			&value.InitiativeID, &value.Action, &value.PortfolioFeedbackID,
			&value.DueAt); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, value := range result {
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_learning_next_cycles SET state='claimed',claimed_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND conclusion_id=$4 AND state='planned'
		`, now, store.tenantID, store.organizationID, value.ConclusionID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (store *Store) CompleteNextCycle(
	ctx context.Context,
	conclusionID string,
	dispatched bool,
) error {
	if token(conclusionID) != nil {
		return ErrNotFound
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	state, column := "completed", "completed_at"
	if dispatched {
		state, column = "dispatched", "dispatched_at"
	}
	query := `UPDATE workforce_learning_next_cycles SET state=$1,` + column + `=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND conclusion_id=$5
		  AND state=`
	if dispatched {
		query += `'claimed'`
	} else {
		query += `'dispatched'`
	}
	command, err := store.pool.Exec(ctx, query, state, now, store.tenantID,
		store.organizationID, conclusionID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) persistTx(
	ctx context.Context,
	tx pgx.Tx,
	kind, id, hypothesisID, initiativeID string,
	authorSeatID contracts.SeatID,
	keyID string,
	createdAt time.Time,
	schema string,
	value contracts.Validatable,
) (bool, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.recordAD(kind, id, schema), canonical)
	if err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_learning_records (
			tenant_id,organization_id,record_id,record_kind,hypothesis_id,
			initiative_id,author_seat_id,key_id,canonical_hash,sealed_record,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, id, kind, hypothesisID,
		initiativeID, authorSeatID, keyID, hash, sealed, createdAt)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 1 {
		return false, nil
	}
	var existing string
	if err := tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_learning_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_kind=$4
	`, store.tenantID, store.organizationID, id, kind).Scan(&existing); err != nil || existing != hash {
		return false, ErrConflict
	}
	return true, nil
}

func loadTyped[T any, P interface {
	*T
	contracts.Validatable
}](ctx context.Context, store *Store, kind, id, schema string) (T, error) {
	var zero T
	if token(id) != nil {
		return zero, ErrNotFound
	}
	var expected string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_learning_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_kind=$4
	`, store.tenantID, store.organizationID, id, kind).Scan(&expected, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	opened, err := store.vault.OpenRecord(store.recordAD(kind, id, schema), sealed)
	if err != nil {
		return zero, ErrIntegrity
	}
	sum := sha256.Sum256(opened)
	if hex.EncodeToString(sum[:]) != expected {
		return zero, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[T, P](opened)
	if err != nil {
		return zero, ErrIntegrity
	}
	return value, nil
}

func (store *Store) seatKey(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
	keyID string,
	now time.Time,
) (ed25519.PublicKey, string, error) {
	var key []byte
	var role string
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,seat.seat_role
		FROM workforce_mail_keys key
		JOIN workforce_organization_seats seat
		  ON seat.tenant_id=key.tenant_id AND seat.organization_id=key.organization_id
		 AND seat.seat_id=key.seat_id
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		  AND seat.active=true
		FOR SHARE OF key,seat
	`, store.tenantID, store.organizationID, seatID, keyID, now).Scan(&key, &role)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, "", ErrUnauthorized
	}
	return ed25519.PublicKey(key), role, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !utc(now) {
		return time.Time{}, fmt.Errorf("learning: time source must return UTC")
	}
	return now, nil
}

func (store *Store) recordAD(kind, id, schema string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.learning." + kind,
		Stream: string(store.organizationID) + "/" + id, Schema: schema,
	}
}

func thresholdMatches(hypothesis Hypothesis, observation Observation) bool {
	for _, threshold := range hypothesis.MetricThresholds {
		if threshold.MetricID == observation.MetricID &&
			threshold.MetricVersion == observation.MetricVersion {
			return threshold.DenominatorMetric == "" || observation.Denominator != nil
		}
	}
	return false
}

func sourceAllowed(hypothesis Hypothesis, authority ObservationAuthority) bool {
	for _, kind := range hypothesis.EvidenceSourceKinds {
		if kind == string(authority) {
			return true
		}
	}
	return false
}
