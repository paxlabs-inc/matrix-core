package policy

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

var (
	ErrUnauthorized = errors.New("policy: owner authority denied")
	ErrStale        = errors.New("policy: stale authority version")
	ErrRevoked      = errors.New("policy: authority revoked")
	ErrLeaseInvalid = errors.New("policy: lease is invalid")
	ErrIntegrity    = errors.New("policy: authority integrity failure")
)

type preparedAuthority struct {
	kind           Kind
	id             string
	version        uint64
	organizationID contracts.OrganizationID
	effectiveAt    time.Time
	keyID          string
	canonical      []byte
	hash           string
	sealed         []byte
}

func (store *Store) PublishOrganization(
	ctx context.Context,
	value contracts.Organization,
	grant OwnerGrant,
) error {
	if value.OwnerID != store.root.OwnerID {
		return ErrUnauthorized
	}
	if err := value.Validate(); err != nil {
		return err
	}
	if err := verifyOrganization(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return err
	}
	return store.publish(ctx, KindOrganization, string(value.ID), value.Version,
		value.ID, value.EffectiveAt, value.Signature.KeyID, &value, grant)
}

func (store *Store) PublishMandate(
	ctx context.Context,
	value contracts.Mandate,
	grant OwnerGrant,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := verifyMandate(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return err
	}
	return store.publish(ctx, KindMandate, string(value.ID), value.Version,
		value.OrganizationID, value.EffectiveAt, value.Signature.KeyID, &value, grant)
}

func (store *Store) PublishSeat(
	ctx context.Context,
	value contracts.Seat,
	grant OwnerGrant,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := verifySeat(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return err
	}
	return store.publish(ctx, KindSeat, string(value.ID), value.Version,
		value.OrganizationID, value.EffectiveAt, value.Signature.KeyID, &value, grant)
}

func (store *Store) PublishPolicy(
	ctx context.Context,
	value contracts.Policy,
	grant OwnerGrant,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := verifyPolicy(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return err
	}
	return store.publish(ctx, KindPolicy, string(value.ID), value.Version,
		value.OrganizationID, value.EffectiveAt, value.Signature.KeyID, &value, grant)
}

func (store *Store) PublishRuntimeAuthority(
	ctx context.Context,
	value RuntimeAuthority,
	grant OwnerGrant,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := verifyRuntimeAuthority(
		store.root.PublicKey, store.root.KeyID, value,
	); err != nil {
		return err
	}
	return store.publish(
		ctx, KindRuntime, value.ID, value.Version,
		value.OrganizationID, value.EffectiveAt,
		value.Signature.KeyID, &value, grant,
	)
}

// PublishSeed atomically installs the first canonical organization topology.
// The caller-supplied records are untrusted: every record and topology binding
// is validated and every signature is verified against the owner root before
// any database mutation begins.
func (store *Store) PublishSeed(
	ctx context.Context,
	seed Seed,
) (SeedPublishResult, error) {
	return store.publishSeed(ctx, seed, nil)
}

// PublishSeedWithCommit installs the seed and invokes commit inside the same
// serializable transaction after all authority and topology projections exist.
func (store *Store) PublishSeedWithCommit(
	ctx context.Context,
	seed Seed,
	commit func(context.Context, pgx.Tx, time.Time) error,
) (SeedPublishResult, error) {
	if commit == nil {
		return SeedPublishResult{}, fmt.Errorf("policy: seed commit hook is required")
	}
	return store.publishSeed(ctx, seed, commit)
}

