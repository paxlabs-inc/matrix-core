package companyrecovery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

type Authority struct {
	FounderKeyID      string
	FounderPublicKey  ed25519.PublicKey
	RuntimeKeyID      string
	RuntimePrivateKey ed25519.PrivateKey
}

func (value Authority) Validate() error {
	if validateToken("founder key_id", value.FounderKeyID) != nil ||
		len(value.FounderPublicKey) != ed25519.PublicKeySize ||
		validateToken("runtime key_id", value.RuntimeKeyID) != nil ||
		len(value.RuntimePrivateKey) != ed25519.PrivateKeySize || value.FounderKeyID == value.RuntimeKeyID ||
		ed25519.PublicKey(value.RuntimePrivateKey[32:]).Equal(value.FounderPublicKey) {
		return ErrUnauthorized
	}
	return nil
}

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	authority      Authority
	now            func() time.Time
	pitr           PITRBackend
	erasure        ErasureBackend
	machines       MachineKeyResolver
}

func NewStore(pool *pgxpool.Pool, userVault *vault.UserVault, tenantID string, organizationID contracts.OrganizationID, authority Authority, now func() time.Time) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || userVault.User() != tenantID ||
		validateToken("organization_id", string(organizationID)) != nil || authority.Validate() != nil || now == nil {
		return nil, fmt.Errorf("company recovery: PostgreSQL, tenant Vault, organization, authority, and time source are required")
	}
	authority.FounderPublicKey = slices.Clone(authority.FounderPublicKey)
	authority.RuntimePrivateKey = slices.Clone(authority.RuntimePrivateKey)
	store := &Store{pool: pool, vault: userVault, tenantID: tenantID, organizationID: organizationID, authority: authority, now: now}
	store.machines = store
	return store, nil
}

func (store *Store) AttachPITRBackend(backend PITRBackend) error {
	if backend == nil {
		return fmt.Errorf("company recovery: PITR backend is required")
	}
	store.pitr = backend
	return nil
}

func (store *Store) AttachErasureBackend(backend ErasureBackend) error {
	if backend == nil {
		return fmt.Errorf("company recovery: erasure backend is required")
	}
	store.erasure = backend
	return nil
}

func (store *Store) AttachMachineKeyResolver(resolver MachineKeyResolver) error {
	if resolver == nil {
		return fmt.Errorf("company recovery: machine key resolver is required")
	}
	store.machines = resolver
	return nil
}

