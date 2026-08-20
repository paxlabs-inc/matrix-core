package approval

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type BatchApproval struct {
	SchemaVersion              string
	BatchID                    contracts.ApprovalID
	TenantID                   string
	OrganizationID             contracts.OrganizationID
	IntentIDs                  []contracts.IntentID
	IntentSetHash              string
	AggregateCeilingMicrounits uint64
	ExpiresAt                  time.Time
	OwnerID                    contracts.OwnerID
	Signature                  contracts.Signature
}

func (batch BatchApproval) Validate() error {
	hash, err := IntentSetHash(batch.IntentIDs)
	if err != nil {
		return err
	}
	if batch.SchemaVersion != contracts.SchemaVersionV1 ||
		batch.BatchID == "" || strings.TrimSpace(batch.TenantID) == "" ||
		batch.OrganizationID == "" || batch.OwnerID == "" ||
		hash != batch.IntentSetHash || batch.ExpiresAt.IsZero() ||
		batch.ExpiresAt.Location() != time.UTC {
		return fmt.Errorf("approval: invalid batch authority")
	}
	return batch.Signature.Validate()
}

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	ownerID        contracts.OwnerID
	keyID          string
	publicKey      ed25519.PublicKey
	now            func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	keyID string,
	publicKey ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || strings.TrimSpace(tenantID) == "" ||
		organizationID == "" || ownerID == "" || strings.TrimSpace(keyID) == "" ||
		len(publicKey) != ed25519.PublicKeySize || now == nil {
		return nil, fmt.Errorf("approval: complete store authority is required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("approval: Vault user does not match tenant")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, ownerID: ownerID,
		keyID: keyID, publicKey: append(ed25519.PublicKey(nil), publicKey...), now: now,
	}, nil
}

func SignBatch(
	batch *BatchApproval,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if batch == nil || len(privateKey) != ed25519.PrivateKeySize ||
		strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("approval: batch and Ed25519 signing key are required")
	}
	hash, err := IntentSetHash(batch.IntentIDs)
	if err != nil {
		return err
	}
	batch.IntentSetHash = hash
	payload, err := batchSigningBytes(*batch)
	if err != nil {
		return err
	}
	batch.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return batch.Validate()
}

