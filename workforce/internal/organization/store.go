package organization

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

var (
	ErrConflict       = errors.New("organization: immutable conflict")
	ErrNotFound       = errors.New("organization: record not found")
	ErrIntegrity      = errors.New("organization: record integrity failure")
	ErrUnauthorized   = errors.New("organization: unauthorized")
	ErrMigrationState = errors.New("organization: migration state conflict")
)

type OwnerAuthority struct {
	TenantID       string
	OrganizationID contracts.OrganizationID
	OwnerID        contracts.OwnerID
	KeyID          string
	PublicKey      ed25519.PublicKey
}

func (value OwnerAuthority) Validate() error {
	if strings.TrimSpace(value.TenantID) == "" || value.OrganizationID == "" ||
		value.OwnerID == "" || value.KeyID == "" || len(value.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("organization: owner authority is incomplete")
	}
	return nil
}

type Store struct {
	pool  *pgxpool.Pool
	vault *vault.UserVault
	owner OwnerAuthority
	now   func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	owner OwnerAuthority,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || now == nil {
		return nil, fmt.Errorf("organization: PostgreSQL, Vault, and time source are required")
	}
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	if userVault.User() != owner.TenantID {
		return nil, fmt.Errorf("organization: Vault tenant does not match owner authority")
	}
	owner.PublicKey = append(ed25519.PublicKey(nil), owner.PublicKey...)
	return &Store{pool: pool, vault: userVault, owner: owner, now: now}, nil
}

func (store *Store) OrganizationID() contracts.OrganizationID {
	if store == nil {
		return ""
	}
	return store.owner.OrganizationID
}

