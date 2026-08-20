package mission

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	// ErrConflict reports a non-identical replay or stale authority version.
	ErrConflict = errors.New("mission: authority conflict")
	// ErrIntegrity reports a sealed-record or canonical-hash mismatch.
	ErrIntegrity = errors.New("mission: authority integrity failure")
)

type authorityKind string

const (
	kindMission      authorityKind = "founder_mission"
	kindConstitution authorityKind = "company_constitution"
	kindCapital      authorityKind = "capital_envelope"
	kindIssuer       authorityKind = "company_issuer_policy"
	kindOrganization authorityKind = "organization_v2"
)

type preparedRecord struct {
	kind        authorityKind
	id          string
	version     uint64
	effectiveAt time.Time
	canonical   []byte
	hash        string
	sealed      []byte
}

// PreparedActivation is an immutable, verified, Vault-sealed authority set
// that can be committed in the legacy organization's serializable transaction.
type PreparedActivation struct {
	organizationID contracts.OrganizationID
	ownerID        contracts.OwnerID
	keyID          string
	records        []preparedRecord
}

// Store owns founder company authority persistence and current-version reads.
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

// CurrentAuthority is the authenticated current company root plus its
// executable pause and issuer-revocation projection.
type CurrentAuthority struct {
	Authority       ActivationAuthority
	State           string
	IssuerRevokedAt *time.Time
}

// Executable reports whether the current company root permits new controller work.
func (value CurrentAuthority) Executable(now time.Time) bool {
	return now.Location() == time.UTC && value.State == "active" &&
		value.IssuerRevokedAt == nil && !value.Authority.Mission.EffectiveAt.After(now) &&
		!value.Authority.Constitution.EffectiveAt.After(now) &&
		!value.Authority.Capital.EffectiveAt.After(now) &&
		!value.Authority.IssuerPolicy.EffectiveAt.After(now) &&
		value.Authority.IssuerPolicy.ExpiresAt.After(now) &&
		!value.Authority.Organization.EffectiveAt.After(now)
}

// LoadCurrent authenticates every Vault-sealed current authority record and
// verifies the complete founder signature set before returning company state.
func (store *Store) LoadCurrent(ctx context.Context) (CurrentAuthority, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT head.authority_kind,head.authority_id,head.latest_version,
		       record.canonical_hash,record.sealed_record
		FROM workforce_company_authority_heads head
		JOIN workforce_company_authority_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.authority_kind
	`, store.tenantID, store.organizationID)
	if err != nil {
		return CurrentAuthority{}, fmt.Errorf("mission: query current authority: %w", err)
	}
	defer rows.Close()
	var authority ActivationAuthority
	seen := 0
	for rows.Next() {
		var kind authorityKind
		var id string
		var version uint64
		var expectedHash string
		var sealed []byte
		if err := rows.Scan(&kind, &id, &version, &expectedHash, &sealed); err != nil {
			return CurrentAuthority{}, fmt.Errorf("mission: scan current authority: %w", err)
		}
		canonical, err := store.vault.OpenRecord(store.authorityAD(kind, id, version), sealed)
		if err != nil {
			return CurrentAuthority{}, ErrIntegrity
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != expectedHash {
			return CurrentAuthority{}, ErrIntegrity
		}
		switch kind {
		case kindMission:
			authority.Mission, err = contracts.DecodeCanonical[FounderMission, *FounderMission](canonical)
		case kindConstitution:
			authority.Constitution, err = contracts.DecodeCanonical[CompanyConstitution, *CompanyConstitution](canonical)
		case kindCapital:
			authority.Capital, err = contracts.DecodeCanonical[CapitalEnvelope, *CapitalEnvelope](canonical)
		case kindIssuer:
			authority.IssuerPolicy, err = contracts.DecodeCanonical[CompanyIssuerPolicy, *CompanyIssuerPolicy](canonical)
		case kindOrganization:
			authority.Organization, err = contracts.DecodeCanonical[OrganizationV2, *OrganizationV2](canonical)
		default:
			return CurrentAuthority{}, ErrIntegrity
		}
		if err != nil {
			return CurrentAuthority{}, ErrIntegrity
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return CurrentAuthority{}, fmt.Errorf("mission: iterate current authority: %w", err)
	}
	if seen != 5 {
		return CurrentAuthority{}, ErrIntegrity
	}
	if err := VerifyActivationAuthority(authority, store.keyID, store.publicKey); err != nil {
		return CurrentAuthority{}, ErrIntegrity
	}
	var current CurrentAuthority
	current.Authority = authority
	var revokedAt pgtype.Timestamptz
	if err := store.pool.QueryRow(ctx, `
		SELECT state,issuer_revoked_at FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, store.organizationID).Scan(&current.State, &revokedAt); err != nil {
		return CurrentAuthority{}, fmt.Errorf("mission: load organization-v2 projection: %w", err)
	}
	if revokedAt.Valid {
		value := revokedAt.Time
		current.IssuerRevokedAt = &value
	}
	return current, nil
}