func (store *Store) PublishBatch(
	ctx context.Context,
	batch BatchApproval,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if err := batch.Validate(); err != nil {
		return err
	}
	if batch.TenantID != store.tenantID ||
		batch.OrganizationID != store.organizationID ||
		batch.OwnerID != store.ownerID || batch.Signature.KeyID != store.keyID ||
		!batch.ExpiresAt.After(now) {
		return ErrUnauthorized
	}
	payload, err := batchSigningBytes(batch)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(batch.Signature.Value)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrUnauthorized
	}
	canonical, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	canonicalHash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.ad(batch.BatchID), canonical)
	if err != nil {
		return fmt.Errorf("%w: seal batch: %v", ErrUncertain, err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin batch: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_approval_batches (
			tenant_id,organization_id,batch_id,intent_set_hash,
			aggregate_ceiling_microunits,expires_at,owner_id,key_id,
			signature,canonical_hash,sealed_batch,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, batch.BatchID, batch.IntentSetHash,
		batch.AggregateCeilingMicrounits, batch.ExpiresAt, batch.OwnerID,
		batch.Signature.KeyID, batch.Signature.Value, canonicalHash, sealed, now)
	if err != nil {
		return fmt.Errorf("%w: insert batch: %v", ErrUncertain, err)
	}
	if command.RowsAffected() == 0 {
		var existing string
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_approval_batches
			WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		`, store.tenantID, store.organizationID, batch.BatchID).Scan(&existing); err != nil {
			return fmt.Errorf("%w: inspect batch identity: %v", ErrUncertain, err)
		}
		if existing != canonicalHash {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	for _, intentID := range batch.IntentIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_approval_batch_intents (
				tenant_id,organization_id,batch_id,intent_id
			) VALUES ($1,$2,$3,$4)
		`, store.tenantID, store.organizationID, batch.BatchID, intentID); err != nil {
			return fmt.Errorf("%w: bind batch intent: %v", ErrUncertain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit batch: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) ConsumeBatch(
	ctx context.Context,
	batchID contracts.ApprovalID,
	intentID contracts.IntentID,
	costMicrounits uint64,
	idempotencyKey string,
) error {
	if batchID == "" || intentID == "" || strings.TrimSpace(idempotencyKey) == "" {
		return fmt.Errorf("approval: batch, intent, and idempotency key are required")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin batch consumption: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var ceiling, consumed uint64
	var expiresAt time.Time
	var revokedAt *time.Time
	var canonicalHash string
	var sealed []byte
	err = tx.QueryRow(ctx, `
		SELECT aggregate_ceiling_microunits,consumed_microunits,
		       expires_at,revoked_at,canonical_hash,sealed_batch
		FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, batchID).Scan(
		&ceiling, &consumed, &expiresAt, &revokedAt, &canonicalHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("%w: load batch: %v", ErrUncertain, err)
	}
	if revokedAt != nil || !expiresAt.After(now) {
		return ErrExpired
	}
	// Ceiling, expiry, and membership are taken from the owner-signed sealed
	// batch rather than the rows beside it: a membership row inserted next to a
	// real batch would otherwise extend the owner's approval to an intent they
	// never approved.
	batch, err := OpenSealedBatch(
		store.vault, store.tenantID, store.organizationID, batchID,
		sealed, canonicalHash, store.Authority(),
	)
	if err != nil {
		return ErrUnauthorized
	}
	if !batch.Authorizes(intentID) || !batch.ExpiresAt.After(now) ||
		!batch.ExpiresAt.Equal(expiresAt) ||
		batch.AggregateCeilingMicrounits != ceiling {
		return ErrUnauthorized
	}
	var recorded uint64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_microunits),0)
		FROM workforce_approval_batch_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, store.tenantID, store.organizationID, batchID).Scan(&recorded); err != nil {
		return fmt.Errorf("%w: measure batch consumption: %v", ErrUncertain, err)
	}
	if recorded != consumed {
		return ErrUnauthorized
	}
	ceiling = batch.AggregateCeilingMicrounits
	consumed = recorded
	var priorCost uint64
	err = tx.QueryRow(ctx, `
		SELECT cost_microunits FROM workforce_approval_batch_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		  AND intent_id=$4 AND idempotency_key=$5
	`, store.tenantID, store.organizationID, batchID, intentID,
		idempotencyKey).Scan(&priorCost)
	if err == nil {
		if priorCost != costMicrounits {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: inspect consumption identity: %v", ErrUncertain, err)
	}
	if costMicrounits > ceiling-consumed {
		return ErrCeiling
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_approval_batch_consumptions (
			tenant_id,organization_id,batch_id,intent_id,idempotency_key,
			cost_microunits,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, store.tenantID, store.organizationID, batchID, intentID,
		idempotencyKey, costMicrounits, now); err != nil {
		return fmt.Errorf("%w: insert batch consumption: %v", ErrUncertain, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_approval_batches
		SET consumed_microunits=consumed_microunits+$1
		WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
	`, costMicrounits, store.tenantID, store.organizationID, batchID); err != nil {
		return fmt.Errorf("%w: update batch ceiling: %v", ErrUncertain, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit batch consumption: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) RevokeBatch(
	ctx context.Context,
	batchID contracts.ApprovalID,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE workforce_approval_batches SET revoked_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
		  AND revoked_at IS NULL
	`, now, store.tenantID, store.organizationID, batchID)
	if err != nil {
		return fmt.Errorf("%w: revoke batch: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) AnnotateAsExecutive(
	ctx context.Context,
	annotationID, requestID string,
	seatID contracts.SeatID,
	departmentKind contracts.DepartmentKind,
	annotation string,
) error {
	if strings.TrimSpace(annotationID) == "" || strings.TrimSpace(requestID) == "" ||
		seatID == "" || strings.TrimSpace(annotation) == "" || len(annotation) > 2048 {
		return fmt.Errorf("approval: complete bounded annotation is required")
	}
	if departmentKind != contracts.DepartmentExecutive {
		return ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	var executive int
	err = store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='seat'
		  AND authority_id=$3 AND version=(
		    SELECT latest_version FROM workforce_authority_heads
		    WHERE tenant_id=$1 AND organization_id=$2
		      AND authority_kind='seat' AND authority_id=$3
		  )
	`, store.tenantID, store.organizationID, seatID).Scan(&executive)
	if err != nil {
		return fmt.Errorf("%w: verify executive seat: %v", ErrUncertain, err)
	}
	if executive != 1 {
		return ErrUnauthorized
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO workforce_approval_annotations (
			tenant_id,organization_id,annotation_id,request_id,
			executive_seat_id,annotation,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, annotationID, requestID,
		seatID, annotation, now)
	if err != nil {
		return fmt.Errorf("%w: append executive annotation: %v", ErrUncertain, err)
	}
	return nil
}

func AssertDecisionAuthority(
	departmentKind contracts.DepartmentKind,
	role contracts.SeatRole,
) error {
	if departmentKind == contracts.DepartmentExecutive ||
		role == contracts.SeatAuditor {
		return ErrUnauthorized
	}
	if role != contracts.SeatLead && role != contracts.SeatExecutor {
		return ErrUnauthorized
	}
	return nil
}

// BatchAD is the Vault authenticated-data binding for one durable owner-signed
// batch. Every reader that opens a sealed batch must bind it to this identity.
func BatchAD(
	tenantID string,
	organizationID contracts.OrganizationID,
	batchID contracts.ApprovalID,
) vault.AD {
	return vault.AD{
		User: tenantID, Store: "workforce.approval.batch",
		Stream: string(organizationID) + "/" + string(batchID),
		Schema: contracts.SchemaVersionV1,
	}
}

// Authority is the exact owner identity whose signature makes a batch
// authoritative. Sealing proves only that the tenant's own seal capability
// produced the bytes; spending requires the owner's own signature.
type Authority struct {
	OwnerID   contracts.OwnerID
	KeyID     string
	PublicKey ed25519.PublicKey
}

// Empty reports whether no owner authority was configured at all. An empty
// authority is a valid choice for a caller that never spends approvals; a
// partially filled one is always a misconfiguration.
func (authority Authority) Empty() bool {
	return authority.OwnerID == "" && strings.TrimSpace(authority.KeyID) == "" &&
		len(authority.PublicKey) == 0
}

// Validate accepts either a complete owner authority or none at all.
func (authority Authority) Validate() error {
	if authority.Empty() {
		return nil
	}
	if authority.OwnerID == "" || strings.TrimSpace(authority.KeyID) == "" ||
		len(authority.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("approval: owner authority is incomplete")
	}
	return nil
}

// Clone returns a copy that does not share key material with the caller.
func (authority Authority) Clone() Authority {
	authority.PublicKey = append(ed25519.PublicKey(nil), authority.PublicKey...)
	return authority
}

// OpenSealedBatch recovers one owner-signed batch from its Vault-sealed bytes,
// binds it to the exact tenant, organization, and batch it claims, and verifies
// the owner's signature over it. Callers that authorize spending must take the
// ceiling, expiry, and intent membership from the returned value: the relational
// columns beside the sealed record are projections, and only the verified
// signature — not the seal — proves the owner approved this authority.
func OpenSealedBatch(
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	batchID contracts.ApprovalID,
	sealed []byte,
	canonicalHash string,
	authority Authority,
) (BatchApproval, error) {
	if userVault == nil || len(sealed) == 0 ||
		authority.OwnerID == "" || strings.TrimSpace(authority.KeyID) == "" ||
		len(authority.PublicKey) != ed25519.PublicKeySize {
		return BatchApproval{}, ErrUnauthorized
	}
	canonical, err := userVault.OpenRecord(
		BatchAD(tenantID, organizationID, batchID), sealed,
	)
	if err != nil {
		return BatchApproval{}, fmt.Errorf("%w: open sealed batch: %v", ErrUnauthorized, err)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != canonicalHash {
		return BatchApproval{}, fmt.Errorf("%w: sealed batch hash mismatch", ErrUnauthorized)
	}
	var batch BatchApproval
	if err := json.Unmarshal(canonical, &batch); err != nil {
		return BatchApproval{}, fmt.Errorf("%w: decode sealed batch: %v", ErrUnauthorized, err)
	}
	// Validate recomputes the intent-set hash from the sealed intent list, so a
	// membership row injected beside the sealed record cannot widen authority.
	if err := batch.Validate(); err != nil {
		return BatchApproval{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if batch.TenantID != tenantID || batch.OrganizationID != organizationID ||
		batch.BatchID != batchID || batch.OwnerID != authority.OwnerID ||
		batch.Signature.KeyID != authority.KeyID ||
		batch.Signature.Algorithm != "ed25519" {
		return BatchApproval{}, fmt.Errorf("%w: sealed batch identity mismatch", ErrUnauthorized)
	}
	payload, err := batchSigningBytes(batch)
	if err != nil {
		return BatchApproval{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(batch.Signature.Value)
	if err != nil || !ed25519.Verify(authority.PublicKey, payload, signature) {
		return BatchApproval{}, fmt.Errorf("%w: sealed batch is not owner signed", ErrUnauthorized)
	}
	return batch, nil
}

// Authority returns the exact owner identity this store verifies against.
func (store *Store) Authority() Authority {
	return Authority{
		OwnerID: store.ownerID, KeyID: store.keyID,
		PublicKey: append(ed25519.PublicKey(nil), store.publicKey...),
	}
}

// Authorizes reports whether the owner signed this exact intent into the batch.
func (batch BatchApproval) Authorizes(intentID contracts.IntentID) bool {
	for _, candidate := range batch.IntentIDs {
		if candidate == intentID {
			return true
		}
	}
	return false
}

func batchSigningBytes(batch BatchApproval) ([]byte, error) {
	intents := append([]contracts.IntentID(nil), batch.IntentIDs...)
	sort.Slice(intents, func(left, right int) bool { return intents[left] < intents[right] })
	return json.Marshal(struct {
		SchemaVersion              string
		BatchID                    contracts.ApprovalID
		TenantID                   string
		OrganizationID             contracts.OrganizationID
		IntentIDs                  []contracts.IntentID
		IntentSetHash              string
		AggregateCeilingMicrounits uint64
		ExpiresAt                  time.Time
		OwnerID                    contracts.OwnerID
	}{
		batch.SchemaVersion, batch.BatchID, batch.TenantID, batch.OrganizationID,
		intents, batch.IntentSetHash, batch.AggregateCeilingMicrounits,
		batch.ExpiresAt, batch.OwnerID,
	})
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	return now, nil
}

func (store *Store) ad(batchID contracts.ApprovalID) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.approval.batch",
		Stream: string(store.organizationID) + "/" + string(batchID),
		Schema: contracts.SchemaVersionV1,
	}
}