func (store *Store) publishSeed(
	ctx context.Context,
	seed Seed,
	commit func(context.Context, pgx.Tx, time.Time) error,
) (SeedPublishResult, error) {
	now, err := store.currentTime()
	if err != nil {
		return SeedPublishResult{}, err
	}
	if err := seed.Validate(); err != nil {
		return SeedPublishResult{}, err
	}
	if seed.Organization.ID != store.root.OrganizationID ||
		seed.Organization.OwnerID != store.root.OwnerID ||
		seed.Organization.EffectiveAt.After(now) {
		return SeedPublishResult{}, ErrUnauthorized
	}
	if err := verifyOrganization(
		store.root.PublicKey, store.root.KeyID, seed.Organization,
	); err != nil {
		return SeedPublishResult{}, err
	}
	prepared := make([]preparedAuthority, 0, 45)
	prepare := func(
		kind Kind,
		id string,
		version uint64,
		effectiveAt time.Time,
		keyID string,
		value contracts.Validatable,
	) error {
		if effectiveAt.After(now) || keyID != store.root.KeyID {
			return ErrUnauthorized
		}
		canonical, err := contracts.EncodeCanonical(value)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(canonical)
		hash := hex.EncodeToString(sum[:])
		sealed, err := store.vault.SealRecord(
			store.authorityAD(kind, id, version), canonical,
		)
		if err != nil {
			return fmt.Errorf("policy: seal seed authority: %w", err)
		}
		prepared = append(prepared, preparedAuthority{
			kind: kind, id: id, version: version,
			organizationID: seed.Organization.ID,
			effectiveAt:    effectiveAt, keyID: keyID,
			canonical: canonical, hash: hash, sealed: sealed,
		})
		return nil
	}
	for index := range seed.Mandates {
		mandate := seed.Mandates[index]
		if err := verifyMandate(store.root.PublicKey, store.root.KeyID, mandate); err != nil {
			return SeedPublishResult{}, err
		}
		if err := prepare(
			KindMandate, string(mandate.ID), mandate.Version,
			mandate.EffectiveAt, mandate.Signature.KeyID, &mandate,
		); err != nil {
			return SeedPublishResult{}, err
		}
	}
	if err := verifyRuntimeAuthority(
		store.root.PublicKey, store.root.KeyID, seed.RuntimeAuthority,
	); err != nil {
		return SeedPublishResult{}, err
	}
	if err := prepare(
		KindRuntime, seed.RuntimeAuthority.ID, seed.RuntimeAuthority.Version,
		seed.RuntimeAuthority.EffectiveAt,
		seed.RuntimeAuthority.Signature.KeyID,
		&seed.RuntimeAuthority,
	); err != nil {
		return SeedPublishResult{}, err
	}
	for index := range seed.Policies {
		policyValue := seed.Policies[index]
		if err := verifyPolicy(
			store.root.PublicKey, store.root.KeyID, policyValue,
		); err != nil {
			return SeedPublishResult{}, err
		}
		if err := prepare(
			KindPolicy, string(policyValue.ID), policyValue.Version,
			policyValue.EffectiveAt, policyValue.Signature.KeyID,
			&policyValue,
		); err != nil {
			return SeedPublishResult{}, err
		}
	}
	for _, department := range seed.Organization.Departments {
		for seatIndex := range department.Seats {
			seat := department.Seats[seatIndex]
			if err := verifySeat(store.root.PublicKey, store.root.KeyID, seat); err != nil {
				return SeedPublishResult{}, err
			}
			if err := prepare(
				KindSeat, string(seat.ID), seat.Version,
				seat.EffectiveAt, seat.Signature.KeyID, &seat,
			); err != nil {
				return SeedPublishResult{}, err
			}
		}
	}
	if err := prepare(
		KindOrganization, string(seed.Organization.ID), seed.Organization.Version,
		seed.Organization.EffectiveAt, seed.Organization.Signature.KeyID,
		&seed.Organization,
	); err != nil {
		return SeedPublishResult{}, err
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SeedPublishResult{}, fmt.Errorf("policy: begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		store.root.TenantID+"|"+string(store.root.OrganizationID)+"|seed",
	); err != nil {
		return SeedPublishResult{}, fmt.Errorf("policy: lock seed: %w", err)
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind='organization' AND authority_id=$2 AND version=1
	`, store.root.TenantID, store.root.OrganizationID).Scan(&existingHash)
	if err == nil {
		organizationHash := prepared[len(prepared)-1].hash
		if existingHash != organizationHash {
			return SeedPublishResult{}, ErrStale
		}
		var count int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_authority_records
			WHERE tenant_id=$1 AND organization_id=$2 AND version=1
			  AND authority_kind IN (
			    'organization','mandate','seat','policy','runtime_authority'
			  )
		`, store.root.TenantID, store.root.OrganizationID).Scan(&count); err != nil {
			return SeedPublishResult{}, err
		}
		if count != 45 {
			return SeedPublishResult{}, ErrIntegrity
		}
		if commit != nil {
			if err := commit(ctx, tx, now); err != nil {
				return SeedPublishResult{}, fmt.Errorf(
					"policy: commit existing seed projection: %w", err,
				)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return SeedPublishResult{}, err
		}
		return SeedPublishResult{Deduplicated: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SeedPublishResult{}, fmt.Errorf("policy: inspect seed: %w", err)
	}
	for _, item := range prepared {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,
				material_change,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,FALSE,$11)
		`, store.root.TenantID, item.organizationID, item.kind, item.id,
			item.version, store.root.OwnerID, item.keyID, item.effectiveAt,
			item.hash, item.sealed, now); err != nil {
			return SeedPublishResult{}, fmt.Errorf("policy: insert seed authority: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_authority_heads (
				tenant_id,organization_id,authority_kind,authority_id,
				latest_version,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6)
		`, store.root.TenantID, item.organizationID, item.kind, item.id,
			item.version, now); err != nil {
			return SeedPublishResult{}, fmt.Errorf("policy: insert seed head: %w", err)
		}
	}
	for _, department := range seed.Organization.Departments {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_organization_departments (
				tenant_id,organization_id,department_id,department_kind,
				enabled,created_at
			) VALUES ($1,$2,$3,$4,$5,$6)
		`, store.root.TenantID, seed.Organization.ID, department.ID,
			department.Kind, department.Enabled, now); err != nil {
			return SeedPublishResult{}, fmt.Errorf("policy: project department: %w", err)
		}
		for _, seat := range department.Seats {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_organization_seats (
					tenant_id,organization_id,department_id,seat_id,seat_role,
					mandate_id,mandate_version,active,created_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8)
			`, store.root.TenantID, seed.Organization.ID, department.ID,
				seat.ID, seat.Role, seat.MandateID, seat.MandateVersion,
				now); err != nil {
				return SeedPublishResult{}, fmt.Errorf("policy: project seat: %w", err)
			}
		}
	}
	if commit != nil {
		if err := commit(ctx, tx, now); err != nil {
			return SeedPublishResult{}, fmt.Errorf("policy: commit seed projection: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SeedPublishResult{}, fmt.Errorf("policy: commit seed: %w", err)
	}
	return SeedPublishResult{}, nil
}

func (store *Store) publish(
	ctx context.Context,
	kind Kind,
	id string,
	version uint64,
	organizationID contracts.OrganizationID,
	effectiveAt time.Time,
	keyID string,
	value contracts.Validatable,
	grant OwnerGrant,
) error {
	now, err := store.verifyGrant(grant, "authority:write")
	if err != nil {
		return err
	}
	if organizationID != store.root.OrganizationID || effectiveAt.After(now) {
		return ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	ad := store.authorityAD(kind, id, version)
	sealed, err := store.vault.SealRecord(ad, canonical)
	if err != nil {
		return fmt.Errorf("policy: seal authority: %w", err)
	}
	prepared := preparedAuthority{
		kind: kind, id: id, version: version,
		organizationID: organizationID, effectiveAt: effectiveAt,
		keyID: keyID, canonical: canonical, hash: hash, sealed: sealed,
	}
	return store.publishPrepared(ctx, prepared, now)
}

func (store *Store) publishPrepared(
	ctx context.Context,
	prepared preparedAuthority,
	now time.Time,
) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("policy: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lock := store.root.TenantID + "|" + string(prepared.organizationID) +
		"|" + string(prepared.kind) + "|" + prepared.id
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		lock,
	); err != nil {
		return fmt.Errorf("policy: lock authority chain: %w", err)
	}
	var head uint64
	var previousEffective time.Time
	err = tx.QueryRow(ctx, `
		SELECT record.version, record.effective_at
		FROM workforce_authority_heads head
		JOIN workforce_authority_records record
		  ON record.tenant_id = head.tenant_id
		 AND record.organization_id = head.organization_id
		 AND record.authority_kind = head.authority_kind
		 AND record.authority_id = head.authority_id
		 AND record.version = head.latest_version
		WHERE head.tenant_id = $1 AND head.organization_id = $2
		  AND head.authority_kind = $3 AND head.authority_id = $4
	`, store.root.TenantID, prepared.organizationID, prepared.kind, prepared.id).Scan(
		&head, &previousEffective,
	)
	if !errors.Is(err, pgx.ErrNoRows) && err != nil {
		return fmt.Errorf("policy: inspect authority head: %w", err)
	}
	if (errors.Is(err, pgx.ErrNoRows) && prepared.version != 1) ||
		(err == nil && prepared.version != head+1) ||
		(err == nil && prepared.effectiveAt.Before(previousEffective)) {
		return ErrStale
	}
	material := head != 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_records (
			tenant_id, organization_id, authority_kind, authority_id, version,
			owner_id, key_id, effective_at, canonical_hash, sealed_record,
			material_change, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, store.root.TenantID, prepared.organizationID, prepared.kind, prepared.id,
		prepared.version, store.root.OwnerID, prepared.keyID,
		prepared.effectiveAt, prepared.hash, prepared.sealed, material, now); err != nil {
		return fmt.Errorf("policy: insert authority record: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_heads (
			tenant_id, organization_id, authority_kind, authority_id,
			latest_version, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant_id, organization_id, authority_kind, authority_id)
		DO UPDATE SET latest_version = EXCLUDED.latest_version,
			updated_at = EXCLUDED.updated_at
	`, store.root.TenantID, prepared.organizationID, prepared.kind,
		prepared.id, prepared.version, now); err != nil {
		return fmt.Errorf("policy: advance authority head: %w", err)
	}
	if material {
		if err := store.invalidateLeases(
			ctx, tx, prepared.kind, prepared.id, prepared.version,
			"material authority change", now,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy: commit authority: %w", err)
	}
	return nil
}

func (store *Store) RegisterLease(ctx context.Context, lease contracts.WakeLease) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if err := lease.Validate(); err != nil {
		return err
	}
	if err := store.verifyRuntimeLeaseAuthority(ctx, lease, now); err != nil {
		return fmt.Errorf("%w: %v", ErrLeaseInvalid, err)
	}
	if lease.OrganizationID != store.root.OrganizationID ||
		!lease.ExpiresAt.After(now) {
		return ErrLeaseInvalid
	}
	if err := store.requireCurrent(
		ctx,
		KindMandate,
		string(lease.MandateID),
		lease.MandateVersion,
		now,
	); err != nil {
		return ErrLeaseInvalid
	}
	seat, err := store.LoadCurrentSeat(ctx, lease.SeatID)
	if err != nil ||
		seat.OrganizationID != lease.OrganizationID ||
		seat.DID != lease.SeatDID ||
		seat.MandateID != lease.MandateID ||
		seat.MandateVersion != lease.MandateVersion {
		return ErrLeaseInvalid
	}
	for _, policy := range lease.Policies {
		if err := store.requirePolicyReference(ctx, policy, now); err != nil {
			return ErrLeaseInvalid
		}
	}
	canonical, err := contracts.EncodeCanonical(&lease)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	ad := vault.AD{
		User: store.root.TenantID, Store: "workforce.authority.lease",
		Stream: string(lease.OrganizationID) + "/" + string(lease.ID),
		Schema: lease.SchemaVersion,
	}
	sealed, err := store.vault.SealRecord(ad, canonical)
	if err != nil {
		return fmt.Errorf("policy: seal lease: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policy: begin lease: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_leases (
			tenant_id, organization_id, lease_id, seat_id,
			mandate_id, mandate_version, issued_at, expires_at,
			canonical_hash, sealed_lease
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
	`, store.root.TenantID, lease.OrganizationID, lease.ID, lease.SeatID,
		lease.MandateID, lease.MandateVersion, lease.IssuedAt, lease.ExpiresAt,
		hash, sealed)
	if err != nil {
		return fmt.Errorf("policy: insert lease: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existing string
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_authority_leases
			WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
		`, store.root.TenantID, lease.OrganizationID, lease.ID).Scan(&existing); err != nil {
			return fmt.Errorf("policy: inspect lease identity: %w", err)
		}
		if existing != hash {
			return ErrStale
		}
	}
	for _, policy := range lease.Policies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_authority_lease_policies (
				tenant_id, organization_id, lease_id, policy_id, policy_version
			) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING
		`, store.root.TenantID, lease.OrganizationID, lease.ID,
			policy.ID, policy.Version); err != nil {
			return fmt.Errorf("policy: bind lease policy: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy: commit lease: %w", err)
	}
	return nil
}

// AuthorizeWorkPacket establishes that a caller-supplied packet carries real
// organizational authority. A WorkPacket is transported, so nothing in it is
// trusted on arrival: the lease, seat, and mandate signatures are verified
// against the owner root, and each object must be the exact one the signed
// lease binds. Without this a caller holding one valid lease could attach a
// mandate of its own making and widen what it is allowed to do.
//
// This proves authenticity, not liveness. Callers must still authorize the
// live lease — through the runtime fence, AuthorizeLease, or both — because a
// correctly signed lease may since have expired or been revoked.
func (store *Store) AuthorizeWorkPacket(
	ctx context.Context,
	packet contracts.WorkPacket,
) error {
	if packet.Lease.OrganizationID != store.root.OrganizationID {
		return ErrLeaseInvalid
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if err := store.verifyRuntimeLeaseAuthority(ctx, packet.Lease, now); err != nil {
		return err
	}
	if err := verifySeat(
		store.root.PublicKey, store.root.KeyID, packet.Seat,
	); err != nil {
		return err
	}
	if err := verifyMandate(
		store.root.PublicKey, store.root.KeyID, packet.Mandate,
	); err != nil {
		return err
	}
	if packet.Seat.ID != packet.Lease.SeatID ||
		packet.Seat.DID != packet.Lease.SeatDID ||
		packet.Seat.OrganizationID != packet.Lease.OrganizationID ||
		packet.Seat.MandateID != packet.Lease.MandateID ||
		packet.Seat.MandateVersion != packet.Lease.MandateVersion {
		return ErrLeaseInvalid
	}
	if packet.Mandate.ID != packet.Lease.MandateID ||
		packet.Mandate.Version != packet.Lease.MandateVersion ||
		packet.Mandate.OrganizationID != packet.Lease.OrganizationID ||
		packet.Mandate.SeatRole != packet.Seat.Role {
		return ErrLeaseInvalid
	}
	return nil
}

func (store *Store) AuthorizeLease(
	ctx context.Context,
	leaseID contracts.LeaseID,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	var expiresAt time.Time
	var mandateID string
	var mandateVersion uint64
	err = store.pool.QueryRow(ctx, `
		SELECT lease.expires_at, lease.mandate_id, lease.mandate_version
		FROM workforce_authority_leases lease
		WHERE lease.tenant_id=$1 AND lease.organization_id=$2 AND lease.lease_id=$3
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_lease_invalidations invalidation
			WHERE invalidation.tenant_id=lease.tenant_id
			  AND invalidation.organization_id=lease.organization_id
			  AND invalidation.lease_id=lease.lease_id
		  )
	`, store.root.TenantID, store.root.OrganizationID, leaseID).Scan(
		&expiresAt, &mandateID, &mandateVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseInvalid
	}
	if err != nil {
		return fmt.Errorf("policy: inspect lease: %w", err)
	}
	if !expiresAt.After(now) {
		return ErrLeaseInvalid
	}
	if err := store.requireCurrent(
		ctx, KindMandate, mandateID, mandateVersion, now,
	); err != nil {
		return ErrLeaseInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT policy_id, policy_version
		FROM workforce_authority_lease_policies
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, store.root.TenantID, store.root.OrganizationID, leaseID)
	if err != nil {
		return fmt.Errorf("policy: inspect lease policies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var policyID string
		var version uint64
		if err := rows.Scan(&policyID, &version); err != nil {
			return fmt.Errorf("policy: scan lease policy: %w", err)
		}
		if err := store.requireCurrent(ctx, KindPolicy, policyID, version, now); err != nil {
			return ErrLeaseInvalid
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("policy: iterate lease policies: %w", err)
	}
	return nil
}

func (store *Store) requireCurrent(
	ctx context.Context,
	kind Kind,
	id string,
	version uint64,
	now time.Time,
) error {
	var active uint64
	err := store.pool.QueryRow(ctx, `
		SELECT record.version
		FROM workforce_authority_records record
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.authority_kind=$3 AND record.authority_id=$4
		  AND record.effective_at <= $5
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=record.tenant_id
			  AND revocation.organization_id=record.organization_id
			  AND revocation.authority_kind=record.authority_kind
			  AND revocation.authority_id=record.authority_id
			  AND revocation.version=record.version
		  )
		ORDER BY record.version DESC LIMIT 1
	`, store.root.TenantID, store.root.OrganizationID, kind, id, now).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || active != version {
		return ErrStale
	}
	return err
}

func (store *Store) requirePolicyReference(
	ctx context.Context,
	reference contracts.PolicyRef,
	now time.Time,
) error {
	var active uint64
	var canonicalHash string
	err := store.pool.QueryRow(ctx, `
		SELECT record.version, record.canonical_hash
		FROM workforce_authority_records record
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.authority_kind='policy' AND record.authority_id=$3
		  AND record.effective_at <= $4
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=record.tenant_id
			  AND revocation.organization_id=record.organization_id
			  AND revocation.authority_kind=record.authority_kind
			  AND revocation.authority_id=record.authority_id
			  AND revocation.version=record.version
		  )
		ORDER BY record.version DESC LIMIT 1
	`, store.root.TenantID, store.root.OrganizationID, reference.ID, now).Scan(
		&active, &canonicalHash,
	)
	if err != nil || active != reference.Version ||
		reference.Hash.Algorithm != "sha256" ||
		reference.Hash.Digest != canonicalHash {
		return ErrStale
	}
	return nil
}

func (store *Store) Revoke(
	ctx context.Context,
	revocation Revocation,
	grant OwnerGrant,
) error {
	now, err := store.verifyGrant(grant, "authority:revoke")
	if err != nil {
		return err
	}
	if err := revocation.Validate(); err != nil {
		return err
	}
	if revocation.TenantID != store.root.TenantID ||
		revocation.OrganizationID != store.root.OrganizationID ||
		revocation.OwnerID != store.root.OwnerID ||
		revocation.KeyID != store.root.KeyID ||
		revocation.RevokedAt.After(now) {
		return ErrUnauthorized
	}
	if err := verifyRevocation(
		store.root.PublicKey, store.root.KeyID, revocation,
	); err != nil {
		return err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policy: begin revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_revocations (
			tenant_id, organization_id, authority_kind, authority_id,
			version, reason, owner_id, key_id, signature, revoked_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
	`, revocation.TenantID, revocation.OrganizationID, revocation.Kind,
		revocation.AuthorityID, revocation.Version, revocation.Reason,
		revocation.OwnerID, revocation.KeyID, revocation.Signature.Value,
		revocation.RevokedAt); err != nil {
		return fmt.Errorf("policy: insert revocation: %w", err)
	}
	if err := store.invalidateLeases(
		ctx, tx, revocation.Kind, revocation.AuthorityID,
		revocation.Version, "authority revoked", now,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policy: commit revocation: %w", err)
	}
	return nil
}

func (store *Store) invalidateLeases(
	ctx context.Context,
	tx pgx.Tx,
	kind Kind,
	id string,
	version uint64,
	reason string,
	now time.Time,
) error {
	condition := "FALSE"
	switch kind {
	case KindOrganization:
		condition = "TRUE"
	case KindRuntime:
		condition = "TRUE"
	case KindSeat:
		condition = "lease.seat_id = $4"
	case KindMandate:
		condition = "lease.mandate_id = $4"
	case KindPolicy:
		condition = `EXISTS (
			SELECT 1 FROM workforce_authority_lease_policies binding
			WHERE binding.tenant_id=lease.tenant_id
			  AND binding.organization_id=lease.organization_id
			  AND binding.lease_id=lease.lease_id
			  AND binding.policy_id=$4
		)`
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_authority_lease_invalidations (
			tenant_id, organization_id, lease_id, authority_kind,
			authority_id, authority_version, reason, invalidated_at
		)
		SELECT lease.tenant_id, lease.organization_id, lease.lease_id,
			$3, $4, $5, $6, $7
		FROM workforce_authority_leases lease
		WHERE lease.tenant_id=$1 AND lease.organization_id=$2
		  AND lease.expires_at > $7 AND (`+condition+`)
		ON CONFLICT DO NOTHING
	`, store.root.TenantID, store.root.OrganizationID, kind, id, version, reason, now)
	if err != nil {
		return fmt.Errorf("policy: invalidate affected leases: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_policy_change_receipts (
			tenant_id,organization_id,receipt_id,lease_family,lease_id,
			authority_kind,authority_id,authority_version,reason,created_at
		)
		SELECT invalidation.tenant_id,invalidation.organization_id,
			'policy-change:' || md5(
			  invalidation.lease_id || '|' || invalidation.authority_kind ||
			  '|' || invalidation.authority_id || '|' ||
			  invalidation.authority_version::text
			),
			'authority',invalidation.lease_id,invalidation.authority_kind,
			invalidation.authority_id,invalidation.authority_version,
			invalidation.reason,invalidation.invalidated_at
		FROM workforce_authority_lease_invalidations invalidation
		WHERE invalidation.tenant_id=$1 AND invalidation.organization_id=$2
		  AND invalidation.authority_kind=$3 AND invalidation.authority_id=$4
		  AND invalidation.authority_version=$5
		ON CONFLICT DO NOTHING
	`, store.root.TenantID, store.root.OrganizationID, kind, id, version); err != nil {
		return fmt.Errorf("policy: receipt authority lease invalidation: %w", err)
	}
	runtimeCondition := "FALSE"
	switch kind {
	case KindOrganization:
		runtimeCondition = "TRUE"
	case KindRuntime:
		runtimeCondition = "TRUE"
	case KindSeat:
		runtimeCondition = "runtime.seat_id = $4"
	case KindMandate:
		runtimeCondition = "runtime.mandate_id = $4"
	case KindPolicy:
		runtimeCondition = `EXISTS (
			SELECT 1 FROM workforce_runtime_lease_policies binding
			WHERE binding.tenant_id=runtime.tenant_id
			  AND binding.organization_id=runtime.organization_id
			  AND binding.lease_id=runtime.lease_id
			  AND binding.policy_id=$4
		)`
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_policy_change_receipts (
			tenant_id,organization_id,receipt_id,lease_family,lease_id,
			authority_kind,authority_id,authority_version,reason,created_at
		)
		SELECT runtime.tenant_id,runtime.organization_id,
			'policy-change:' || md5(
			  runtime.lease_id || '|' || $3 || '|' || $4 || '|' ||
			  ($5::bigint)::text
			),
			'runtime',runtime.lease_id,$3,$4,$5::bigint,$6,$7
		FROM workforce_runtime_leases runtime
		WHERE runtime.tenant_id=$1 AND runtime.organization_id=$2
		  AND runtime.state='active' AND runtime.expires_at>$7
		  AND $3::text IS NOT NULL AND $4::text IS NOT NULL AND $5::bigint > 0
		  AND (`+runtimeCondition+`)
		ON CONFLICT DO NOTHING
	`, store.root.TenantID, store.root.OrganizationID, kind, id, version,
		reason, now); err != nil {
		return fmt.Errorf("policy: receipt runtime lease invalidation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_runtime_leases runtime
		SET state='cancelled',cancellation_reason=$6
		WHERE runtime.tenant_id=$1 AND runtime.organization_id=$2
		  AND runtime.state='active' AND runtime.expires_at>$7
		  AND $3::text IS NOT NULL AND $4::text IS NOT NULL AND $5::bigint > 0
		  AND (`+runtimeCondition+`)
	`, store.root.TenantID, store.root.OrganizationID, kind, id, version,
		reason, now); err != nil {
		return fmt.Errorf("policy: suspend runtime leases: %w", err)
	}
	return nil
}

func (store *Store) LoadPolicy(
	ctx context.Context,
	id contracts.PolicyID,
	version uint64,
) (contracts.Policy, error) {
	canonical, err := store.loadAuthority(ctx, KindPolicy, string(id), version)
	if err != nil {
		return contracts.Policy{}, err
	}
	value, err := contracts.DecodeCanonical[contracts.Policy, *contracts.Policy](canonical)
	if err != nil {
		return contracts.Policy{}, ErrIntegrity
	}
	if err := verifyPolicy(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return contracts.Policy{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) LoadCurrentPolicyRefs(
	ctx context.Context,
) ([]contracts.PolicyRef, error) {
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT record.authority_id,record.version,record.canonical_hash
		FROM workforce_authority_heads head
		JOIN workforce_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind='policy' AND record.effective_at<=$3
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=record.tenant_id
			  AND revocation.organization_id=record.organization_id
			  AND revocation.authority_kind=record.authority_kind
			  AND revocation.authority_id=record.authority_id
			  AND revocation.version=record.version
		  )
		ORDER BY record.authority_id
	`, store.root.TenantID, store.root.OrganizationID, now)
	if err != nil {
		return nil, fmt.Errorf("policy: list current policies: %w", err)
	}
	defer rows.Close()
	result := make([]contracts.PolicyRef, 0)
	for rows.Next() {
		reference := contracts.PolicyRef{
			Hash: contracts.ContentHash{Algorithm: "sha256"},
		}
		if err := rows.Scan(
			&reference.ID, &reference.Version, &reference.Hash.Digest,
		); err != nil {
			return nil, fmt.Errorf("policy: scan current policy: %w", err)
		}
		result = append(result, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("policy: iterate current policies: %w", err)
	}
	if len(result) == 0 {
		return nil, ErrUnauthorized
	}
	return result, nil
}

func (store *Store) LoadRuntimeAuthority(
	ctx context.Context,
	id string,
	version uint64,
) (RuntimeAuthority, error) {
	canonical, err := store.loadAuthority(ctx, KindRuntime, id, version)
	if err != nil {
		return RuntimeAuthority{}, err
	}
	value, err := contracts.DecodeCanonical[RuntimeAuthority, *RuntimeAuthority](canonical)
	if err != nil || value.Validate() != nil {
		return RuntimeAuthority{}, ErrIntegrity
	}
	if err := verifyRuntimeAuthority(
		store.root.PublicKey, store.root.KeyID, value,
	); err != nil {
		return RuntimeAuthority{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) LoadCurrentRuntimeAuthority(
	ctx context.Context,
	keyID string,
) (RuntimeAuthority, error) {
	id := RuntimeAuthorityID(keyID)
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT latest_version
		FROM workforce_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4
	`, store.root.TenantID, store.root.OrganizationID,
		KindRuntime, id,
	).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeAuthority{}, ErrRevoked
	}
	if err != nil {
		return RuntimeAuthority{}, fmt.Errorf(
			"policy: load current runtime authority: %w", err,
		)
	}
	return store.LoadRuntimeAuthority(ctx, id, version)
}

func (store *Store) LoadMandate(
	ctx context.Context,
	id contracts.MandateID,
	version uint64,
) (contracts.Mandate, error) {
	canonical, err := store.loadAuthority(ctx, KindMandate, string(id), version)
	if err != nil {
		return contracts.Mandate{}, err
	}
	value, err := contracts.DecodeCanonical[contracts.Mandate, *contracts.Mandate](canonical)
	if err != nil {
		return contracts.Mandate{}, ErrIntegrity
	}
	if err := verifyMandate(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return contracts.Mandate{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) LoadSeat(
	ctx context.Context,
	id contracts.SeatID,
	version uint64,
) (contracts.Seat, error) {
	canonical, err := store.loadAuthority(ctx, KindSeat, string(id), version)
	if err != nil {
		return contracts.Seat{}, err
	}
	value, err := contracts.DecodeCanonical[contracts.Seat, *contracts.Seat](canonical)
	if err != nil {
		return contracts.Seat{}, ErrIntegrity
	}
	if err := verifySeat(store.root.PublicKey, store.root.KeyID, value); err != nil {
		return contracts.Seat{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) LoadCurrentSeat(
	ctx context.Context,
	id contracts.SeatID,
) (contracts.Seat, error) {
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT latest_version
		FROM workforce_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4
	`, store.root.TenantID, store.root.OrganizationID, KindSeat, id).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Seat{}, ErrRevoked
	}
	if err != nil {
		return contracts.Seat{}, fmt.Errorf("policy: load current seat: %w", err)
	}
	return store.LoadSeat(ctx, id, version)
}

func (store *Store) LoadLease(
	ctx context.Context,
	id contracts.LeaseID,
) (contracts.WakeLease, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_lease,canonical_hash
		FROM workforce_authority_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_lease_invalidations invalidation
			WHERE invalidation.tenant_id=$1
			  AND invalidation.organization_id=$2
			  AND invalidation.lease_id=$3
		  )
	`, store.root.TenantID, store.root.OrganizationID, id).Scan(&sealed, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WakeLease{}, ErrLeaseInvalid
	}
	if err != nil {
		return contracts.WakeLease{}, fmt.Errorf("policy: load lease: %w", err)
	}
	canonical, err := store.vault.OpenRecord(vault.AD{
		User: store.root.TenantID, Store: "workforce.authority.lease",
		Stream: string(store.root.OrganizationID) + "/" + string(id),
		Schema: contracts.SchemaVersionV1,
	}, sealed)
	if err != nil {
		return contracts.WakeLease{}, ErrIntegrity
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return contracts.WakeLease{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[contracts.WakeLease, *contracts.WakeLease](canonical)
	if err != nil || value.Validate() != nil {
		return contracts.WakeLease{}, ErrIntegrity
	}
	now, err := store.currentTime()
	if err != nil ||
		store.verifyRuntimeLeaseAuthority(ctx, value, now) != nil {
		return contracts.WakeLease{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) verifyRuntimeLeaseAuthority(
	ctx context.Context,
	lease contracts.WakeLease,
	now time.Time,
) error {
	authority, err := store.LoadCurrentRuntimeAuthority(
		ctx, lease.Signature.KeyID,
	)
	if err != nil {
		return fmt.Errorf("%w: load runtime authority: %v", ErrLeaseInvalid, err)
	}
	if authority.OrganizationID != lease.OrganizationID {
		return fmt.Errorf("%w: runtime authority organization mismatch", ErrLeaseInvalid)
	}
	if authority.EffectiveAt.After(now) {
		return fmt.Errorf("%w: runtime authority is not effective", ErrLeaseInvalid)
	}
	if authority.ExpiresAt != nil && !authority.ExpiresAt.After(now) {
		return fmt.Errorf("%w: runtime authority expired", ErrLeaseInvalid)
	}
	if lease.Signature.KeyID != authority.KeyID {
		return fmt.Errorf("%w: runtime authority key mismatch", ErrLeaseInvalid)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(authority.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ErrIntegrity
	}
	if err := verifyWakeLease(
		ed25519.PublicKey(publicKey), authority.KeyID, lease,
	); err != nil {
		return fmt.Errorf("%w: runtime signature: %v", ErrLeaseInvalid, err)
	}
	return nil
}

func (store *Store) loadAuthority(
	ctx context.Context,
	kind Kind,
	id string,
	version uint64,
) ([]byte, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT record.sealed_record, record.canonical_hash
		FROM workforce_authority_records record
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.authority_kind=$3 AND record.authority_id=$4
		  AND record.version=$5
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revocation
			WHERE revocation.tenant_id=record.tenant_id
			  AND revocation.organization_id=record.organization_id
			  AND revocation.authority_kind=record.authority_kind
			  AND revocation.authority_id=record.authority_id
			  AND revocation.version=record.version
		  )
	`, store.root.TenantID, store.root.OrganizationID, kind, id, version).Scan(
		&sealed, &expectedHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevoked
	}
	if err != nil {
		return nil, fmt.Errorf("policy: load authority: %w", err)
	}
	canonical, err := store.vault.OpenRecord(
		store.authorityAD(kind, id, version),
		sealed,
	)
	if err != nil {
		return nil, ErrIntegrity
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return nil, ErrIntegrity
	}
	return canonical, nil
}

func (store *Store) authorityAD(kind Kind, id string, version uint64) vault.AD {
	return vault.AD{
		User:  store.root.TenantID,
		Store: "workforce.authority." + string(kind),
		Stream: string(store.root.OrganizationID) + "/" + id + "/" +
			strconv.FormatUint(version, 10),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) verifyGrant(
	grant OwnerGrant,
	requiredScope string,
) (time.Time, error) {
	now, err := store.currentTime()
	if err != nil {
		return time.Time{}, err
	}
	if err := grant.Validate(); err != nil {
		return time.Time{}, ErrUnauthorized
	}
	if grant.TenantID != store.root.TenantID ||
		grant.OrganizationID != store.root.OrganizationID ||
		grant.OwnerID != store.root.OwnerID ||
		grant.KeyID != store.root.KeyID ||
		grant.Scope != requiredScope ||
		grant.IssuedAt.After(now) ||
		!grant.ExpiresAt.After(now) {
		return time.Time{}, ErrUnauthorized
	}
	if err := verifyGrant(store.root.PublicKey, store.root.KeyID, grant); err != nil {
		return time.Time{}, ErrUnauthorized
	}
	return now, nil
}