// AuthorityChangeImpact is the current durable mutation radius of a material
// founder authority change. Counts are read from one database snapshot.
type AuthorityChangeImpact struct {
	ActiveAuthorityLeases int64
	ActiveRuntimeLeases   int64
	QueuedWakes           int64
	DispatchedWakes       int64
	UnsettledEffects      int64
}

type authorityImpactQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// AnalyzeChange reports the exact current lease, wake, and effect radius that
// will be paused or invalidated by CommitVersionTx.
func (store *Store) AnalyzeChange(ctx context.Context, now time.Time) (AuthorityChangeImpact, error) {
	if now.IsZero() || now.Location() != time.UTC {
		return AuthorityChangeImpact{}, fmt.Errorf("mission: authority impact time must be UTC")
	}
	return store.analyzeChange(ctx, store.pool, now)
}

func (store *Store) analyzeChange(
	ctx context.Context,
	querier authorityImpactQuerier,
	now time.Time,
) (AuthorityChangeImpact, error) {
	var impact AuthorityChangeImpact
	err := querier.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_authority_leases lease
		   WHERE lease.tenant_id=$1 AND lease.organization_id=$2 AND lease.expires_at>$3
		     AND NOT EXISTS (
		       SELECT 1 FROM workforce_authority_lease_invalidations invalidation
		       WHERE invalidation.tenant_id=lease.tenant_id
		         AND invalidation.organization_id=lease.organization_id
		         AND invalidation.lease_id=lease.lease_id)),
		  (SELECT COUNT(*) FROM workforce_runtime_leases
		   WHERE tenant_id=$1 AND organization_id=$2 AND state='active' AND expires_at>$3),
		  (SELECT COUNT(*) FROM workforce_scheduled_wakes
		   WHERE tenant_id=$1 AND organization_id=$2 AND state='queued'),
		  (SELECT COUNT(*) FROM workforce_scheduled_wakes
		   WHERE tenant_id=$1 AND organization_id=$2 AND state='dispatched'),
		  (SELECT COUNT(*) FROM workforce_effect_operations
		   WHERE tenant_id=$1 AND organization_id=$2
		     AND state IN ('prepared','dispatching','externally_ambiguous'))
	`, store.tenantID, store.organizationID, now).Scan(
		&impact.ActiveAuthorityLeases, &impact.ActiveRuntimeLeases,
		&impact.QueuedWakes, &impact.DispatchedWakes, &impact.UnsettledEffects,
	)
	if err != nil {
		return AuthorityChangeImpact{}, fmt.Errorf("mission: analyze authority change: %w", err)
	}
	return impact, nil
}

// NewStore constructs a tenant- and organization-bound company authority store.
func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	ownerID contracts.OwnerID,
	keyID string,
	publicKey ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" ||
		ownerID == "" || keyID == "" || len(publicKey) != ed25519.PublicKeySize || now == nil {
		return nil, fmt.Errorf("mission: store authority and dependencies are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("mission: Vault tenant does not match authority tenant")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, ownerID: ownerID, keyID: keyID,
		publicKey: append(ed25519.PublicKey(nil), publicKey...), now: now,
	}, nil
}

// PrepareActivation verifies founder signatures and seals all authority before
// the activation transaction starts.
func (store *Store) PrepareActivation(value ActivationAuthority) (PreparedActivation, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return PreparedActivation{}, fmt.Errorf("mission: time source must return UTC")
	}
	if value.Mission.OrganizationID != store.organizationID ||
		value.Mission.OwnerID != store.ownerID || value.Mission.EffectiveAt.After(now) {
		return PreparedActivation{}, fmt.Errorf("mission: activation authority is outside the owner root")
	}
	if err := VerifyActivationAuthority(value, store.keyID, store.publicKey); err != nil {
		return PreparedActivation{}, err
	}
	prepared := PreparedActivation{
		organizationID: store.organizationID, ownerID: store.ownerID,
		keyID: store.keyID, records: make([]preparedRecord, 0, 5),
	}
	values := []struct {
		kind        authorityKind
		id          string
		version     uint64
		effectiveAt time.Time
		value       contracts.Validatable
	}{
		{kindMission, value.Mission.ID, value.Mission.Version, value.Mission.EffectiveAt, &value.Mission},
		{kindConstitution, value.Constitution.ID, value.Constitution.Version, value.Constitution.EffectiveAt, &value.Constitution},
		{kindCapital, value.Capital.ID, value.Capital.Version, value.Capital.EffectiveAt, &value.Capital},
		{kindIssuer, value.IssuerPolicy.ID, value.IssuerPolicy.Version, value.IssuerPolicy.EffectiveAt, &value.IssuerPolicy},
		{kindOrganization, value.Organization.ID, value.Organization.Version, value.Organization.EffectiveAt, &value.Organization},
	}
	for index := range values {
		canonical, err := contracts.EncodeCanonical(values[index].value)
		if err != nil {
			return PreparedActivation{}, err
		}
		sum := sha256.Sum256(canonical)
		sealed, err := store.vault.SealRecord(
			store.authorityAD(values[index].kind, values[index].id, values[index].version),
			canonical,
		)
		if err != nil {
			return PreparedActivation{}, fmt.Errorf("mission: seal %s: %w", values[index].kind, err)
		}
		prepared.records = append(prepared.records, preparedRecord{
			kind: values[index].kind, id: values[index].id,
			version: values[index].version, effectiveAt: values[index].effectiveAt,
			canonical: canonical, hash: hex.EncodeToString(sum[:]), sealed: sealed,
		})
	}
	return prepared, nil
}

// CommitActivationTx installs the prepared authority and organization-v2
// compatibility projection in the caller's serializable transaction.
func (store *Store) CommitActivationTx(
	ctx context.Context,
	tx pgx.Tx,
	prepared PreparedActivation,
	legacyOrganizationVersion uint64,
	now time.Time,
) (bool, error) {
	if tx == nil || prepared.organizationID != store.organizationID ||
		prepared.ownerID != store.ownerID || prepared.keyID != store.keyID ||
		len(prepared.records) != 5 || legacyOrganizationVersion == 0 ||
		now.IsZero() || now.Location() != time.UTC {
		return false, fmt.Errorf("mission: prepared activation is invalid")
	}
	for index := range prepared.records {
		if prepared.records[index].version != 1 {
			return false, ErrConflict
		}
	}
	var existing int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_company_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND version=1
	`, store.tenantID, store.organizationID).Scan(&existing); err != nil {
		return false, fmt.Errorf("mission: inspect activation: %w", err)
	}
	if existing != 0 {
		if existing != len(prepared.records) {
			return false, ErrIntegrity
		}
		for index := range prepared.records {
			var hash string
			err := tx.QueryRow(ctx, `
				SELECT canonical_hash FROM workforce_company_authority_records
				WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind=$3
				  AND authority_id=$4 AND version=$5
			`, store.tenantID, store.organizationID, prepared.records[index].kind,
				prepared.records[index].id, prepared.records[index].version).Scan(&hash)
			if err != nil || hash != prepared.records[index].hash {
				return false, ErrConflict
			}
		}
		return true, nil
	}
	for index := range prepared.records {
		record := prepared.records[index]
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, store.tenantID, store.organizationID, record.kind, record.id,
			record.version, store.ownerID, store.keyID, record.effectiveAt,
			record.hash, record.sealed, now); err != nil {
			return false, fmt.Errorf("mission: insert %s: %w", record.kind, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_authority_heads (
				tenant_id,organization_id,authority_kind,authority_id,latest_version,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6)
		`, store.tenantID, store.organizationID, record.kind, record.id,
			record.version, now); err != nil {
			return false, fmt.Errorf("mission: insert %s head: %w", record.kind, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_organization_v2_projection (
			tenant_id,organization_id,schema_version,template_id,template_version,
			legacy_organization_version,organization_v2_version,mission_version,constitution_version,
			capital_envelope_version,issuer_policy_version,state,paused_at,
			issuer_revoked_at,activated_at
		) VALUES ($1,$2,'workforce.organization.v2','organization-template:default-v1',1,$3,1,1,1,1,1,'active',NULL,NULL,$4)
	`, store.tenantID, store.organizationID, legacyOrganizationVersion, now); err != nil {
		return false, fmt.Errorf("mission: project organization v2: %w", err)
	}
	return false, nil
}

