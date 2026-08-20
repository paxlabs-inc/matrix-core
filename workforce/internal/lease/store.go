package lease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
)

// Store owns linearizable lease transactions for one tenant.
type Store struct {
	pool     *pgxpool.Pool
	tenantID string
	now      func() time.Time
}

// New constructs a fail-closed tenant lease service.
func New(pool *pgxpool.Pool, tenantID string, now func() time.Time) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || tenantID == "" || now == nil {
		return nil, fmt.Errorf("lease: pool, tenant_id, and time source are required")
	}
	return &Store{pool: pool, tenantID: tenantID, now: now}, nil
}

// Acquire atomically expires prior authority, verifies current signed authority,
// locks both scopes, and mints a monotonically increasing fence.
func (store *Store) Acquire(ctx context.Context, request Request) (Grant, error) {
	if err := request.Validate(); err != nil {
		return Grant{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return Grant{}, err
	}
	if request.IssuedAt.After(now) || !request.ExpiresAt.After(now) {
		return Grant{}, ErrExpired
	}
	for attempt := 0; ; attempt++ {
		grant, acquireErr := store.acquireOnce(ctx, request, now)
		if !errors.Is(acquireErr, ErrUncertain) || attempt == 3 {
			return grant, acquireErr
		}
	}
}

// Recover returns an already-minted active grant only when every scheduler and
// graph identity supplied by a reclaimed wake matches. It never creates,
// renews, or widens authority.
func (store *Store) Recover(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	wakeID contracts.WakeID,
	seatID contracts.SeatID,
	nodeID dependency.NodeID,
) (Grant, error) {
	for name, value := range map[string]string{
		"organization_id": string(organizationID),
		"lease_id":        string(leaseID),
		"wake_id":         string(wakeID),
		"seat_id":         string(seatID),
		"node_id":         string(nodeID),
	} {
		if err := validateToken(name, value); err != nil {
			return Grant{}, err
		}
	}
	grant, err := store.load(ctx, organizationID, leaseID)
	if err != nil {
		return Grant{}, err
	}
	if grant.WakeID != wakeID || grant.SeatID != seatID ||
		grant.NodeID != nodeID {
		return Grant{}, ErrStaleFence
	}
	return store.Authorize(ctx, organizationID, leaseID, grant.Fence)
}

func (store *Store) acquireOnce(
	ctx context.Context,
	request Request,
	now time.Time,
) (Grant, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Grant{}, fmt.Errorf("%w: begin acquisition: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, scope := range []string{
		store.tenantID + "|" + string(request.OrganizationID) + "|seat|" + string(request.SeatID),
		store.tenantID + "|" + string(request.OrganizationID) + "|node|" + string(request.NodeID),
	} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope); err != nil {
			return Grant{}, fmt.Errorf("%w: lock acquisition: %v", ErrUncertain, err)
		}
	}
	if err := expireActive(ctx, tx, store.tenantID, request.OrganizationID, now); err != nil {
		return Grant{}, err
	}
	if err := requireAuthority(ctx, tx, store.tenantID, request, now); err != nil {
		return Grant{}, err
	}
	var held int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		  AND (seat_id=$3 OR node_id=$4)
	`, store.tenantID, request.OrganizationID, request.SeatID, request.NodeID).Scan(&held); err != nil {
		return Grant{}, fmt.Errorf("%w: inspect holders: %v", ErrUncertain, err)
	}
	if held != 0 {
		return Grant{}, ErrHeld
	}
	fence, err := nextFence(ctx, tx, store.tenantID, request.OrganizationID,
		"organization", string(request.OrganizationID), now)
	if err != nil {
		return Grant{}, err
	}
	bindingHash := policyBindingHash(request.Policies)
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_runtime_leases (
			tenant_id,organization_id,lease_id,wake_id,seat_id,node_id,
			mandate_id,mandate_version,policy_binding_hash,fence,state,
			issued_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',$11,$12)
	`, store.tenantID, request.OrganizationID, request.ID, request.WakeID,
		request.SeatID, request.NodeID, request.MandateID, request.MandateVersion,
		bindingHash, fence, request.IssuedAt, request.ExpiresAt)
	if err != nil {
		return Grant{}, fmt.Errorf("%w: insert acquisition: %v", ErrUncertain, err)
	}
	for _, policy := range request.Policies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_runtime_lease_policies (
				tenant_id,organization_id,lease_id,policy_id,policy_version,policy_hash
			) VALUES ($1,$2,$3,$4,$5,$6)
		`, store.tenantID, request.OrganizationID, request.ID,
			policy.ID, policy.Version, policy.Hash.Digest); err != nil {
			return Grant{}, fmt.Errorf("%w: bind policy: %v", ErrUncertain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, fmt.Errorf("%w: commit acquisition: %v", ErrUncertain, err)
	}
	return Grant{Request: request, Fence: contracts.FenceToken(fence), State: StateActive}, nil
}

// Renew extends one active generation without changing its fence.
func (store *Store) Renew(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	fence contracts.FenceToken,
	expiresAt time.Time,
) (Grant, error) {
	now, err := store.currentTime()
	if err != nil {
		return Grant{}, err
	}
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return Grant{}, err
	}
	if err := validateToken("lease_id", string(leaseID)); err != nil {
		return Grant{}, err
	}
	if err := fence.Validate(); err != nil {
		return Grant{}, err
	}
	if !validUTC(expiresAt) || !expiresAt.After(now) || expiresAt.Sub(now) > 2*time.Hour {
		return Grant{}, ErrExpired
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET expires_at=$1,renewed_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND lease_id=$5
		  AND fence=$6 AND state='active' AND expires_at>$2
	`, expiresAt, now, store.tenantID, organizationID, leaseID, fence)
	if err != nil {
		return Grant{}, fmt.Errorf("%w: renew: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return Grant{}, store.classify(ctx, organizationID, leaseID, fence, now)
	}
	return store.load(ctx, organizationID, leaseID)
}

