package securityqualification

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
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

var (
	ErrConflict     = errors.New("security qualification: immutable conflict")
	ErrNotFound     = errors.New("security qualification: record not found")
	ErrUnauthorized = errors.New("security qualification: unauthorized")
	ErrIntegrity    = errors.New("security qualification: integrity failure")
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
		return nil, fmt.Errorf("security qualification: durable store dependencies are required")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, organizationID: organizationID,
		runtimeKeyID: runtimeKeyID, runtimeKey: append(ed25519.PublicKey(nil), runtimeKey...), now: now,
	}, nil
}

func (store *Store) CommitThreatModel(ctx context.Context, value ThreatModel) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.CreatedAt.After(now) || !value.ExpiresAt.After(now) {
		return false, fmt.Errorf("security qualification: threat model is invalid")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	key, role, _, err := store.seatKey(ctx, tx, value.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || role == "auditor" || VerifyThreatModel(value, key) != nil {
		return false, ErrUnauthorized
	}
	replay, hash, err := store.persistTx(ctx, tx, "threat_model", value.ID, value.Version,
		value.AuthorSeatID, value.Signature.KeyID, value.CreatedAt, ModelSchemaVersion, &value)
	if err == nil && !replay {
		var currentVersion uint64
		err = tx.QueryRow(ctx, `
			SELECT version FROM workforce_security_qualification_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND threat_model_id=$3 FOR UPDATE
		`, store.tenantID, store.organizationID, value.ID).Scan(&currentVersion)
		switch {
		case errors.Is(err, pgx.ErrNoRows) && value.Version == 1:
			_, err = tx.Exec(ctx, `
				INSERT INTO workforce_security_qualification_heads (
					tenant_id,organization_id,threat_model_id,version,threat_model_hash,
					state,expires_at,updated_at
				) VALUES ($1,$2,$3,$4,$5,'reviewing',$6,$7)
			`, store.tenantID, store.organizationID, value.ID, value.Version,
				hash, value.ExpiresAt, now)
		case err == nil && value.Version == currentVersion+1:
			_, err = tx.Exec(ctx, `
				UPDATE workforce_security_qualification_heads
				SET version=$1,threat_model_hash=$2,state='reviewing',qualification_id=NULL,
				    expires_at=$3,updated_at=$4
				WHERE tenant_id=$5 AND organization_id=$6 AND threat_model_id=$7
			`, value.Version, hash, value.ExpiresAt, now, store.tenantID,
				store.organizationID, value.ID)
		case err == nil:
			return false, ErrConflict
		case err != nil:
			return false, err
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

func (store *Store) CommitReview(ctx context.Context, value BoundaryReview) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.ReviewedAt.After(now) {
		return false, fmt.Errorf("security qualification: review is invalid")
	}
	model, err := store.LoadThreatModel(ctx, value.ThreatModelID)
	modelHash, hashErr := contracts.HashCanonical(&model)
	if err != nil || hashErr != nil || modelHash != value.ThreatModelHash ||
		model.AuthorSeatID == value.ReviewerSeatID || model.Version == 0 {
		return false, ErrConflict
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	key, role, departmentID, err := store.seatKey(
		ctx, tx, value.ReviewerSeatID, value.Signature.KeyID, now,
	)
	if err != nil || role != "auditor" || departmentID != value.ReviewerDepartmentID ||
		VerifyReview(value, key) != nil {
		return false, ErrUnauthorized
	}
	replay, _, err := store.persistTx(ctx, tx, "boundary_review", value.ID, model.Version,
		value.ReviewerSeatID, value.Signature.KeyID, value.ReviewedAt, ReviewSchemaVersion, &value)
	if err == nil && !replay {
		for _, boundary := range value.Boundaries {
			_, err = tx.Exec(ctx, `
				INSERT INTO workforce_security_review_coverage (
					tenant_id,organization_id,threat_model_id,threat_model_version,
					review_id,boundary,reviewer_seat_id,reviewer_department_id,
					outcome,reviewed_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			`, store.tenantID, store.organizationID, value.ThreatModelID, model.Version,
				value.ID, boundary, value.ReviewerSeatID, value.ReviewerDepartmentID,
				value.Outcome, value.ReviewedAt)
			if err != nil {
				break
			}
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

func (store *Store) CommitQualification(ctx context.Context, value Qualification) (bool, error) {
	now, err := store.currentTime()
	if err != nil || value.Validate() != nil || value.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.runtimeKeyID || VerifyQualification(value, store.runtimeKey) != nil ||
		value.QualifiedAt.After(now) || !value.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	model, err := store.LoadThreatModel(ctx, value.ThreatModelID)
	modelHash, hashErr := contracts.HashCanonical(&model)
	if err != nil || hashErr != nil || modelHash != value.ThreatModelHash {
		return false, ErrConflict
	}
	for index, reviewID := range value.ReviewIDs {
		review, err := store.LoadReview(ctx, reviewID)
		hash, hashErr := contracts.HashCanonical(&review)
		if err != nil || hashErr != nil || hash != value.ReviewHashes[index] ||
			review.ThreatModelHash != modelHash || review.Outcome != ReviewApproved {
			return false, ErrConflict
		}
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	replay, _, err := store.persistTx(ctx, tx, "qualification", value.ID, model.Version,
		"company-controller", value.Signature.KeyID, value.QualifiedAt,
		QualificationSchemaVersion, &value)
	if err == nil && !replay {
		command, updateErr := tx.Exec(ctx, `
			UPDATE workforce_security_qualification_heads
			SET state='qualified',qualification_id=$1,expires_at=$2,updated_at=$3
			WHERE tenant_id=$4 AND organization_id=$5 AND threat_model_id=$6
			  AND version=$7 AND threat_model_hash=$8 AND state='reviewing'
		`, value.ID, value.ExpiresAt, now, store.tenantID, store.organizationID,
			value.ThreatModelID, model.Version, modelHash.Digest)
		if updateErr != nil || command.RowsAffected() != 1 {
			return false, ErrConflict
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

func (store *Store) LoadThreatModel(ctx context.Context, id string) (ThreatModel, error) {
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT version FROM workforce_security_qualification_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND threat_model_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ThreatModel{}, ErrNotFound
	}
	if err != nil {
		return ThreatModel{}, err
	}
	return loadTyped[ThreatModel, *ThreatModel](
		ctx, store, "threat_model", id, version, ModelSchemaVersion,
	)
}

func (store *Store) LoadReview(ctx context.Context, id string) (BoundaryReview, error) {
	return loadAnyVersion[BoundaryReview, *BoundaryReview](
		ctx, store, "boundary_review", id, ReviewSchemaVersion,
	)
}

func (store *Store) LoadQualification(ctx context.Context, id string) (Qualification, error) {
	return loadAnyVersion[Qualification, *Qualification](
		ctx, store, "qualification", id, QualificationSchemaVersion,
	)
}

func (store *Store) CurrentQualification(ctx context.Context) (Qualification, error) {
	now, err := store.currentTime()
	if err != nil {
		return Qualification{}, err
	}
	var id string
	err = store.pool.QueryRow(ctx, `
		SELECT qualification_id FROM workforce_security_qualification_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='qualified'
		  AND expires_at>$3
		ORDER BY updated_at DESC LIMIT 1
	`, store.tenantID, store.organizationID, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Qualification{}, ErrNotFound
	}
	if err != nil {
		return Qualification{}, err
	}
	return store.LoadQualification(ctx, id)
}

func (store *Store) persistTx(
	ctx context.Context,
	tx pgx.Tx,
	kind, id string,
	version uint64,
	authorSeatID contracts.SeatID,
	keyID string,
	createdAt time.Time,
	schema string,
	value contracts.Validatable,
) (bool, string, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return false, "", err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.recordAD(kind, id, version, schema), canonical)
	if err != nil {
		return false, "", err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_security_qualification_records (
			tenant_id,organization_id,record_id,record_kind,version,
			author_seat_id,key_id,canonical_hash,sealed_record,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, id, kind, version,
		authorSeatID, keyID, hash, sealed, createdAt)
	if err != nil {
		return false, "", err
	}
	if command.RowsAffected() == 1 {
		return false, hash, nil
	}
	var existing string
	if err := tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_security_qualification_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
		  AND record_kind=$4 AND version=$5
	`, store.tenantID, store.organizationID, id, kind, version).Scan(&existing); err != nil ||
		existing != hash {
		return false, "", ErrConflict
	}
	return true, hash, nil
}

func loadTyped[T any, P interface {
	*T
	contracts.Validatable
}](ctx context.Context, store *Store, kind, id string, version uint64, schema string) (T, error) {
	var zero T
	if token(id) != nil || version == 0 {
		return zero, ErrNotFound
	}
	var expected string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record
		FROM workforce_security_qualification_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
		  AND record_kind=$4 AND version=$5
	`, store.tenantID, store.organizationID, id, kind, version).Scan(&expected, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	opened, err := store.vault.OpenRecord(store.recordAD(kind, id, version, schema), sealed)
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

func loadAnyVersion[T any, P interface {
	*T
	contracts.Validatable
}](ctx context.Context, store *Store, kind, id, schema string) (T, error) {
	var zero T
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT version FROM workforce_security_qualification_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_kind=$4
		ORDER BY version DESC LIMIT 1
	`, store.tenantID, store.organizationID, id, kind).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	return loadTyped[T, P](ctx, store, kind, id, version, schema)
}

func (store *Store) seatKey(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
	keyID string,
	now time.Time,
) (ed25519.PublicKey, string, contracts.DepartmentID, error) {
	var key []byte
	var role string
	var departmentID contracts.DepartmentID
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,seat.seat_role,seat.department_id
		FROM workforce_mail_keys key
		JOIN workforce_organization_seats seat
		  ON seat.tenant_id=key.tenant_id AND seat.organization_id=key.organization_id
		 AND seat.seat_id=key.seat_id
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		  AND seat.active=true FOR SHARE OF key,seat
	`, store.tenantID, store.organizationID, seatID, keyID, now).Scan(
		&key, &role, &departmentID,
	)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, "", "", ErrUnauthorized
	}
	return ed25519.PublicKey(key), role, departmentID, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !utc(now) {
		return time.Time{}, fmt.Errorf("security qualification: time source must return UTC")
	}
	return now, nil
}

func (store *Store) recordAD(kind, id string, version uint64, schema string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.security." + kind,
		Stream: fmt.Sprintf("%s/%s/%d", store.organizationID, id, version), Schema: schema,
	}
}
