package founderprojection

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"centra/workforce/internal/contracts"
)

type Store struct {
	pool           *pgxpool.Pool
	tenantID       string
	organizationID contracts.OrganizationID
	ownerID        contracts.OwnerID
	keyID          string
	privateKey     ed25519.PrivateKey
	publicKey      ed25519.PublicKey
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	tenantID string,
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	keyID string,
	privateKey ed25519.PrivateKey,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || tenantID == "" || organizationID == "" || ownerID == "" ||
		token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize || now == nil {
		return nil, fmt.Errorf("founder projection: durable store dependencies are required")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, tenantID: tenantID, organizationID: organizationID,
		ownerID: ownerID, keyID: keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		publicKey:  append(ed25519.PublicKey(nil), publicKey...), now: now,
	}, nil
}

func (store *Store) Capture(ctx context.Context, draft CaptureDraft) (CurrentReceipt, error) {
	now := store.now()
	if !utc(now) || draft.Validate() != nil || draft.Process.OwnerID != store.ownerID ||
		draft.RenderedAt.After(now) || !draft.ExpiresAt.After(now) {
		return CurrentReceipt{}, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CurrentReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	receiptID := "founder-projection:" + draft.InitiativeID
	version := uint64(1)
	var currentVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT version FROM workforce_founder_projection_receipt_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, draft.InitiativeID).Scan(&currentVersion)
	switch {
	case err == nil:
		version = currentVersion + 1
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return CurrentReceipt{}, err
	}
	receipt := Receipt{
		SchemaVersion: SchemaVersion, ID: receiptID, Version: version,
		OrganizationID: store.organizationID, InitiativeID: draft.InitiativeID,
		Authoritative: draft.Authoritative, Rendered: draft.Rendered,
		Process: draft.Process, Evidence: draft.Evidence,
		RenderedAt: draft.RenderedAt, ExpiresAt: draft.ExpiresAt, CreatedAt: now,
	}
	if err := signReceipt(&receipt, store.keyID, store.privateKey); err != nil {
		return CurrentReceipt{}, err
	}
	counts, err := json.Marshal(receipt.Authoritative.ResourceCounts)
	if err != nil {
		return CurrentReceipt{}, err
	}
	versions, err := json.Marshal(receipt.Authoritative.ResourceVersions)
	if err != nil {
		return CurrentReceipt{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_founder_projection_receipts (
			tenant_id,organization_id,initiative_id,receipt_id,version,
			server_snapshot_hash,rendered_snapshot_hash,snapshot_cursor,rendered_cursor,
			resource_counts,resource_versions,owner_id,process_id,wake_id,
			process_runtime,process_role,fresh_process,render_evidence_id,
			render_evidence_hash,evidence_observed_at,evidence_fresh_until,
			rendered_at,expires_at,canonical_hash,signer_key_id,signature,created_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
			$18,$19,$20,$21,$22,$23,$24,$25,$26,$27
		)
	`, store.tenantID, store.organizationID, receipt.InitiativeID, receipt.ID,
		receipt.Version, receipt.Authoritative.Hash.Digest, receipt.Rendered.Hash.Digest,
		receipt.Authoritative.Cursor, receipt.Rendered.Cursor, counts, versions,
		receipt.Process.OwnerID, receipt.Process.ProcessID, receipt.Process.WakeID,
		receipt.Process.Runtime, receipt.Process.Role, receipt.Process.FreshProcess,
		receipt.Evidence.ID, receipt.Evidence.Hash.Digest, receipt.Evidence.ObservedAt,
		receipt.Evidence.FreshUntil, receipt.RenderedAt, receipt.ExpiresAt,
		receipt.CanonicalHash.Digest, receipt.SignerKeyID, receipt.Signature, receipt.CreatedAt)
	if err != nil {
		return CurrentReceipt{}, err
	}
	if version == 1 {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_founder_projection_receipt_heads (
				tenant_id,organization_id,initiative_id,receipt_id,version,
				canonical_hash,rendered_at,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, receipt.InitiativeID, receipt.ID,
			receipt.Version, receipt.CanonicalHash.Digest, receipt.RenderedAt,
			receipt.ExpiresAt, now)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE workforce_founder_projection_receipt_heads
			SET version=$1,canonical_hash=$2,rendered_at=$3,expires_at=$4,updated_at=$5
			WHERE tenant_id=$6 AND organization_id=$7 AND initiative_id=$8 AND version=$9
		`, receipt.Version, receipt.CanonicalHash.Digest, receipt.RenderedAt,
			receipt.ExpiresAt, now, store.tenantID, store.organizationID,
			receipt.InitiativeID, currentVersion)
	}
	if err != nil {
		return CurrentReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CurrentReceipt{}, err
	}
	return receipt.Current(), nil
}