func (store *Store) RegisterLimitPolicy(ctx context.Context, value LimitPolicy) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.authority.FounderKeyID || VerifyLimitPolicy(value, store.authority.FounderPublicKey) != nil ||
		value.Body.EffectiveAt.After(now) || !value.Body.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	canonical, hash, sealed, err := store.sealCanonical(store.limitPolicyAD(value.Body.ID, value.Body.Version), &value)
	_ = canonical
	if err != nil {
		return false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "limit-policy", string(value.Body.Scope.Kind), value.Body.Scope.ID, string(value.Body.Resource)); err != nil {
		return false, err
	}
	var existing string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_recovery_limit_policies
		WHERE tenant_id=$1 AND organization_id=$2 AND policy_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version).Scan(&existing)
	if err == nil {
		if existing != hash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var headVersion uint64
	var headHash string
	err = tx.QueryRow(ctx, `
		SELECT version,content_hash FROM workforce_recovery_limit_policy_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND scope_kind=$3 AND scope_id=$4 AND resource=$5
		FOR UPDATE
	`, store.tenantID, store.organizationID, value.Body.Scope.Kind, value.Body.Scope.ID, value.Body.Resource).Scan(&headVersion, &headHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (value.Body.Version != 1 || value.Body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (value.Body.Version != headVersion+1 || value.Body.Supersedes == nil || value.Body.Supersedes.Digest != headHash):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_limit_policies (
			tenant_id,organization_id,policy_id,version,scope_kind,scope_id,resource,
			soft_limit,hard_limit,window_micros,max_reservation_micros,open_circuit,
			content_hash,canonical_hash,key_id,sealed_policy,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
		value.Body.Scope.Kind, value.Body.Scope.ID, value.Body.Resource, value.Body.SoftLimit,
		value.Body.HardLimit, value.Body.Window.Microseconds(), value.Body.MaximumReservationAge.Microseconds(),
		value.Body.OpenCircuitOnExhaustion, value.ContentHash.Digest, hash.Digest,
		value.Signature.KeyID, sealed, value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, fmt.Errorf("company recovery: insert limit policy: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_limit_policy_heads (
			tenant_id,organization_id,scope_kind,scope_id,resource,policy_id,version,
			content_hash,effective_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (tenant_id,organization_id,scope_kind,scope_id,resource) DO UPDATE SET
			policy_id=EXCLUDED.policy_id,version=EXCLUDED.version,content_hash=EXCLUDED.content_hash,
			effective_at=EXCLUDED.effective_at,expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.Body.Scope.Kind, value.Body.Scope.ID,
		value.Body.Resource, value.Body.ID, value.Body.Version, value.ContentHash.Digest,
		value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, fmt.Errorf("company recovery: update limit policy head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) RegisterRecoveryPolicy(ctx context.Context, value RecoveryPolicy) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID ||
		value.Signature.KeyID != store.authority.FounderKeyID || VerifyRecoveryPolicy(value, store.authority.FounderPublicKey) != nil ||
		value.Body.EffectiveAt.After(now) || !value.Body.ExpiresAt.After(now) || value.Body.PITRRequired && store.pitr == nil {
		return false, ErrUnauthorized
	}
	_, hash, sealed, err := store.sealCanonical(store.recoveryPolicyAD(value.Body.ID, value.Body.Version), &value)
	if err != nil {
		return false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "recovery-policy"); err != nil {
		return false, err
	}
	var existing string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_recovery_policies
		WHERE tenant_id=$1 AND organization_id=$2 AND policy_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version).Scan(&existing)
	if err == nil {
		if existing != hash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var headVersion uint64
	var headHash string
	err = tx.QueryRow(ctx, `
		SELECT version,content_hash FROM workforce_recovery_policy_heads
		WHERE tenant_id=$1 AND organization_id=$2 FOR UPDATE
	`, store.tenantID, store.organizationID).Scan(&headVersion, &headHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && (value.Body.Version != 1 || value.Body.Supersedes != nil):
		return false, ErrConflict
	case err == nil && (value.Body.Version != headVersion+1 || value.Body.Supersedes == nil || value.Body.Supersedes.Digest != headHash):
		return false, ErrConflict
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_policies (
			tenant_id,organization_id,policy_id,version,backup_interval_micros,rpo_micros,
			rto_micros,pitr_required,max_archive_bytes,content_hash,canonical_hash,key_id,
			sealed_policy,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
		value.Body.BackupInterval.Microseconds(), value.Body.RPO.Microseconds(), value.Body.RTO.Microseconds(),
		value.Body.PITRRequired, value.Body.MaximumArchiveBytes, value.ContentHash.Digest, hash.Digest,
		value.Signature.KeyID, sealed, value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, fmt.Errorf("company recovery: insert recovery policy: %w", err)
	}
	for _, rule := range value.Body.Rules {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_retention_rules (
				tenant_id,organization_id,policy_id,policy_version,data_class,
				retention_micros,action
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
			rule.Class, rule.Retention.Microseconds(), rule.Action)
		if err != nil {
			return false, fmt.Errorf("company recovery: insert retention rule: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_policy_heads (
			tenant_id,organization_id,policy_id,version,content_hash,effective_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			policy_id=EXCLUDED.policy_id,version=EXCLUDED.version,content_hash=EXCLUDED.content_hash,
			effective_at=EXCLUDED.effective_at,expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Version,
		value.ContentHash.Digest, value.Body.EffectiveAt, value.Body.ExpiresAt, now)
	if err != nil {
		return false, fmt.Errorf("company recovery: update recovery policy head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) LoadRecoveryPolicy(ctx context.Context, requireCurrent bool) (RecoveryPolicy, error) {
	var policyID RecoveryPolicyID
	var version uint64
	var contentHash string
	var effectiveAt, expiresAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT policy_id,version,content_hash,effective_at,expires_at
		FROM workforce_recovery_policy_heads
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, store.organizationID).Scan(&policyID, &version, &contentHash, &effectiveAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryPolicy{}, ErrNotFound
	}
	if err != nil {
		return RecoveryPolicy{}, err
	}
	if requireCurrent {
		now, err := store.currentTime()
		if err != nil {
			return RecoveryPolicy{}, err
		}
		if effectiveAt.After(now) || !expiresAt.After(now) {
			return RecoveryPolicy{}, ErrNotFound
		}
	}
	var canonicalHash, keyID string
	var sealed []byte
	err = store.pool.QueryRow(ctx, `
		SELECT canonical_hash,key_id,sealed_policy
		FROM workforce_recovery_policies
		WHERE tenant_id=$1 AND organization_id=$2 AND policy_id=$3 AND version=$4 AND content_hash=$5
	`, store.tenantID, store.organizationID, policyID, version, contentHash).Scan(&canonicalHash, &keyID, &sealed)
	if err != nil {
		return RecoveryPolicy{}, ErrNotFound
	}
	opened, err := store.vault.OpenRecord(store.recoveryPolicyAD(policyID, version), sealed)
	if err != nil {
		return RecoveryPolicy{}, ErrUnauthorized
	}
	value, err := contracts.DecodeCanonical[RecoveryPolicy, *RecoveryPolicy](opened)
	if err != nil || value.ContentHash.Digest != contentHash || value.Signature.KeyID != keyID ||
		VerifyRecoveryPolicy(value, store.authority.FounderPublicKey) != nil {
		return RecoveryPolicy{}, ErrUnauthorized
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil || hash.Digest != canonicalHash {
		return RecoveryPolicy{}, ErrUnauthorized
	}
	return value, nil
}

func (store *Store) Admit(ctx context.Context, request UsageRequest) (UsageReceipt, error) {
	now, err := store.currentTime()
	if err != nil {
		return UsageReceipt{}, err
	}
	if request.Validate() != nil || request.OrganizationID != store.organizationID || request.RequestedAt.After(now) || !request.ExpiresAt.After(now) {
		return UsageReceipt{}, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UsageReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if existing, found, loadErr := store.loadUsageReplayTx(ctx, tx, request); loadErr != nil {
		return UsageReceipt{}, loadErr
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return UsageReceipt{}, err
		}
		if existing.State == ReservationDenied {
			return existing, ErrLimitExceeded
		}
		return existing, nil
	}
	if err := store.assertOperationalTx(ctx, tx); err != nil {
		return UsageReceipt{}, err
	}
	if err := store.expireReservationsTx(ctx, tx, now); err != nil {
		return UsageReceipt{}, err
	}
	type binding struct {
		policy      LimitPolicy
		windowStart time.Time
		used        uint64
		reserved    uint64
	}
	bindings := make([]binding, 0, len(request.Scopes))
	companyPolicy := false
	for _, scope := range request.Scopes {
		var circuitOpen bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM workforce_recovery_circuits
			WHERE tenant_id=$1 AND organization_id=$2 AND scope_kind=$3 AND scope_id=$4
			  AND resource=$5 AND state='open')`, store.tenantID, store.organizationID,
			scope.Kind, scope.ID, request.Resource).Scan(&circuitOpen); err != nil {
			return UsageReceipt{}, err
		}
		if circuitOpen {
			return UsageReceipt{}, ErrCircuitOpen
		}
		policy, loadErr := store.loadLimitPolicyTx(ctx, tx, scope, request.Resource, now)
		if errors.Is(loadErr, ErrNotFound) {
			return UsageReceipt{}, ErrNoLimitPolicy
		}
		if loadErr != nil {
			return UsageReceipt{}, loadErr
		}
		if scope.Kind == ScopeCompany {
			companyPolicy = true
		}
		if request.ExpiresAt.Sub(request.RequestedAt) > policy.Body.MaximumReservationAge {
			return UsageReceipt{}, ErrLimitExceeded
		}
		windowStart := truncateWindow(now, policy.Body.Window)
		if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "usage", string(policy.Body.ID), fmt.Sprint(policy.Body.Version), windowStart.Format(time.RFC3339Nano)); err != nil {
			return UsageReceipt{}, err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_usage_windows (
				tenant_id,organization_id,policy_id,policy_version,content_hash,
				window_started_at,window_ends_at,used_units,reserved_units,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,0,0,$8)
			ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, policy.Body.ID, policy.Body.Version,
			policy.ContentHash.Digest, windowStart, windowStart.Add(policy.Body.Window), now)
		if err != nil {
			return UsageReceipt{}, err
		}
		var used, reserved uint64
		err = tx.QueryRow(ctx, `
			SELECT used_units,reserved_units FROM workforce_recovery_usage_windows
			WHERE tenant_id=$1 AND organization_id=$2 AND policy_id=$3 AND policy_version=$4
			  AND window_started_at=$5 FOR UPDATE
		`, store.tenantID, store.organizationID, policy.Body.ID, policy.Body.Version, windowStart).Scan(&used, &reserved)
		if err != nil {
			return UsageReceipt{}, err
		}
		bindings = append(bindings, binding{policy: policy, windowStart: windowStart, used: used, reserved: reserved})
	}
	if !companyPolicy || len(bindings) == 0 {
		return UsageReceipt{}, ErrNoLimitPolicy
	}
	policyHashes := make([]contracts.ContentHash, 0, len(bindings))
	denied := false
	softExceeded := false
	for _, item := range bindings {
		policyHashes = append(policyHashes, item.policy.ContentHash)
		total := item.used + item.reserved
		if total > ^uint64(0)-request.Units || total+request.Units > item.policy.Body.HardLimit {
			denied = true
		}
		if total > ^uint64(0)-request.Units || total+request.Units > item.policy.Body.SoftLimit {
			softExceeded = true
		}
	}
	slices.SortFunc(policyHashes, func(left, right contracts.ContentHash) int { return strings.Compare(left.Digest, right.Digest) })
	state, reason := ReservationReserved, "reserved"
	if denied {
		state, reason = ReservationDenied, "hard_limit_exhausted"
	}
	receipt := UsageReceipt{SchemaVersion: SchemaVersion, Request: request, State: state,
		PolicyHashes: policyHashes, ReservedAt: now, ReasonCode: reason}
	if denied {
		finalized := now
		receipt.FinalizedAt = &finalized
	}
	canonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return UsageReceipt{}, err
	}
	sealed, err := store.vault.SealRecord(store.reservationAD(request.ID), canonical)
	if err != nil {
		return UsageReceipt{}, err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_recovery_reservations (
			tenant_id,organization_id,reservation_id,resource,units,operation,idempotency_key,
			irreversible,state,reason_code,sealed_receipt,requested_at,reserved_at,expires_at,finalized_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, request.ID, request.Resource, request.Units,
		request.Operation, request.IdempotencyKey, request.Irreversible, state, reason, sealed,
		request.RequestedAt, now, request.ExpiresAt, receipt.FinalizedAt)
	if err != nil {
		return UsageReceipt{}, err
	}
	if command.RowsAffected() != 1 {
		return UsageReceipt{}, ErrConflict
	}
	for _, item := range bindings {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_recovery_reservation_bindings (
				tenant_id,organization_id,reservation_id,policy_id,policy_version,
				content_hash,window_started_at,units
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, store.tenantID, store.organizationID, request.ID, item.policy.Body.ID,
			item.policy.Body.Version, item.policy.ContentHash.Digest, item.windowStart, request.Units)
		if err != nil {
			return UsageReceipt{}, err
		}
		if !denied {
			_, err = tx.Exec(ctx, `
				UPDATE workforce_recovery_usage_windows SET reserved_units=reserved_units+$1,updated_at=$2
				WHERE tenant_id=$3 AND organization_id=$4 AND policy_id=$5 AND policy_version=$6
				  AND window_started_at=$7
			`, request.Units, now, store.tenantID, store.organizationID,
				item.policy.Body.ID, item.policy.Body.Version, item.windowStart)
			if err != nil {
				return UsageReceipt{}, err
			}
		}
		if denied && item.policy.Body.OpenCircuitOnExhaustion {
			if err := store.openResourceCircuitTx(ctx, tx, item.policy.Body.Scope, request.Resource, "hard_limit_exhausted", now); err != nil {
				return UsageReceipt{}, err
			}
		}
	}
	if denied || softExceeded {
		kind, code := IncidentOverload, "soft_limit_exceeded"
		if denied {
			kind, code = IncidentResourceExhausted, "hard_limit_exhausted"
		}
		incident := Incident{SchemaVersion: SchemaVersion,
			ID: IncidentID(stableID("incident", string(request.ID), code)), OrganizationID: store.organizationID,
			Kind: kind, Scope: request.Scopes[0], Resource: request.Resource, SafeCode: code,
			RecordKind: "reservation", RecordID: string(request.ID), Observed: request.Units, CreatedAt: now}
		if err := store.recordIncidentTx(ctx, tx, incident); err != nil {
			return UsageReceipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return UsageReceipt{}, err
	}
	if denied {
		return receipt, ErrLimitExceeded
	}
	return receipt, nil
}

func (store *Store) CommitUsage(ctx context.Context, id ReservationID, actualUnits uint64) (UsageReceipt, error) {
	if actualUnits == 0 {
		return UsageReceipt{}, fmt.Errorf("company recovery: committed usage must be positive")
	}
	return store.finalizeUsage(ctx, id, ReservationCommitted, actualUnits, "committed")
}

func (store *Store) ReleaseUsage(ctx context.Context, id ReservationID, reasonCode string) (UsageReceipt, error) {
	if validateToken("release reason_code", reasonCode) != nil {
		return UsageReceipt{}, fmt.Errorf("company recovery: release reason is invalid")
	}
	return store.finalizeUsage(ctx, id, ReservationReleased, 0, reasonCode)
}

func (store *Store) RecordMetric(ctx context.Context, value MetricSample) (bool, error) {
	if value.Validate() != nil || value.OrganizationID != store.organizationID {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	hash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.metricAD(value.ID), canonical)
	if err != nil {
		return false, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_recovery_metrics (
			tenant_id,organization_id,metric_id,metric_kind,resource,value,unit,
			canonical_hash,sealed_metric,observed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.ID, value.Kind, value.Resource,
		value.Value, value.Unit, hash.Digest, sealed, value.ObservedAt)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		var existing string
		err := store.pool.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_metrics
			WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3`, store.tenantID, store.organizationID, value.ID).Scan(&existing)
		if err != nil || existing != hash.Digest {
			return false, ErrConflict
		}
		return true, nil
	}
	return false, nil
}

func (store *Store) RecordTrace(ctx context.Context, value TraceSpan) (bool, error) {
	if value.Validate() != nil || value.OrganizationID != store.organizationID {
		return false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	hash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.traceAD(value.ID), canonical)
	if err != nil {
		return false, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_recovery_traces (
			tenant_id,organization_id,trace_id,parent_id,operation,resource_kind,resource_id,
			status,safe_code,canonical_hash,sealed_trace,started_at,finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.ID, optionalTraceID(value.ParentID), value.Operation,
		value.ResourceKind, value.ResourceID, value.Status, value.SafeCode, hash.Digest,
		sealed, value.StartedAt, value.FinishedAt)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		var existing string
		err := store.pool.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_traces
			WHERE tenant_id=$1 AND organization_id=$2 AND trace_id=$3`, store.tenantID,
			store.organizationID, value.ID).Scan(&existing)
		if err != nil || existing != hash.Digest {
			return false, ErrConflict
		}
		return true, nil
	}
	return false, nil
}

func (store *Store) RecordIncident(ctx context.Context, value Incident) (bool, error) {
	if value.Validate() != nil || value.OrganizationID != store.organizationID {
		return false, ErrUnauthorized
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	expectedHash := hashBytes(canonical)
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT canonical_hash FROM workforce_recovery_incidents
		WHERE tenant_id=$1 AND organization_id=$2 AND incident_id=$3 FOR SHARE`, store.tenantID,
		store.organizationID, value.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != expectedHash.Digest {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err := store.recordIncidentTx(ctx, tx, value); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func (store *Store) BeginShutdown(ctx context.Context, id ShutdownID, reasonCode string) (ShutdownReceipt, error) {
	now, err := store.currentTime()
	if err != nil {
		return ShutdownReceipt{}, err
	}
	value := ShutdownReceipt{Body: ShutdownReceiptBody{SchemaVersion: ShutdownSchemaVersion,
		ID: id, OrganizationID: store.organizationID, State: ShutdownDraining,
		ReasonCode: reasonCode, StartedAt: now}}
	if value.Body.Validate() != nil {
		return ShutdownReceipt{}, ErrUnauthorized
	}
	if err := signSimple(&value, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { value.Signature = signature }); err != nil {
		return ShutdownReceipt{}, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	hash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.shutdownAD(id), canonical)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ShutdownReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "shutdown"); err != nil {
		return ShutdownReceipt{}, err
	}
	var existingSealed []byte
	err = tx.QueryRow(ctx, `SELECT sealed_receipt FROM workforce_recovery_shutdowns
		WHERE tenant_id=$1 AND organization_id=$2 AND shutdown_id=$3 FOR UPDATE`, store.tenantID,
		store.organizationID, id).Scan(&existingSealed)
	if err == nil {
		existing, openErr := store.openShutdownReceipt(id, existingSealed)
		if openErr != nil || existing.Body.ReasonCode != reasonCode {
			return ShutdownReceipt{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ShutdownReceipt{}, err
	}
	var activeID ShutdownID
	err = tx.QueryRow(ctx, `SELECT shutdown_id FROM workforce_recovery_shutdown_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='draining' FOR UPDATE`, store.tenantID,
		store.organizationID).Scan(&activeID)
	if err == nil && activeID != id {
		return ShutdownReceipt{}, ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ShutdownReceipt{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_shutdowns (
			tenant_id,organization_id,shutdown_id,state,reason_code,canonical_hash,
			key_id,sealed_receipt,started_at,updated_at
		) VALUES ($1,$2,$3,'draining',$4,$5,$6,$7,$8,$8)
	`, store.tenantID, store.organizationID, id, reasonCode, hash.Digest,
		store.authority.RuntimeKeyID, sealed, now)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_shutdown_heads (
			tenant_id,organization_id,shutdown_id,state,updated_at
		) VALUES ($1,$2,$3,'draining',$4)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			shutdown_id=EXCLUDED.shutdown_id,state='draining',updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, id, now)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ShutdownReceipt{}, err
	}
	return value, nil
}

func (store *Store) CompleteShutdown(ctx context.Context, id ShutdownID) (ShutdownReceipt, error) {
	now, err := store.currentTime()
	if err != nil {
		return ShutdownReceipt{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ShutdownReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var reason string
	var startedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT reason_code,started_at FROM workforce_recovery_shutdowns
		WHERE tenant_id=$1 AND organization_id=$2 AND shutdown_id=$3 AND state='draining' FOR UPDATE
	`, store.tenantID, store.organizationID, id).Scan(&reason, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var sealed []byte
		loadErr := tx.QueryRow(ctx, `SELECT sealed_receipt FROM workforce_recovery_shutdowns
			WHERE tenant_id=$1 AND organization_id=$2 AND shutdown_id=$3 AND state='stopped'`,
			store.tenantID, store.organizationID, id).Scan(&sealed)
		if loadErr != nil {
			return ShutdownReceipt{}, ErrNotFound
		}
		existing, openErr := store.openShutdownReceipt(id, sealed)
		if openErr != nil {
			return ShutdownReceipt{}, openErr
		}
		return existing, tx.Commit(ctx)
	}
	if err != nil {
		return ShutdownReceipt{}, ErrNotFound
	}
	counts, err := store.neutralizeEphemeralTx(ctx, tx, "graceful_shutdown", string(id), now)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	released, err := store.releaseAllReservationsTx(ctx, tx, now, "graceful_shutdown")
	if err != nil {
		return ShutdownReceipt{}, err
	}
	completed := now
	value := ShutdownReceipt{Body: ShutdownReceiptBody{SchemaVersion: ShutdownSchemaVersion,
		ID: id, OrganizationID: store.organizationID, State: ShutdownStopped, ReasonCode: reason,
		ReleasedReservations: released, CancelledLeases: counts.CancelledRuntimeLeases,
		CoalescedWakes: counts.CoalescedWakes, QuarantinedEffects: counts.QuarantinedEffects,
		StartedAt: startedAt.UTC(), CompletedAt: &completed}}
	if err := signSimple(&value, store.authority.RuntimeKeyID, store.authority.RuntimePrivateKey,
		func(signature contracts.Signature) { value.Signature = signature }); err != nil {
		return ShutdownReceipt{}, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	hash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.shutdownAD(id), canonical)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE workforce_recovery_shutdowns SET state='stopped',canonical_hash=$1,key_id=$2,
			sealed_receipt=$3,released_reservations=$4,cancelled_leases=$5,
			coalesced_wakes=$6,quarantined_effects=$7,completed_at=$8,updated_at=$8
		WHERE tenant_id=$9 AND organization_id=$10 AND shutdown_id=$11 AND state='draining'
	`, hash.Digest, store.authority.RuntimeKeyID, sealed, released, counts.CancelledRuntimeLeases,
		counts.CoalescedWakes, counts.QuarantinedEffects, now,
		store.tenantID, store.organizationID, id)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_shutdown_heads SET state='stopped',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND shutdown_id=$4`, now, store.tenantID, store.organizationID, id)
	if err != nil {
		return ShutdownReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ShutdownReceipt{}, err
	}
	return value, nil
}

func (store *Store) AssertOperational(ctx context.Context) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.assertOperationalTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) finalizeUsage(ctx context.Context, id ReservationID, state ReservationState, actual uint64, reason string) (UsageReceipt, error) {
	now, err := store.currentTime()
	if err != nil {
		return UsageReceipt{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UsageReceipt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var sealed []byte
	var currentState ReservationState
	var reservedUnits uint64
	err = tx.QueryRow(ctx, `
		SELECT state,units,sealed_receipt FROM workforce_recovery_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND reservation_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, id).Scan(&currentState, &reservedUnits, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageReceipt{}, ErrNotFound
	}
	if err != nil {
		return UsageReceipt{}, err
	}
	opened, err := store.vault.OpenRecord(store.reservationAD(id), sealed)
	if err != nil {
		return UsageReceipt{}, ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[UsageReceipt, *UsageReceipt](opened)
	if err != nil {
		return UsageReceipt{}, ErrUnauthorized
	}
	if currentState != ReservationReserved {
		if currentState == state && (state != ReservationCommitted || receipt.ActualUnits == actual) {
			return receipt, tx.Commit(ctx)
		}
		return UsageReceipt{}, ErrConflict
	}
	if actual > reservedUnits {
		return UsageReceipt{}, ErrLimitExceeded
	}
	rows, err := tx.Query(ctx, `
		SELECT policy_id,policy_version,window_started_at,units
		FROM workforce_recovery_reservation_bindings
		WHERE tenant_id=$1 AND organization_id=$2 AND reservation_id=$3
		ORDER BY policy_id,policy_version
	`, store.tenantID, store.organizationID, id)
	if err != nil {
		return UsageReceipt{}, err
	}
	type bound struct {
		policyID string
		version  uint64
		window   time.Time
		units    uint64
	}
	var bindings []bound
	for rows.Next() {
		var item bound
		if err := rows.Scan(&item.policyID, &item.version, &item.window, &item.units); err != nil {
			rows.Close()
			return UsageReceipt{}, err
		}
		bindings = append(bindings, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return UsageReceipt{}, err
	}
	rows.Close()
	for _, item := range bindings {
		command, updateErr := tx.Exec(ctx, `
			UPDATE workforce_recovery_usage_windows
			SET reserved_units=reserved_units-$1,used_units=used_units+$2,updated_at=$3
			WHERE tenant_id=$4 AND organization_id=$5 AND policy_id=$6 AND policy_version=$7
			  AND window_started_at=$8 AND reserved_units>=$1
		`, item.units, actual, now, store.tenantID, store.organizationID, item.policyID, item.version, item.window)
		if updateErr != nil {
			return UsageReceipt{}, updateErr
		}
		if command.RowsAffected() != 1 {
			return UsageReceipt{}, ErrConflict
		}
	}
	finalized := now
	receipt.State, receipt.ActualUnits, receipt.FinalizedAt, receipt.ReasonCode = state, actual, &finalized, reason
	canonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return UsageReceipt{}, err
	}
	sealed, err = store.vault.SealRecord(store.reservationAD(id), canonical)
	if err != nil {
		return UsageReceipt{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_reservations
		SET state=$1,reason_code=$2,sealed_receipt=$3,actual_units=$4,finalized_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND reservation_id=$8 AND state='reserved'`,
		state, reason, sealed, actual, now, store.tenantID, store.organizationID, id)
	if err != nil {
		return UsageReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UsageReceipt{}, err
	}
	return receipt, nil
}

func (store *Store) loadLimitPolicyTx(ctx context.Context, tx pgx.Tx, scope ScopeRef, resource ResourceKind, at time.Time) (LimitPolicy, error) {
	var policyID LimitPolicyID
	var version uint64
	var contentHash, canonicalHash, keyID string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT head.policy_id,head.version,head.content_hash,policy.canonical_hash,
		       policy.key_id,policy.sealed_policy
		FROM workforce_recovery_limit_policy_heads head
		JOIN workforce_recovery_limit_policies policy
		  ON policy.tenant_id=head.tenant_id AND policy.organization_id=head.organization_id
		 AND policy.policy_id=head.policy_id AND policy.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.scope_kind=$3
		  AND head.scope_id=$4 AND head.resource=$5 AND head.effective_at<=$6 AND head.expires_at>$6
		FOR SHARE OF head,policy
	`, store.tenantID, store.organizationID, scope.Kind, scope.ID, resource, at).Scan(
		&policyID, &version, &contentHash, &canonicalHash, &keyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return LimitPolicy{}, ErrNotFound
	}
	if err != nil {
		return LimitPolicy{}, err
	}
	opened, err := store.vault.OpenRecord(store.limitPolicyAD(policyID, version), sealed)
	if err != nil {
		return LimitPolicy{}, ErrUnauthorized
	}
	value, err := contracts.DecodeCanonical[LimitPolicy, *LimitPolicy](opened)
	if err != nil || value.ContentHash.Digest != contentHash || value.Signature.KeyID != keyID ||
		VerifyLimitPolicy(value, store.authority.FounderPublicKey) != nil {
		return LimitPolicy{}, ErrUnauthorized
	}
	hash, err := contracts.HashCanonical(&value)
	if err != nil || hash.Digest != canonicalHash {
		return LimitPolicy{}, ErrUnauthorized
	}
	return value, nil
}

func (store *Store) assertOperationalTx(ctx context.Context, tx pgx.Tx) error {
	var restorePending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM workforce_recovery_restore_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='reconciliation_required')`,
		store.tenantID, store.organizationID).Scan(&restorePending); err != nil {
		return err
	}
	if restorePending {
		return ErrRestoreQuarantined
	}
	var draining bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM workforce_recovery_shutdown_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='draining')`,
		store.tenantID, store.organizationID).Scan(&draining); err != nil {
		return err
	}
	if draining {
		return ErrDraining
	}
	return nil
}

func (store *Store) openResourceCircuitTx(ctx context.Context, tx pgx.Tx, scope ScopeRef, resource ResourceKind, reason string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_recovery_circuits (
			tenant_id,organization_id,scope_kind,scope_id,resource,state,reason_code,opened_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,'open',$6,$7,$7)
		ON CONFLICT (tenant_id,organization_id,scope_kind,scope_id,resource) DO UPDATE SET
			state='open',reason_code=EXCLUDED.reason_code,opened_at=EXCLUDED.opened_at,
			closed_at=NULL,updated_at=EXCLUDED.updated_at,version=workforce_recovery_circuits.version+1
	`, store.tenantID, store.organizationID, scope.Kind, scope.ID, resource, reason, now)
	return err
}

func (store *Store) recordIncidentTx(ctx context.Context, tx pgx.Tx, value Incident) error {
	if value.Validate() != nil || value.OrganizationID != store.organizationID {
		return ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	hash := hashBytes(canonical)
	sealed, err := store.vault.SealRecord(store.incidentAD(value.ID), canonical)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_recovery_incidents (
			tenant_id,organization_id,incident_id,incident_kind,scope_kind,scope_id,
			resource,safe_code,record_kind,record_id,observed_value,limit_value,
			canonical_hash,sealed_incident,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.ID, value.Kind, value.Scope.Kind,
		value.Scope.ID, value.Resource, value.SafeCode, value.RecordKind, value.RecordID,
		value.Observed, value.Limit, hash.Digest, sealed, value.CreatedAt)
	return err
}

func (store *Store) expireReservationsTx(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT reservation_id FROM workforce_recovery_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND state='reserved' AND expires_at<=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, now)
	if err != nil {
		return err
	}
	var ids []ReservationID
	for rows.Next() {
		var id ReservationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := store.releaseReservationBindingsTx(ctx, tx, id, now); err != nil {
			return err
		}
		if err := store.updateReservationReceiptTx(ctx, tx, id, ReservationExpired, "reservation_expired", now); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) releaseAllReservationsTx(ctx context.Context, tx pgx.Tx, now time.Time, reason string) (uint64, error) {
	rows, err := tx.Query(ctx, `SELECT reservation_id FROM workforce_recovery_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND state='reserved' FOR UPDATE`, store.tenantID, store.organizationID)
	if err != nil {
		return 0, err
	}
	var ids []ReservationID
	for rows.Next() {
		var id ReservationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		if err := store.releaseReservationBindingsTx(ctx, tx, id, now); err != nil {
			return 0, err
		}
		if err := store.updateReservationReceiptTx(ctx, tx, id, ReservationReleased, reason, now); err != nil {
			return 0, err
		}
	}
	return uint64(len(ids)), nil
}

func (store *Store) releaseReservationBindingsTx(ctx context.Context, tx pgx.Tx, id ReservationID, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE workforce_recovery_usage_windows usage SET
			reserved_units=usage.reserved_units-binding.units,updated_at=$1
		FROM workforce_recovery_reservation_bindings binding
		WHERE binding.tenant_id=$2 AND binding.organization_id=$3 AND binding.reservation_id=$4
		  AND usage.tenant_id=binding.tenant_id AND usage.organization_id=binding.organization_id
		  AND usage.policy_id=binding.policy_id AND usage.policy_version=binding.policy_version
		  AND usage.window_started_at=binding.window_started_at AND usage.reserved_units>=binding.units
	`, now, store.tenantID, store.organizationID, id)
	return err
}

func (store *Store) loadUsageReplayTx(ctx context.Context, tx pgx.Tx, request UsageRequest) (UsageReceipt, bool, error) {
	var existingID ReservationID
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT reservation_id,sealed_receipt FROM workforce_recovery_reservations
		WHERE tenant_id=$1 AND organization_id=$2
		  AND (reservation_id=$3 OR (resource=$4 AND idempotency_key=$5))
		ORDER BY CASE WHEN reservation_id=$3 THEN 0 ELSE 1 END LIMIT 1 FOR UPDATE
	`, store.tenantID, store.organizationID, request.ID, request.Resource, request.IdempotencyKey).Scan(&existingID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageReceipt{}, false, nil
	}
	if err != nil {
		return UsageReceipt{}, false, err
	}
	opened, err := store.vault.OpenRecord(store.reservationAD(existingID), sealed)
	if err != nil {
		return UsageReceipt{}, false, ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[UsageReceipt, *UsageReceipt](opened)
	if err != nil {
		return UsageReceipt{}, false, ErrUnauthorized
	}
	existingRequest, err := contracts.EncodeCanonical(&receipt.Request)
	if err != nil {
		return UsageReceipt{}, false, err
	}
	providedRequest, err := contracts.EncodeCanonical(&request)
	if err != nil {
		return UsageReceipt{}, false, err
	}
	if !bytes.Equal(existingRequest, providedRequest) {
		return UsageReceipt{}, false, ErrConflict
	}
	return receipt, true, nil
}

func (store *Store) updateReservationReceiptTx(ctx context.Context, tx pgx.Tx, id ReservationID, state ReservationState, reason string, now time.Time) error {
	var sealed []byte
	err := tx.QueryRow(ctx, `SELECT sealed_receipt FROM workforce_recovery_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND reservation_id=$3 AND state='reserved' FOR UPDATE`,
		store.tenantID, store.organizationID, id).Scan(&sealed)
	if err != nil {
		return err
	}
	opened, err := store.vault.OpenRecord(store.reservationAD(id), sealed)
	if err != nil {
		return ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[UsageReceipt, *UsageReceipt](opened)
	if err != nil || receipt.State != ReservationReserved {
		return ErrUnauthorized
	}
	finalized := now
	receipt.State, receipt.ActualUnits, receipt.ReasonCode, receipt.FinalizedAt = state, 0, reason, &finalized
	canonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return err
	}
	sealed, err = store.vault.SealRecord(store.reservationAD(id), canonical)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE workforce_recovery_reservations
		SET state=$1,reason_code=$2,sealed_receipt=$3,actual_units=0,finalized_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND reservation_id=$7 AND state='reserved'`,
		state, reason, sealed, now, store.tenantID, store.organizationID, id)
	return err
}

func (store *Store) openShutdownReceipt(id ShutdownID, sealed []byte) (ShutdownReceipt, error) {
	opened, err := store.vault.OpenRecord(store.shutdownAD(id), sealed)
	if err != nil {
		return ShutdownReceipt{}, ErrUnauthorized
	}
	receipt, err := contracts.DecodeCanonical[ShutdownReceipt, *ShutdownReceipt](opened)
	if err != nil || receipt.Signature.KeyID != store.authority.RuntimeKeyID ||
		VerifyShutdownReceipt(receipt, runtimePublicKey(store.authority.RuntimePrivateKey)) != nil {
		return ShutdownReceipt{}, ErrUnauthorized
	}
	return receipt, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("company recovery: time source must return UTC")
	}
	return now, nil
}

func (store *Store) sealCanonical(ad vault.AD, value contracts.Validatable) ([]byte, contracts.ContentHash, []byte, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return nil, contracts.ContentHash{}, nil, err
	}
	hash, err := contracts.HashCanonical(value)
	if err != nil {
		return nil, contracts.ContentHash{}, nil, err
	}
	sealed, err := store.vault.SealRecord(ad, canonical)
	return canonical, hash, sealed, err
}

func truncateWindow(value time.Time, window time.Duration) time.Time {
	return value.Truncate(window).UTC()
}

func stableID(prefix string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		hash.Write([]byte{0})
		hash.Write([]byte(value))
	}
	return prefix + ":" + hex.EncodeToString(hash.Sum(nil))[:32]
}

func lockScope(ctx context.Context, tx pgx.Tx, values ...string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, strings.Join(values, "|"))
	return err
}

func optionalTraceID(value *TraceID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func (store *Store) limitPolicyAD(id LimitPolicyID, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.limit-policy", Stream: strings.Join([]string{string(store.organizationID), string(id), fmt.Sprint(version)}, "/"), Schema: SchemaVersion}
}
func (store *Store) recoveryPolicyAD(id RecoveryPolicyID, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.policy", Stream: strings.Join([]string{string(store.organizationID), string(id), fmt.Sprint(version)}, "/"), Schema: SchemaVersion}
}
func (store *Store) reservationAD(id ReservationID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.reservation", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
func (store *Store) metricAD(id MetricID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.metric", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
func (store *Store) traceAD(id TraceID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.trace", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
func (store *Store) incidentAD(id IncidentID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.incident", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: SchemaVersion}
}
func (store *Store) shutdownAD(id ShutdownID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.company-recovery.shutdown", Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: ShutdownSchemaVersion}
}