// CommitVersionTx installs one founder-signed material authority version,
// invalidates every affected lease, pauses new initiation, and emits immutable
// change receipts in the caller's serializable transaction.
func (store *Store) CommitVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	prepared PreparedActivation,
	expectedVersion uint64,
	now time.Time,
) error {
	if tx == nil || expectedVersion == 0 ||
		prepared.organizationID != store.organizationID ||
		prepared.ownerID != store.ownerID || prepared.keyID != store.keyID ||
		len(prepared.records) != 5 || now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("mission: prepared authority change is invalid")
	}
	for index := range prepared.records {
		record := prepared.records[index]
		if record.version != expectedVersion+1 {
			return ErrConflict
		}
		var head uint64
		err := tx.QueryRow(ctx, `
			SELECT latest_version FROM workforce_company_authority_heads
			WHERE tenant_id=$1 AND organization_id=$2
			  AND authority_kind=$3 AND authority_id=$4
			FOR UPDATE
		`, store.tenantID, store.organizationID, record.kind, record.id).Scan(&head)
		if err != nil || head != expectedVersion {
			return ErrConflict
		}
	}
	impact, err := store.analyzeChange(ctx, tx, now)
	if err != nil {
		return err
	}
	for index := range prepared.records {
		record := prepared.records[index]
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, store.tenantID, store.organizationID, record.kind, record.id,
			record.version, store.ownerID, store.keyID, record.effectiveAt,
			record.hash, record.sealed, now); err != nil {
			return fmt.Errorf("mission: insert changed %s: %w", record.kind, err)
		}
		result, err := tx.Exec(ctx, `
			UPDATE workforce_company_authority_heads
			SET latest_version=$5,updated_at=$6
			WHERE tenant_id=$1 AND organization_id=$2
			  AND authority_kind=$3 AND authority_id=$4
			  AND latest_version=$7
		`, store.tenantID, store.organizationID, record.kind, record.id,
			record.version, now, expectedVersion)
		if err != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_authority_lease_invalidations (
				tenant_id,organization_id,lease_id,authority_kind,authority_id,
				authority_version,reason,invalidated_at
			) SELECT lease.tenant_id,lease.organization_id,lease.lease_id,$3,$4,$5,
			  'material founder company-authority change',$6
			  FROM workforce_authority_leases lease
			  WHERE lease.tenant_id=$1 AND lease.organization_id=$2 AND lease.expires_at>$6
			    AND NOT EXISTS (
			      SELECT 1 FROM workforce_authority_lease_invalidations invalidation
			      WHERE invalidation.tenant_id=lease.tenant_id
			        AND invalidation.organization_id=lease.organization_id
			        AND invalidation.lease_id=lease.lease_id)
			  ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, record.kind, record.id,
			record.version, now); err != nil {
			return fmt.Errorf("mission: invalidate leases for %s: %w", record.kind, err)
		}
		totalAffected := impact.ActiveAuthorityLeases + impact.ActiveRuntimeLeases
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_authority_change_receipts (
				tenant_id,organization_id,receipt_id,authority_kind,authority_id,
				authority_version,affected_lease_count,
				affected_authority_lease_count,affected_runtime_lease_count,
				affected_queued_wake_count,affected_dispatched_wake_count,
				affected_effect_count,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		`, store.tenantID, store.organizationID,
			fmt.Sprintf("company-authority-change:%s:%d", record.kind, record.version),
			record.kind, record.id, record.version, totalAffected,
			impact.ActiveAuthorityLeases, impact.ActiveRuntimeLeases,
			impact.QueuedWakes, impact.DispatchedWakes, impact.UnsettledEffects, now); err != nil {
			return fmt.Errorf("mission: receipt %s change: %w", record.kind, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET state='cancelled',cancellation_reason='material founder company-authority change'
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active' AND expires_at>$3
	`, store.tenantID, store.organizationID, now); err != nil {
		return fmt.Errorf("mission: cancel runtime leases: %w", err)
	}
	result, err := tx.Exec(ctx, `
		UPDATE workforce_organization_v2_projection
		SET organization_v2_version=$3,mission_version=$3,constitution_version=$3,
		    capital_envelope_version=$3,issuer_policy_version=$3,
		    state='paused',paused_at=$4
		WHERE tenant_id=$1 AND organization_id=$2
		  AND mission_version=$5 AND constitution_version=$5
		  AND capital_envelope_version=$5 AND issuer_policy_version=$5
	`, store.tenantID, store.organizationID, expectedVersion+1, now, expectedVersion)
	if err != nil || result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) authorityAD(kind authorityKind, id string, version uint64) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.company-authority." + string(kind),
		Stream: string(store.organizationID) + "/" + id,
		Schema: fmt.Sprintf("%s.v%d", kind, version),
	}
}
