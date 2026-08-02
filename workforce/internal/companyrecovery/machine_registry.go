package companyrecovery

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

// RegisterMachineKey installs one founder-signed machine identity version for
// the built-in offline batch verifier.
func (store *Store) RegisterMachineKey(ctx context.Context, value MachineKeyRegistration) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.authority.FounderKeyID ||
		VerifyMachineKeyRegistration(value, store.authority.FounderPublicKey) != nil ||
		value.Body.EffectiveAt.After(now) || !value.Body.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	_, canonicalHash, sealed, err := store.sealCanonical(store.machineKeyAD(value.Body.ID, value.Body.Version), &value)
	if err != nil {
		return false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "machine-key", value.Body.MachineID); err != nil {
		return false, err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_machine_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND machine_key_id=$3 AND version=$4`,
		store.tenantID, store.organizationID, value.Body.ID, value.Body.Version).Scan(&existing)
	if err == nil {
		if existing != canonicalHash.Digest {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var headVersion uint64
	var headHash string
	err = tx.QueryRow(ctx, `SELECT version,content_hash FROM workforce_recovery_machine_key_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND machine_id=$3 FOR UPDATE`, store.tenantID,
		store.organizationID, value.Body.MachineID).Scan(&headVersion, &headHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (value.Body.Version != 1 || value.Body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (value.Body.Version != headVersion+1 || value.Body.Supersedes == nil || value.Body.Supersedes.Digest != headHash):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_machine_keys (
			tenant_id,organization_id,machine_key_id,version,machine_id,key_id,public_key_hash,
			content_hash,canonical_hash,founder_key_id,sealed_registration,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version, value.Body.MachineID,
		value.Body.KeyID, hashBytes(value.Body.PublicKey).Digest, value.ContentHash.Digest,
		canonicalHash.Digest, value.Signature.KeyID, sealed, value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_machine_key_heads (
			tenant_id,organization_id,machine_id,machine_key_id,version,key_id,content_hash,effective_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,machine_id) DO UPDATE SET
			machine_key_id=EXCLUDED.machine_key_id,version=EXCLUDED.version,key_id=EXCLUDED.key_id,
			content_hash=EXCLUDED.content_hash,effective_at=EXCLUDED.effective_at,
			expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.Body.MachineID, value.Body.ID, value.Body.Version,
		value.Body.KeyID, value.ContentHash.Digest, value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

// ResolveMachineKey implements MachineKeyResolver using the built-in,
// founder-signed registry. NewStore installs this resolver by default.
func (store *Store) ResolveMachineKey(ctx context.Context, tenantID, machineID string) (ed25519.PublicKey, error) {
	if tenantID != store.tenantID || validateToken("machine_id", machineID) != nil {
		return nil, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	var registrationID MachineKeyID
	var version uint64
	var keyID, contentHash, publicKeyHash, canonicalHash, founderKeyID string
	var sealed []byte
	err = store.pool.QueryRow(ctx, `
		SELECT head.machine_key_id,head.version,head.key_id,head.content_hash,
		       registration.public_key_hash,registration.canonical_hash,
		       registration.founder_key_id,registration.sealed_registration
		FROM workforce_recovery_machine_key_heads head
		JOIN workforce_recovery_machine_keys registration
		  ON registration.tenant_id=head.tenant_id AND registration.organization_id=head.organization_id
		 AND registration.machine_key_id=head.machine_key_id AND registration.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.machine_id=$3
		  AND head.effective_at<=$4 AND head.expires_at>$4
	`, store.tenantID, store.organizationID, machineID, now).Scan(&registrationID, &version, &keyID,
		&contentHash, &publicKeyHash, &canonicalHash, &founderKeyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	opened, err := store.vault.OpenRecord(store.machineKeyAD(registrationID, version), sealed)
	if err != nil {
		return nil, ErrUnauthorized
	}
	value, err := contracts.DecodeCanonical[MachineKeyRegistration, *MachineKeyRegistration](opened)
	if err != nil || value.Body.MachineID != machineID || value.Body.KeyID != keyID ||
		value.ContentHash.Digest != contentHash || value.Signature.KeyID != founderKeyID ||
		VerifyMachineKeyRegistration(value, store.authority.FounderPublicKey) != nil ||
		hashBytes(value.Body.PublicKey).Digest != publicKeyHash {
		return nil, ErrUnauthorized
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil || hash.Digest != canonicalHash {
		return nil, ErrUnauthorized
	}
	return ed25519.PublicKey(slices.Clone(value.Body.PublicKey)), nil
}

func (store *Store) machineKeyAD(id MachineKeyID, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.machine-key", Stream: strings.Join([]string{string(store.organizationID), string(id), fmt.Sprint(version)}, "/"), Schema: OfflineSchemaVersion}
}