func (store *Store) Current(
	ctx context.Context,
	initiativeID string,
	at time.Time,
) (CurrentReceipt, error) {
	if token(initiativeID) != nil || !utc(at) {
		return CurrentReceipt{}, ErrNotFound
	}
	receipt, err := store.loadCurrent(ctx, initiativeID)
	if err != nil {
		return CurrentReceipt{}, err
	}
	if verifyReceipt(receipt, store.publicKey) != nil || !receipt.ExpiresAt.After(at) {
		return CurrentReceipt{}, ErrNotFound
	}
	return receipt.Current(), nil
}

func (store *Store) loadCurrent(ctx context.Context, initiativeID string) (Receipt, error) {
	var value Receipt
	var serverHash, renderedHash, cursor, renderedCursor string
	var countsJSON, versionsJSON []byte
	err := store.pool.QueryRow(ctx, `
		SELECT receipt.receipt_id,receipt.version,receipt.initiative_id,
		       receipt.server_snapshot_hash,receipt.rendered_snapshot_hash,
		       receipt.snapshot_cursor,receipt.rendered_cursor,
		       receipt.resource_counts,receipt.resource_versions,
		       receipt.owner_id,receipt.process_id,receipt.wake_id,
		       receipt.process_runtime,receipt.process_role,receipt.fresh_process,
		       receipt.render_evidence_id,receipt.render_evidence_hash,
		       receipt.evidence_observed_at,receipt.evidence_fresh_until,
		       receipt.rendered_at,receipt.expires_at,receipt.created_at,
		       receipt.canonical_hash,receipt.signer_key_id,receipt.signature
		FROM workforce_founder_projection_receipt_heads head
		JOIN workforce_founder_projection_receipts receipt
		  ON receipt.tenant_id=head.tenant_id
		 AND receipt.organization_id=head.organization_id
		 AND receipt.receipt_id=head.receipt_id AND receipt.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.initiative_id=$3
	`, store.tenantID, store.organizationID, initiativeID).Scan(
		&value.ID, &value.Version, &value.InitiativeID, &serverHash, &renderedHash,
		&cursor, &renderedCursor, &countsJSON, &versionsJSON, &value.Process.OwnerID,
		&value.Process.ProcessID, &value.Process.WakeID, &value.Process.Runtime,
		&value.Process.Role, &value.Process.FreshProcess, &value.Evidence.ID,
		&value.Evidence.Hash.Digest, &value.Evidence.ObservedAt, &value.Evidence.FreshUntil,
		&value.RenderedAt, &value.ExpiresAt, &value.CreatedAt,
		&value.CanonicalHash.Digest, &value.SignerKeyID, &value.Signature,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, err
	}
	value.SchemaVersion = SchemaVersion
	value.OrganizationID = store.organizationID
	value.Authoritative.Hash = contracts.ContentHash{Algorithm: "sha256", Digest: serverHash}
	value.Rendered.Hash = contracts.ContentHash{Algorithm: "sha256", Digest: renderedHash}
	value.Authoritative.Cursor = cursor
	value.Rendered.Cursor = renderedCursor
	value.Evidence.Hash.Algorithm = "sha256"
	value.CanonicalHash.Algorithm = "sha256"
	if err := json.Unmarshal(countsJSON, &value.Authoritative.ResourceCounts); err != nil {
		return Receipt{}, err
	}
	if err := json.Unmarshal(versionsJSON, &value.Authoritative.ResourceVersions); err != nil {
		return Receipt{}, err
	}
	value.Rendered.ResourceCounts = cloneMap(value.Authoritative.ResourceCounts)
	value.Rendered.ResourceVersions = cloneMap(value.Authoritative.ResourceVersions)
	return value, nil
}