func (store *Store) PublishCapability(
	ctx context.Context,
	value CapabilityDefinition,
) (bool, error) {
	if value.OrganizationID != store.owner.OrganizationID ||
		value.Signature.KeyID != store.owner.KeyID {
		return false, ErrUnauthorized
	}
	if err := VerifyCapabilityDefinition(value, store.owner.KeyID, store.owner.PublicKey); err != nil {
		return false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	canonical, hash, sealed, err := store.prepareCapability(value)
	if err != nil {
		return false, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("organization: begin capability publication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return false, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, value.Signature.KeyID, value.EffectiveAt,
	); err != nil {
		return false, err
	}
	if err := store.lock(ctx, tx, "capability|"+string(value.ID)); err != nil {
		return false, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_capability_definitions
		WHERE tenant_id=$1 AND organization_id=$2 AND capability_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("organization: inspect capability version: %w", err)
	}
	var latestVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT latest_version FROM workforce_capability_definition_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND capability_id=$3
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID).Scan(&latestVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		if value.Version != 1 {
			return false, ErrConflict
		}
	} else if err != nil {
		return false, fmt.Errorf("organization: inspect capability head: %w", err)
	} else if value.Version != latestVersion+1 {
		return false, ErrConflict
	}
	if value.Version > 1 {
		if value.Previous == nil {
			return false, ErrConflict
		}
		var previousHash string
		var previousEffectiveAt time.Time
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash,effective_at FROM workforce_capability_definitions
			WHERE tenant_id=$1 AND organization_id=$2 AND capability_id=$3 AND version=$4
		`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version-1).Scan(
			&previousHash, &previousEffectiveAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, ErrConflict
			}
			return false, fmt.Errorf("organization: inspect previous capability: %w", err)
		}
		if value.Previous.Digest.Digest != previousHash ||
			!value.EffectiveAt.After(previousEffectiveAt) {
			return false, ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_capability_definitions (
			tenant_id,organization_id,capability_id,version,capability_kind,
			canonical_hash,signature_key_id,sealed_definition,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
		value.Kind, hash, value.Signature.KeyID, sealed, value.EffectiveAt, value.ExpiresAt, now); err != nil {
		return false, fmt.Errorf("organization: insert capability: %w", err)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_capability_definition_heads (
			tenant_id,organization_id,capability_id,latest_version,updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,capability_id) DO UPDATE SET
			latest_version=EXCLUDED.latest_version,updated_at=EXCLUDED.updated_at
		WHERE workforce_capability_definition_heads.latest_version + 1 = EXCLUDED.latest_version
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version, now)
	if err != nil {
		return false, fmt.Errorf("organization: advance capability head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("organization: commit capability: %w", err)
	}
	return false, nil
}

func (store *Store) LoadRegistry(ctx context.Context, at time.Time) (*Registry, error) {
	return store.loadRegistryQuery(ctx, store.pool, at)
}

type registryQuerier interface {
	templateQuerier
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (store *Store) loadRegistryQuery(
	ctx context.Context,
	query registryQuerier,
	at time.Time,
) (*Registry, error) {
	if !validUTC(at) {
		return nil, fmt.Errorf("organization: registry time must be UTC")
	}
	rows, err := query.Query(ctx, `
		SELECT capability_id,version,canonical_hash,sealed_definition
		FROM workforce_capability_definitions
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY capability_id,version
	`, store.owner.TenantID, store.owner.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("organization: query capability registry: %w", err)
	}
	defer rows.Close()
	values := make([]CapabilityDefinition, 0)
	for rows.Next() {
		var id CapabilityID
		var version uint64
		var expectedHash string
		var sealed []byte
		if err := rows.Scan(&id, &version, &expectedHash, &sealed); err != nil {
			return nil, fmt.Errorf("organization: scan capability registry: %w", err)
		}
		canonical, err := store.vault.OpenRecord(store.capabilityAD(id, version), sealed)
		if err != nil || digestBytes(canonical) != expectedHash {
			return nil, ErrIntegrity
		}
		value, err := contracts.DecodeCanonical[CapabilityDefinition, *CapabilityDefinition](canonical)
		if err != nil || value.ID != id || value.Version != version {
			return nil, ErrIntegrity
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("organization: iterate capability registry: %w", err)
	}
	rows.Close()
	if len(values) == 0 {
		return nil, ErrNotFound
	}
	for _, value := range values {
		publicKey, err := store.resolveOwnerPublicKey(
			ctx, query, value.Signature.KeyID, value.EffectiveAt,
		)
		if err != nil || VerifyCapabilityDefinition(
			value, value.Signature.KeyID, publicKey,
		) != nil {
			return nil, ErrIntegrity
		}
	}
	return newVerifiedRegistry(values, at)
}

func (store *Store) PublishTemplate(
	ctx context.Context,
	value OrganizationTemplate,
	requireStartupCoverage bool,
) (bool, error) {
	if value.OrganizationID != store.owner.OrganizationID || value.OwnerID != store.owner.OwnerID {
		return false, ErrUnauthorized
	}
	registry, err := store.LoadRegistry(ctx, value.EffectiveAt)
	if err != nil {
		return false, err
	}
	if err := ValidateTemplateAgainstRegistry(
		value, registry, store.owner.KeyID, store.owner.PublicKey,
		value.EffectiveAt, requireStartupCoverage,
	); err != nil {
		return false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	canonical, hash, sealed, err := store.prepareTemplate(value)
	if err != nil {
		return false, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("organization: begin template publication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.requireCurrentOwnerKey(ctx, tx, now); err != nil {
		return false, err
	}
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, value.Signature.KeyID, value.EffectiveAt,
	); err != nil {
		return false, err
	}
	if err := store.lock(ctx, tx, "template|"+string(value.ID)); err != nil {
		return false, err
	}
	mandateIDs := make([]string, 0, len(value.Departments)*3)
	for _, department := range value.Departments {
		for _, mandate := range department.Mandates {
			mandateIDs = append(mandateIDs, string(mandate.ID))
		}
	}
	slices.Sort(mandateIDs)
	for _, mandateID := range mandateIDs {
		if err := store.lock(ctx, tx, "mandate|"+mandateID); err != nil {
			return false, err
		}
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_organization_templates
		WHERE tenant_id=$1 AND organization_id=$2 AND template_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("organization: inspect template version: %w", err)
	}
	if value.Version > 1 {
		var previous uint64
		if err := tx.QueryRow(ctx, `
			SELECT latest_version FROM workforce_organization_template_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND template_id=$3
		`, store.owner.TenantID, store.owner.OrganizationID, value.ID).Scan(&previous); err != nil ||
			previous+1 != value.Version {
			return false, ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_templates (
			tenant_id,organization_id,template_id,version,owner_id,template_mode,
			department_count,seat_count,capability_registry_digest,receipt_schema_versions,
			canonical_hash,signature_key_id,sealed_template,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
		value.OwnerID, value.Mode, len(value.Departments), len(value.Departments)*3,
		value.CapabilityRegistryDigest.Digest, value.ReceiptSchemaVersions,
		hash, value.Signature.KeyID, sealed, value.EffectiveAt, value.ExpiresAt, now); err != nil {
		return false, fmt.Errorf("organization: insert template: %w", err)
	}
	for _, department := range value.Departments {
		for _, mandate := range department.Mandates {
			if err := store.publishMandateTx(ctx, tx, mandate, now); err != nil {
				return false, err
			}
		}
	}
	for _, department := range value.Departments {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_organization_template_departments (
				tenant_id,organization_id,template_id,template_version,
				department_id,department_key,name,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
			department.ID, department.Key, department.Name, now); err != nil {
			return false, fmt.Errorf("organization: insert template department: %w", err)
		}
		for _, mandate := range department.Mandates {
			mandateDigest, err := SeatMandateDigest(mandate)
			if err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_organization_template_seats (
					tenant_id,organization_id,template_id,template_version,department_id,
					seat_id,seat_role,mandate_id,mandate_version,mandate_digest,
					binding_id,binding_version,independence_domain,created_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
				department.ID, mandate.SeatID, mandate.Role, mandate.ID, mandate.Version,
				mandateDigest.Digest, mandate.ModelBinding.ID, mandate.ModelBinding.Version,
				mandate.IndependenceDomain, now); err != nil {
				return false, fmt.Errorf("organization: insert template seat: %w", err)
			}
		}
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_template_heads (
			tenant_id,organization_id,template_id,latest_version,updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,template_id) DO UPDATE SET
			latest_version=EXCLUDED.latest_version,updated_at=EXCLUDED.updated_at
		WHERE workforce_organization_template_heads.latest_version + 1 = EXCLUDED.latest_version
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version, now)
	if err != nil {
		return false, fmt.Errorf("organization: advance template head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("organization: commit template: %w", err)
	}
	return false, nil
}

func (store *Store) LoadTemplate(
	ctx context.Context,
	id TemplateID,
	version uint64,
) (OrganizationTemplate, error) {
	if id == "" || version == 0 {
		return OrganizationTemplate{}, ErrNotFound
	}
	return store.loadTemplateQuery(ctx, store.pool, id, version)
}

func (store *Store) LoadMandate(
	ctx context.Context,
	id contracts.MandateID,
	version uint64,
) (SeatMandate, error) {
	if id == "" || version == 0 {
		return SeatMandate{}, ErrNotFound
	}
	return store.loadMandateQuery(ctx, store.pool, id, version)
}

func (store *Store) LoadActiveTemplate(ctx context.Context) (OrganizationTemplate, error) {
	var id TemplateID
	var version uint64
	var schemaVersion string
	err := store.pool.QueryRow(ctx, `
		SELECT schema_version,template_id,template_version
		FROM workforce_active_organization_template
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.owner.TenantID, store.owner.OrganizationID).Scan(&schemaVersion, &id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationTemplate{}, ErrNotFound
	}
	if err != nil {
		return OrganizationTemplate{}, fmt.Errorf("organization: load active template head: %w", err)
	}
	if schemaVersion != TemplateSchemaVersion {
		return OrganizationTemplate{}, ErrIntegrity
	}
	return store.LoadTemplate(ctx, id, version)
}

func (store *Store) LoadSeatBinding(
	ctx context.Context,
	templateID TemplateID,
	templateVersion uint64,
	seatID contracts.SeatID,
) (ResolvedSeatBinding, error) {
	template, err := store.LoadTemplate(ctx, templateID, templateVersion)
	if err != nil {
		return ResolvedSeatBinding{}, err
	}
	return resolveSeatBinding(template, seatID)
}

func (store *Store) LoadActiveSeatBinding(
	ctx context.Context,
	seatID contracts.SeatID,
) (ResolvedSeatBinding, error) {
	template, err := store.LoadActiveTemplate(ctx)
	if err != nil {
		return ResolvedSeatBinding{}, err
	}
	return resolveSeatBinding(template, seatID)
}

func resolveSeatBinding(
	template OrganizationTemplate,
	seatID contracts.SeatID,
) (ResolvedSeatBinding, error) {
	if err := validateID("seat_id", string(seatID)); err != nil {
		return ResolvedSeatBinding{}, err
	}
	templateDigest, err := TemplateDigest(template)
	if err != nil {
		return ResolvedSeatBinding{}, err
	}
	copied := copyTemplate(template)
	for _, department := range copied.Departments {
		for _, mandate := range department.Mandates {
			if mandate.SeatID != seatID {
				continue
			}
			mandateDigest, err := SeatMandateDigest(mandate)
			if err != nil {
				return ResolvedSeatBinding{}, err
			}
			value := ResolvedSeatBinding{
				OrganizationID:           copied.OrganizationID,
				TemplateID:               copied.ID,
				TemplateVersion:          copied.Version,
				TemplateDigest:           templateDigest,
				CapabilityRegistryDigest: copied.CapabilityRegistryDigest,
				DepartmentID:             department.ID,
				SeatID:                   seatID,
				Mandate:                  mandate,
				MandateDigest:            mandateDigest,
			}
			if err := value.Validate(); err != nil {
				return ResolvedSeatBinding{}, ErrIntegrity
			}
			return value, nil
		}
	}
	return ResolvedSeatBinding{}, ErrNotFound
}

type templateQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) loadTemplateQuery(
	ctx context.Context,
	query templateQuerier,
	id TemplateID,
	version uint64,
) (OrganizationTemplate, error) {
	var expectedHash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_template
		FROM workforce_organization_templates
		WHERE tenant_id=$1 AND organization_id=$2 AND template_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, id, version).Scan(&expectedHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationTemplate{}, ErrNotFound
	}
	if err != nil {
		return OrganizationTemplate{}, fmt.Errorf("organization: load template: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.templateAD(id, version), sealed)
	if err != nil || digestBytes(canonical) != expectedHash {
		return OrganizationTemplate{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[OrganizationTemplate, *OrganizationTemplate](canonical)
	if err != nil || value.ID != id || value.Version != version {
		return OrganizationTemplate{}, ErrIntegrity
	}
	publicKey, err := store.resolveOwnerPublicKey(
		ctx, query, value.Signature.KeyID, value.EffectiveAt,
	)
	if err != nil || VerifyOrganizationTemplate(
		value, value.Signature.KeyID, publicKey,
	) != nil {
		return OrganizationTemplate{}, ErrIntegrity
	}
	for _, department := range value.Departments {
		for _, mandate := range department.Mandates {
			storedMandate, err := store.loadMandateQuery(ctx, query, mandate.ID, mandate.Version)
			if err != nil {
				return OrganizationTemplate{}, ErrIntegrity
			}
			embeddedDigest, err := SeatMandateDigest(mandate)
			if err != nil {
				return OrganizationTemplate{}, ErrIntegrity
			}
			storedDigest, err := SeatMandateDigest(storedMandate)
			if err != nil || storedDigest != embeddedDigest {
				return OrganizationTemplate{}, ErrIntegrity
			}
			mandatePublicKey, err := store.resolveOwnerPublicKey(
				ctx, query, mandate.Signature.KeyID, mandate.EffectiveAt,
			)
			if err != nil || VerifySeatMandate(
				mandate, mandate.Signature.KeyID, mandatePublicKey,
			) != nil {
				return OrganizationTemplate{}, ErrIntegrity
			}
		}
	}
	return copyTemplate(value), nil
}

func (store *Store) loadMandateQuery(
	ctx context.Context,
	query templateQuerier,
	id contracts.MandateID,
	version uint64,
) (SeatMandate, error) {
	var expectedHash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_mandate
		FROM workforce_organization_seat_mandates
		WHERE tenant_id=$1 AND organization_id=$2 AND mandate_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, id, version).Scan(
		&expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SeatMandate{}, ErrNotFound
	}
	if err != nil {
		return SeatMandate{}, fmt.Errorf("organization: load seat mandate: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.mandateAD(id, version), sealed)
	if err != nil || digestBytes(canonical) != expectedHash {
		return SeatMandate{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[SeatMandate, *SeatMandate](canonical)
	if err != nil || value.ID != id || value.Version != version {
		return SeatMandate{}, ErrIntegrity
	}
	publicKey, err := store.resolveOwnerPublicKey(
		ctx, query, value.Signature.KeyID, value.EffectiveAt,
	)
	if err != nil || VerifySeatMandate(value, value.Signature.KeyID, publicKey) != nil {
		return SeatMandate{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) publishMandateTx(
	ctx context.Context,
	tx pgx.Tx,
	value SeatMandate,
	now time.Time,
) error {
	if err := store.requireConfiguredOwnerKeyAt(
		ctx, tx, value.Signature.KeyID, value.EffectiveAt,
	); err != nil {
		return err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	hash := digestBytes(canonical)
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_organization_seat_mandates
		WHERE tenant_id=$1 AND organization_id=$2 AND mandate_id=$3 AND version=$4
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("organization: inspect seat mandate version: %w", err)
	}
	var latestVersion uint64
	var previousSeatID contracts.SeatID
	var previousRole contracts.SeatRole
	var previousOrigin MandateOrigin
	var previousEffectiveAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT head.latest_version,mandate.seat_id,mandate.seat_role,
		       mandate.mandate_origin,mandate.effective_at
		FROM workforce_organization_seat_mandate_heads head
		JOIN workforce_organization_seat_mandates mandate
		  ON mandate.tenant_id=head.tenant_id AND mandate.organization_id=head.organization_id
		 AND mandate.mandate_id=head.mandate_id AND mandate.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.mandate_id=$3
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID).Scan(
		&latestVersion, &previousSeatID, &previousRole, &previousOrigin, &previousEffectiveAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if value.Origin == MandateOwnerNative && value.Version != 1 {
			return ErrConflict
		}
	} else if err != nil {
		return fmt.Errorf("organization: inspect seat mandate head: %w", err)
	} else if value.Version != latestVersion+1 || value.SeatID != previousSeatID ||
		value.Role != previousRole || value.Origin != previousOrigin ||
		!value.EffectiveAt.After(previousEffectiveAt) {
		return ErrConflict
	}
	sealed, err := store.vault.SealRecord(store.mandateAD(value.ID, value.Version), canonical)
	if err != nil {
		return fmt.Errorf("organization: seal seat mandate: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_seat_mandates (
			tenant_id,organization_id,mandate_id,version,seat_id,department_id,
			seat_role,mandate_origin,canonical_hash,signature_key_id,sealed_mandate,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version,
		value.SeatID, value.DepartmentID, value.Role, value.Origin, hash,
		value.Signature.KeyID, sealed, value.EffectiveAt, value.ExpiresAt, now); err != nil {
		return fmt.Errorf("organization: insert seat mandate: %w", err)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_seat_mandate_heads (
			tenant_id,organization_id,mandate_id,latest_version,updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,mandate_id) DO UPDATE SET
			latest_version=EXCLUDED.latest_version,updated_at=EXCLUDED.updated_at
		WHERE workforce_organization_seat_mandate_heads.latest_version + 1 = EXCLUDED.latest_version
	`, store.owner.TenantID, store.owner.OrganizationID, value.ID, value.Version, now)
	if err != nil {
		return fmt.Errorf("organization: advance seat mandate head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) validateTemplate(
	ctx context.Context,
	value OrganizationTemplate,
	registry *Registry,
	at time.Time,
	requireStartupCoverage bool,
) error {
	return store.validateTemplateQuery(
		ctx, store.pool, value, registry, at, requireStartupCoverage,
	)
}

func (store *Store) validateTemplateQuery(
	ctx context.Context,
	query templateQuerier,
	value OrganizationTemplate,
	registry *Registry,
	at time.Time,
	requireStartupCoverage bool,
) error {
	publicKey, err := store.resolveOwnerPublicKey(
		ctx, query, value.Signature.KeyID, value.EffectiveAt,
	)
	if err != nil {
		return err
	}
	return ValidateTemplateAgainstRegistry(
		value, registry, value.Signature.KeyID, publicKey, at, requireStartupCoverage,
	)
}

func (store *Store) resolveOwnerPublicKey(
	ctx context.Context,
	query templateQuerier,
	keyID string,
	signedAt time.Time,
) (ed25519.PublicKey, error) {
	var publicKey []byte
	var registeredAt time.Time
	var revokedAt *time.Time
	err := query.QueryRow(ctx, `
		SELECT control.public_key,control.registered_at,
		       (
			SELECT revocation.revoked_at
			FROM workforce_owner_control_key_revocations revocation
			WHERE revocation.tenant_id=control.tenant_id
			  AND revocation.organization_id=control.organization_id
			  AND revocation.owner_id=control.owner_id
			  AND revocation.key_id=control.key_id
		       )
		FROM workforce_owner_control_keys control
		WHERE control.tenant_id=$1 AND control.organization_id=$2
		  AND control.owner_id=$3 AND control.key_id=$4
	`, store.owner.TenantID, store.owner.OrganizationID, store.owner.OwnerID, keyID).Scan(
		&publicKey, &registeredAt, &revokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) && keyID == store.owner.KeyID {
		return append(ed25519.PublicKey(nil), store.owner.PublicKey...), nil
	}
	if err != nil {
		return nil, fmt.Errorf("organization: resolve owner verification key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize || registeredAt.After(signedAt) ||
		revokedAt != nil && !signedAt.Before(*revokedAt) {
		return nil, ErrUnauthorized
	}
	return append(ed25519.PublicKey(nil), publicKey...), nil
}

func (store *Store) requireCurrentOwnerKey(
	ctx context.Context,
	query templateQuerier,
	at time.Time,
) error {
	return store.requireConfiguredOwnerKeyAt(ctx, query, store.owner.KeyID, at)
}

func (store *Store) requireConfiguredOwnerKeyAt(
	ctx context.Context,
	query templateQuerier,
	keyID string,
	at time.Time,
) error {
	if keyID != store.owner.KeyID {
		return ErrUnauthorized
	}
	publicKey, err := store.resolveOwnerPublicKey(ctx, query, keyID, at)
	if err != nil || !bytes.Equal(publicKey, store.owner.PublicKey) {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) prepareCapability(
	value CapabilityDefinition,
) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.capabilityAD(value.ID, value.Version), canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("organization: seal capability: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) prepareTemplate(
	value OrganizationTemplate,
) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.templateAD(value.ID, value.Version), canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("organization: seal template: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("organization: time source must return UTC")
	}
	return now, nil
}

func (store *Store) lock(ctx context.Context, tx pgx.Tx, identity string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.owner.TenantID+"|"+string(store.owner.OrganizationID)+"|"+identity)
	if err != nil {
		return fmt.Errorf("organization: acquire transaction lock: %w", err)
	}
	return nil
}

func (store *Store) capabilityAD(id CapabilityID, version uint64) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.capability",
		Stream: string(store.owner.OrganizationID) + "/" + string(id),
		Schema: CapabilitySchemaVersion + ".v" + strconv.FormatUint(version, 10),
	}
}

func (store *Store) templateAD(id TemplateID, version uint64) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.template",
		Stream: string(store.owner.OrganizationID) + "/" + string(id),
		Schema: TemplateSchemaVersion + ".v" + strconv.FormatUint(version, 10),
	}
}

func (store *Store) mandateAD(id contracts.MandateID, version uint64) vault.AD {
	return vault.AD{
		User: store.owner.TenantID, Store: "workforce.organization.seat-mandate",
		Stream: string(store.owner.OrganizationID) + "/" + string(id),
		Schema: SeatMandateSchemaVersion + ".v" + strconv.FormatUint(version, 10),
	}
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
