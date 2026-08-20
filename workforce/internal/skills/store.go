package skills

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	ErrSkillConflict           = errors.New("skills: immutable version conflict")
	ErrSkillNotFound           = errors.New("skills: version not found")
	ErrReauthorizationRequired = errors.New("skills: material version change requires recompilation and reauthorization")
)

type SignedContract struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Contract       Contract                 `json:"contract"`
	EffectiveAt    time.Time                `json:"effective_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value SignedContract) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || value.OrganizationID == "" ||
		value.EffectiveAt.IsZero() || value.EffectiveAt.Location() != time.UTC {
		return fmt.Errorf("skills: signed contract identity or effective time is invalid")
	}
	if err := value.Contract.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	keyID          string
	publicKey      ed25519.PublicKey
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	keyID string,
	publicKey ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" ||
		keyID == "" || len(publicKey) != ed25519.PublicKeySize || now == nil {
		return nil, fmt.Errorf("skills: complete registry authority is required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("skills: Vault user does not match tenant")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, keyID: keyID,
		publicKey: publicKey, now: now,
	}, nil
}

func SignContract(
	value *SignedContract,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("skills: signed contract and owner key are required")
	}
	value.Signature = placeholderSignature(keyID)
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func (store *Store) Publish(ctx context.Context, value SignedContract) error {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("skills: time source must return UTC")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.PublishTx(ctx, tx, value, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PublishTx installs one exact owner-signed skill contract inside an owning
// serializable transaction.
func (store *Store) PublishTx(
	ctx context.Context,
	tx pgx.Tx,
	value SignedContract,
	now time.Time,
) error {
	if tx == nil || now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("skills: transaction and UTC time are required")
	}
	if err := value.Validate(); err != nil {
		return err
	}
	if value.OrganizationID != store.organizationID || value.EffectiveAt.After(now) {
		return ErrSkillConflict
	}
	if err := store.verify(value); err != nil {
		return err
	}
	compatibility, err := compatibilityDigest(value.Contract)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	sealed, err := store.vault.SealRecord(
		store.ad(value.Contract.ID, value.Contract.Version), encoded,
	)
	if err != nil {
		return fmt.Errorf("skills: seal contract: %w", err)
	}
	var existingDigest string
	var existingSealed []byte
	err = tx.QueryRow(ctx, `
		SELECT contract_digest,sealed_contract
		FROM workforce_skill_versions
		WHERE tenant_id=$1 AND organization_id=$2 AND skill_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, value.Contract.ID,
		value.Contract.Version).Scan(&existingDigest, &existingSealed)
	if err == nil {
		existingBytes, openErr := store.vault.OpenRecord(
			store.ad(value.Contract.ID, value.Contract.Version), existingSealed,
		)
		if openErr != nil || existingDigest != value.Contract.Digest.Digest ||
			!bytes.Equal(existingBytes, encoded) {
			return ErrSkillConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var head uint64
	var priorCompatibility string
	err = tx.QueryRow(ctx, `
		SELECT head.latest_version,version.compatibility_digest
		FROM workforce_skill_heads head
		JOIN workforce_skill_versions version
		  ON version.tenant_id=head.tenant_id
		 AND version.organization_id=head.organization_id
		 AND version.skill_id=head.skill_id
		 AND version.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.skill_id=$3
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, value.Contract.ID).Scan(
		&head, &priorCompatibility,
	)
	if !errors.Is(err, pgx.ErrNoRows) && err != nil {
		return err
	}
	if head == 0 && value.Contract.Version != 1 ||
		head != 0 && value.Contract.Version != head+1 {
		return ErrSkillConflict
	}
	material := head != 0 && priorCompatibility != compatibility
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_skill_versions (
			tenant_id,organization_id,skill_id,version,contract_digest,
			compatibility_digest,verifier_digest,material_change,effective_at,
			key_id,sealed_contract,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, store.tenantID, store.organizationID, value.Contract.ID,
		value.Contract.Version, value.Contract.Digest.Digest, compatibility,
		value.Contract.VerifierDigest.Digest, material, value.EffectiveAt,
		value.Signature.KeyID, sealed, now); err != nil {
		return fmt.Errorf("skills: insert version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_skill_heads (
			tenant_id,organization_id,skill_id,latest_version,updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,skill_id)
		DO UPDATE SET latest_version=EXCLUDED.latest_version,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.Contract.ID,
		value.Contract.Version, now); err != nil {
		return fmt.Errorf("skills: update head: %w", err)
	}
	return nil
}

func (store *Store) Load(
	ctx context.Context,
	id contracts.SkillID,
	version uint64,
) (SignedContract, error) {
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_contract
		FROM workforce_skill_versions
		WHERE tenant_id=$1 AND organization_id=$2 AND skill_id=$3 AND version=$4
		  AND effective_at <= $5
	`, store.tenantID, store.organizationID, id, version, store.now()).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignedContract{}, ErrSkillNotFound
	}
	if err != nil {
		return SignedContract{}, err
	}
	opened, err := store.vault.OpenRecord(store.ad(id, version), sealed)
	if err != nil {
		return SignedContract{}, fmt.Errorf("skills: open contract: %w", err)
	}
	var value SignedContract
	if err := json.Unmarshal(opened, &value); err != nil ||
		value.Validate() != nil || value.Contract.ID != id ||
		value.Contract.Version != version || store.verify(value) != nil {
		return SignedContract{}, fmt.Errorf("skills: contract integrity failure")
	}
	return value, nil
}

func (store *Store) LoadCurrent(
	ctx context.Context,
	id contracts.SkillID,
) (SignedContract, error) {
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT latest_version FROM workforce_skill_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND skill_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignedContract{}, ErrSkillNotFound
	}
	if err != nil {
		return SignedContract{}, err
	}
	return store.Load(ctx, id, version)
}

func (store *Store) LoadAccepted(
	ctx context.Context,
	reference contracts.SkillRef,
) (SignedContract, error) {
	value, err := store.Load(ctx, reference.ID, reference.Version)
	if err != nil {
		return SignedContract{}, err
	}
	if value.Contract.Digest != reference.Digest {
		return SignedContract{}, ErrSkillConflict
	}
	var materialNewer bool
	err = store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_skill_versions
			WHERE tenant_id=$1 AND organization_id=$2 AND skill_id=$3
			  AND version>$4 AND material_change=TRUE
		)
	`, store.tenantID, store.organizationID, reference.ID,
		reference.Version).Scan(&materialNewer)
	if err != nil {
		return SignedContract{}, err
	}
	if materialNewer {
		return SignedContract{}, ErrReauthorizationRequired
	}
	return value, nil
}

func (store *Store) verify(value SignedContract) error {
	if value.Signature.KeyID != store.keyID || value.Signature.Algorithm != "ed25519" {
		return ErrSkillConflict
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil {
		return ErrSkillConflict
	}
	copyValue := value
	copyValue.Signature = placeholderSignature(store.keyID)
	payload, err := json.Marshal(copyValue)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrSkillConflict
	}
	return nil
}

func compatibilityDigest(contract Contract) (string, error) {
	copyValue := copyContract(contract)
	copyValue.Version = 0
	copyValue.Digest = contracts.ContentHash{}
	encoded, err := json.Marshal(copyValue)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func placeholderSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func (store *Store) ad(id contracts.SkillID, version uint64) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.skill.contract",
		Stream: string(store.organizationID) + "/" + string(id) + "/" + fmt.Sprint(version),
		Schema: contracts.SchemaVersionV1,
	}
}