// Cancel revokes one current lease and never reuses its fence.
func (store *Store) Cancel(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	fence contracts.FenceToken,
	reason string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 512 {
		return fmt.Errorf("cancellation reason must contain 1 to 512 bytes")
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET state='cancelled',cancellation_reason=$1
		WHERE tenant_id=$3 AND organization_id=$4 AND lease_id=$5
		  AND fence=$6 AND state='active' AND expires_at>$2
	`, reason, now, store.tenantID, organizationID, leaseID, fence)
	if err != nil {
		return fmt.Errorf("%w: cancel: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return store.classify(ctx, organizationID, leaseID, fence, now)
	}
	return nil
}

// Authorize proves the exact lease and fence are active at this instant and that
// every signed mandate and policy binding remains current.
func (store *Store) Authorize(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	fence contracts.FenceToken,
) (Grant, error) {
	now, err := store.currentTime()
	if err != nil {
		return Grant{}, err
	}
	grant, err := store.load(ctx, organizationID, leaseID)
	if err != nil {
		return Grant{}, err
	}
	if grant.Fence != fence {
		_ = store.incident(ctx, organizationID, leaseID, "stale_fence", "The caller presented an obsolete fencing token.", now)
		return Grant{}, ErrStaleFence
	}
	if grant.State == StateCancelled {
		return Grant{}, ErrCancelled
	}
	if grant.State != StateActive || !grant.ExpiresAt.After(now) {
		return Grant{}, ErrExpired
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Grant{}, fmt.Errorf("%w: begin authorization: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := requireAuthority(ctx, tx, store.tenantID, grant.Request, now); err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, fmt.Errorf("%w: commit authorization: %v", ErrUncertain, err)
	}
	return grant, nil
}

func (store *Store) load(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
) (Grant, error) {
	grant := Grant{Request: Request{OrganizationID: organizationID, ID: leaseID}}
	var fence uint64
	err := store.pool.QueryRow(ctx, `
		SELECT wake_id,seat_id,node_id,mandate_id,mandate_version,fence,state,
			issued_at,expires_at,renewed_at
		FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, store.tenantID, organizationID, leaseID).Scan(
		&grant.WakeID, &grant.SeatID, &grant.NodeID, &grant.MandateID,
		&grant.MandateVersion, &fence, &grant.State, &grant.IssuedAt,
		&grant.ExpiresAt, &grant.RenewedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrStaleFence
	}
	if err != nil {
		return Grant{}, fmt.Errorf("%w: load lease: %v", ErrUncertain, err)
	}
	grant.IssuedAt = grant.IssuedAt.UTC()
	grant.ExpiresAt = grant.ExpiresAt.UTC()
	if grant.RenewedAt != nil {
		normalized := grant.RenewedAt.UTC()
		grant.RenewedAt = &normalized
	}
	grant.Fence = contracts.FenceToken(fence)
	rows, err := store.pool.Query(ctx, `
		SELECT policy_id,policy_version,policy_hash
		FROM workforce_runtime_lease_policies
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
		ORDER BY policy_id
	`, store.tenantID, organizationID, leaseID)
	if err != nil {
		return Grant{}, fmt.Errorf("%w: load policies: %v", ErrUncertain, err)
	}
	defer rows.Close()
	for rows.Next() {
		var policy contracts.PolicyRef
		policy.Hash.Algorithm = "sha256"
		if err := rows.Scan(&policy.ID, &policy.Version, &policy.Hash.Digest); err != nil {
			return Grant{}, fmt.Errorf("%w: scan policy: %v", ErrUncertain, err)
		}
		grant.Policies = append(grant.Policies, policy)
	}
	if err := rows.Err(); err != nil {
		return Grant{}, fmt.Errorf("%w: iterate policies: %v", ErrUncertain, err)
	}
	return grant, nil
}

func requireAuthority(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	request Request,
	now time.Time,
) error {
	if err := requireCurrent(ctx, tx, tenantID, request.OrganizationID,
		"mandate", string(request.MandateID), request.MandateVersion, "", now); err != nil {
		return err
	}
	for _, policy := range request.Policies {
		if err := requireCurrent(ctx, tx, tenantID, request.OrganizationID,
			"policy", string(policy.ID), policy.Version, policy.Hash.Digest, now); err != nil {
			return err
		}
	}
	return nil
}

func requireCurrent(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	organizationID contracts.OrganizationID,
	kind, id string,
	version uint64,
	expectedHash string,
	now time.Time,
) error {
	var currentVersion uint64
	var currentHash string
	err := tx.QueryRow(ctx, `
		SELECT record.version,record.canonical_hash
		FROM workforce_authority_records record
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.authority_kind=$3 AND record.authority_id=$4
		  AND record.effective_at<=$5
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_authority_revocations revoked
			WHERE revoked.tenant_id=record.tenant_id
			  AND revoked.organization_id=record.organization_id
			  AND revoked.authority_kind=record.authority_kind
			  AND revoked.authority_id=record.authority_id
			  AND revoked.version=record.version
		  )
		ORDER BY record.version DESC LIMIT 1
	`, tenantID, organizationID, kind, id, now).Scan(&currentVersion, &currentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPolicyMismatch
	}
	if err != nil {
		return fmt.Errorf("%w: inspect %s authority: %v", ErrUncertain, kind, err)
	}
	if currentVersion != version || expectedHash != "" && expectedHash != currentHash {
		return ErrPolicyMismatch
	}
	return nil
}

func expireActive(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	organizationID contracts.OrganizationID,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET state='expired'
		WHERE tenant_id=$1 AND organization_id=$2
		  AND state='active' AND expires_at<=$3
	`, tenantID, organizationID, now); err != nil {
		return fmt.Errorf("%w: expire prior leases: %v", ErrUncertain, err)
	}
	return nil
}

func nextFence(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	organizationID contracts.OrganizationID,
	scopeKind, scopeID string,
	now time.Time,
) (uint64, error) {
	var fence uint64
	err := tx.QueryRow(ctx, `
		INSERT INTO workforce_fence_counters (
			tenant_id,organization_id,scope_kind,scope_id,last_fence,updated_at
		) VALUES ($1,$2,$3,$4,1,$5)
		ON CONFLICT (tenant_id,organization_id,scope_kind,scope_id)
		DO UPDATE SET last_fence=workforce_fence_counters.last_fence+1,updated_at=$5
		RETURNING last_fence
	`, tenantID, organizationID, scopeKind, scopeID, now).Scan(&fence)
	if err != nil {
		return 0, fmt.Errorf("%w: mint fence: %v", ErrUncertain, err)
	}
	return fence, nil
}

func (store *Store) classify(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	fence contracts.FenceToken,
	now time.Time,
) error {
	grant, err := store.load(ctx, organizationID, leaseID)
	if err != nil {
		return err
	}
	switch {
	case grant.Fence != fence:
		return ErrStaleFence
	case grant.State == StateCancelled:
		return ErrCancelled
	case grant.State != StateActive || !grant.ExpiresAt.After(now):
		return ErrExpired
	default:
		return ErrUncertain
	}
}

func (store *Store) incident(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	kind, reason string,
	now time.Time,
) error {
	sum := sha256.Sum256([]byte(string(organizationID) + "|" + string(leaseID) + "|" + kind))
	_, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_runtime_lease_incidents (
			tenant_id,organization_id,incident_id,lease_id,kind,reason,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING
	`, store.tenantID, organizationID, "incident:"+hex.EncodeToString(sum[:16]),
		leaseID, kind, reason, now)
	return err
}

func policyBindingHash(policies []contracts.PolicyRef) string {
	sorted := append([]contracts.PolicyRef(nil), policies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	hash := sha256.New()
	for _, policy := range sorted {
		_, _ = fmt.Fprintf(hash, "%s|%d|%s\n", policy.ID, policy.Version, policy.Hash.Digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	return now, nil
}
