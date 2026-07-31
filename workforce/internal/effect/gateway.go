package effect

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
)

// Gateway is the single local writer for external effects.
type Gateway struct {
	pool      *pgxpool.Pool
	vault     *vault.UserVault
	leases    *lease.Store
	policy    *policy.Store
	breakers  *circuit.Store
	tenantID  string
	authority approval.Authority
	now       func() time.Time
	adapters  map[string]Adapter
}

// New constructs a credential-isolating effect gateway.
//
// ownerAuthority is the owner identity whose signature makes an approval
// authoritative. A gateway built without it cannot dispatch irreversible
// effects at all: it has no way to tell an owner-approved batch from one merely
// sealed by the tenant, so it refuses rather than guesses.
func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	leases *lease.Store,
	leaseAuthority *policy.Store,
	breakers *circuit.Store,
	tenantID string,
	ownerAuthority approval.Authority,
	now func() time.Time,
	adapters ...Adapter,
) (*Gateway, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || leases == nil || leaseAuthority == nil || breakers == nil ||
		tenantID == "" || now == nil {
		return nil, fmt.Errorf("effect: pool, Vault, runtime lease service, signed lease authority, circuit authority, tenant_id, and time source are required")
	}
	if err := ownerAuthority.Validate(); err != nil {
		return nil, err
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("effect: Vault user does not match tenant")
	}
	registry := make(map[string]Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, fmt.Errorf("effect: nil adapter")
		}
		name := adapter.Name()
		if err := validateToken("adapter name", name); err != nil {
			return nil, err
		}
		if _, exists := registry[name]; exists {
			return nil, fmt.Errorf("effect: duplicate adapter %q", name)
		}
		registry[name] = adapter
	}
	if len(registry) == 0 {
		return nil, fmt.Errorf("effect: at least one real adapter is required")
	}
	return &Gateway{
		pool: pool, vault: userVault, leases: leases, policy: leaseAuthority, breakers: breakers,
		tenantID: tenantID, authority: ownerAuthority.Clone(),
		now: now, adapters: registry,
	}, nil
}

// Execute preflights, durably marks dispatch, invokes one real adapter, and
// records authoritative evidence or ambiguity without blind retry.
func (gateway *Gateway) Execute(ctx context.Context, proposal Proposal) (Result, error) {
	now, adapter, proposalHash, err := gateway.preflight(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	existing, found, err := gateway.prepare(ctx, proposal, proposalHash, now)
	if err != nil {
		return Result{}, err
	}
	if found {
		existing.Deduplicated = true
		switch existing.State {
		case StatePrepared:
		case StateDispatching:
			if err := gateway.setState(ctx, proposal, StateExternallyAmbiguous, "", "restart_after_dispatch"); err != nil {
				return Result{}, err
			}
			existing.State = StateExternallyAmbiguous
			return existing, ErrAmbiguous
		case StateExternallyAmbiguous:
			return existing, ErrAmbiguous
		case StateSucceeded, StateFailed:
			return existing, nil
		default:
			return Result{}, ErrConflict
		}
	}
	permit, err := gateway.admit(ctx, proposal)
	if err != nil {
		return Result{ProposalID: proposal.ID, State: StatePrepared}, err
	}
	if err := gateway.beginDispatch(ctx, proposal, now); err != nil {
		_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
		return Result{}, err
	}
	dispatchContext, cancel := context.WithTimeout(ctx, proposal.Deadline.Sub(now))
	defer cancel()
	observation, dispatchErr := adapter.Dispatch(dispatchContext, Operation{
		OrganizationID: proposal.OrganizationID,
		SeatID:         proposal.SeatID,
		LeaseID:        proposal.LeaseID,
		Fence:          proposal.Fence,
		Name:           proposal.Operation, IdempotencyKey: proposal.IdempotencyKey,
		Input: append([]byte(nil), proposal.Input...),
	})
	if dispatchErr != nil {
		if observation.Started {
			if err := gateway.setState(ctx, proposal, StateExternallyAmbiguous,
				observation.ExternalID, "dispatch_outcome_unknown"); err != nil {
				_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
				return Result{}, err
			}
			if err := gateway.breakers.Fail(context.WithoutCancel(ctx), permit,
				"dispatch_outcome_unknown"); err != nil {
				return Result{}, err
			}
			return Result{
				ProposalID: proposal.ID, State: StateExternallyAmbiguous,
				ExternalID: observation.ExternalID, SafeErrorCode: "dispatch_outcome_unknown",
			}, ErrAmbiguous
		}
		if err := gateway.setState(ctx, proposal, StateFailed, "", "dispatch_not_started"); err != nil {
			_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
			return Result{}, err
		}
		if err := gateway.breakers.Fail(context.WithoutCancel(ctx), permit,
			"dispatch_not_started"); err != nil {
			return Result{}, err
		}
		return Result{
			ProposalID: proposal.ID, State: StateFailed,
			SafeErrorCode: "dispatch_not_started",
		}, ErrRejected
	}
	result, err := gateway.commitEvidence(ctx, proposal, observation)
	if err != nil {
		_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
		return Result{}, err
	}
	if err := gateway.breakers.Succeed(context.WithoutCancel(ctx), permit); err != nil {
		return result, err
	}
	return result, nil
}

// Reconcile probes one ambiguous or dispatching operation and commits only an
// authoritative provider observation.
func (gateway *Gateway) Reconcile(ctx context.Context, proposal Proposal) (Result, error) {
	adapter, proposalHash, err := gateway.probePreflight(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	existing, found, err := gateway.lookup(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	if !found || existing.proposalHash != proposalHash {
		return Result{}, ErrConflict
	}
	if existing.result.State == StateSucceeded || existing.result.State == StateFailed {
		existing.result.Deduplicated = true
		return existing.result, nil
	}
	if existing.result.State != StateDispatching &&
		existing.result.State != StateExternallyAmbiguous {
		return Result{}, ErrRejected
	}
	permit, err := gateway.admit(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	probe, probeErr := adapter.Probe(ctx, Operation{
		OrganizationID: proposal.OrganizationID,
		SeatID:         proposal.SeatID,
		LeaseID:        proposal.LeaseID,
		Fence:          proposal.Fence,
		Name:           proposal.Operation, IdempotencyKey: proposal.IdempotencyKey,
		Input: append([]byte(nil), proposal.Input...),
	})
	if probeErr != nil || !probe.Outcome.Valid() {
		_ = gateway.setProbe(ctx, proposal, skills.ProbeUnknown,
			probe.Dispatch.ExternalID, "probe_inconclusive")
		if err := gateway.breakers.Fail(context.WithoutCancel(ctx), permit,
			"probe_inconclusive"); err != nil {
			return Result{}, err
		}
		return Result{
			ProposalID: proposal.ID, State: StateExternallyAmbiguous,
			ExternalID: probe.Dispatch.ExternalID, SafeErrorCode: "probe_inconclusive",
			ProbeOutcome: skills.ProbeUnknown,
		}, ErrAmbiguous
	}
	if probe.Outcome == skills.ProbeCompletedOutOfBand {
		result, err := gateway.commitEvidence(ctx, proposal, probe.Dispatch)
		if err != nil {
			_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
			return Result{}, err
		}
		result.ProbeOutcome = probe.Outcome
		if err := gateway.setProbeOutcome(ctx, proposal, probe.Outcome); err != nil {
			return result, err
		}
		if err := gateway.breakers.Succeed(context.WithoutCancel(ctx), permit); err != nil {
			return result, err
		}
		return result, nil
	}
	safeCode := "probe_" + string(probe.Outcome)
	if err := gateway.setProbe(ctx, proposal, probe.Outcome,
		probe.Dispatch.ExternalID, safeCode); err != nil {
		_ = gateway.breakers.Release(context.WithoutCancel(ctx), permit)
		return Result{}, err
	}
	if err := gateway.breakers.Succeed(context.WithoutCancel(ctx), permit); err != nil {
		return Result{}, err
	}
	return Result{
		ProposalID: proposal.ID, State: StateExternallyAmbiguous,
		ExternalID: probe.Dispatch.ExternalID, SafeErrorCode: safeCode,
		ProbeOutcome: probe.Outcome,
	}, ErrAmbiguous
}

// Pending returns exact Vault-opened proposals that require a read-only sweep.
func (gateway *Gateway) Pending(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) ([]Proposal, error) {
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return nil, err
	}
	rows, err := gateway.pool.Query(ctx, `
		SELECT proposal_id,intent_id,seat_id,lease_id,fence,provider,operation,
			idempotency_key,skill_digest,operation_digest,skill_id,graph_node_id,effect_class,
			irreversible,COALESCE(approval_id,''),approval_cost_microunits,
			deadline,proposal_sealed
		FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2
		  AND state IN ('dispatching','externally_ambiguous')
		ORDER BY proposal_id
	`, gateway.tenantID, organizationID)
	if err != nil {
		return nil, fmt.Errorf("%w: query pending effects: %v", ErrUncertain, err)
	}
	defer rows.Close()
	result := make([]Proposal, 0)
	for rows.Next() {
		proposal := Proposal{OrganizationID: organizationID}
		var sealed []byte
		if err := rows.Scan(
			&proposal.ID, &proposal.IntentID, &proposal.SeatID, &proposal.LeaseID,
			&proposal.Fence, &proposal.Provider, &proposal.Operation,
			&proposal.IdempotencyKey, &proposal.SkillDigest.Digest,
			&proposal.OperationDigest.Digest, &proposal.SkillID, &proposal.NodeID,
			&proposal.EffectClass,
			&proposal.Irreversible, &proposal.ApprovalID, &proposal.ApprovalCost,
			&proposal.Deadline, &sealed,
		); err != nil {
			return nil, fmt.Errorf("%w: scan pending effect: %v", ErrUncertain, err)
		}
		if len(sealed) == 0 {
			return nil, fmt.Errorf("%w: pending proposal is not sealed", ErrUncertain)
		}
		proposal.SkillDigest.Algorithm = "sha256"
		proposal.OperationDigest.Algorithm = "sha256"
		proposal.Deadline = proposal.Deadline.UTC()
		proposal.Input, err = gateway.vault.OpenRecord(vault.AD{
			User: gateway.tenantID, Store: "workforce.effect.proposal",
			Stream: string(organizationID) + "/" + proposal.ID,
			Schema: contracts.SchemaVersionV1,
		}, sealed)
		if err != nil {
			return nil, fmt.Errorf("%w: open pending proposal: %v", ErrUncertain, err)
		}
		if err := proposal.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid pending proposal: %v", ErrUncertain, err)
		}
		result = append(result, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate pending effects: %v", ErrUncertain, err)
	}
	return result, nil
}

func (gateway *Gateway) preflight(
	ctx context.Context,
	proposal Proposal,
) (time.Time, Adapter, string, error) {
	if err := proposal.Validate(); err != nil {
		return time.Time{}, nil, "", err
	}
	now := gateway.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, nil, "", fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	if !proposal.Deadline.After(now) {
		return time.Time{}, nil, "", ErrRejected
	}
	adapter, exists := gateway.adapters[proposal.Provider]
	if !exists {
		return time.Time{}, nil, "", ErrRejected
	}
	grant, err := gateway.leases.Authorize(
		ctx, proposal.OrganizationID, proposal.LeaseID, proposal.Fence,
	)
	if err != nil {
		return time.Time{}, nil, "", err
	}
	if grant.SeatID != proposal.SeatID {
		return time.Time{}, nil, "", ErrRejected
	}
	if err := gateway.policy.AuthorizeLease(ctx, proposal.LeaseID); err != nil {
		return time.Time{}, nil, "", ErrRejected
	}
	registered, err := gateway.policy.LoadLease(ctx, proposal.LeaseID)
	if err != nil ||
		registered.OrganizationID != proposal.OrganizationID ||
		registered.SeatID != proposal.SeatID ||
		registered.Fence != proposal.Fence ||
		registered.ID != proposal.LeaseID ||
		!registered.ExpiresAt.After(now) {
		return time.Time{}, nil, "", ErrRejected
	}
	proposalHash := ProposalHash(proposal)
	if err := gateway.requireCompiledProposal(ctx, proposal, proposalHash); err != nil {
		return time.Time{}, nil, "", err
	}
	return now, adapter, proposalHash, nil
}

func (gateway *Gateway) probePreflight(
	ctx context.Context,
	proposal Proposal,
) (Adapter, string, error) {
	if err := proposal.Validate(); err != nil {
		return nil, "", err
	}
	now := gateway.now()
	if now.IsZero() || now.Location() != time.UTC {
		return nil, "", fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	adapter, exists := gateway.adapters[proposal.Provider]
	if !exists {
		return nil, "", ErrRejected
	}
	proposalHash := ProposalHash(proposal)
	if err := gateway.requireCompiledProposal(ctx, proposal, proposalHash); err != nil {
		return nil, "", err
	}
	return adapter, proposalHash, nil
}

func (gateway *Gateway) prepare(
	ctx context.Context,
	proposal Proposal,
	proposalHash string,
	now time.Time,
) (Result, bool, error) {
	sealedProposal, err := gateway.vault.SealRecord(vault.AD{
		User: gateway.tenantID, Store: "workforce.effect.proposal",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, proposal.Input)
	if err != nil {
		return Result{}, false, fmt.Errorf("%w: seal proposal: %v", ErrUncertain, err)
	}
	// The exact idempotency advisory lock serializes conflicting prepares.
	// Read committed lets independent proposals prepare concurrently without
	// PostgreSQL predicate-serialization aborts.
	tx, err := gateway.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Result{}, false, fmt.Errorf("%w: begin prepare: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		gateway.tenantID+"|"+string(proposal.OrganizationID)+"|"+proposal.Provider+"|"+proposal.IdempotencyKey); err != nil {
		return Result{}, false, fmt.Errorf("%w: lock idempotency: %v", ErrUncertain, err)
	}
	command, err := tx.Exec(ctx, `
			INSERT INTO workforce_effect_operations (
				tenant_id,organization_id,proposal_id,intent_id,seat_id,lease_id,fence,
				provider,operation,idempotency_key,proposal_hash,skill_digest,
				operation_digest,state,created_at,updated_at,skill_id,graph_node_id,effect_class,
				irreversible,deadline,proposal_sealed,compiled_proposal_hash
				,approval_id,approval_cost_microunits
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'prepared',$14,$14,
				$15,$16,$17,$18,$19,$20,$11,NULLIF($21,''),$22)
			ON CONFLICT DO NOTHING
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID, proposal.IntentID,
		proposal.SeatID, proposal.LeaseID, proposal.Fence, proposal.Provider,
		proposal.Operation, proposal.IdempotencyKey, proposalHash,
		proposal.SkillDigest.Digest, proposal.OperationDigest.Digest, now,
		proposal.SkillID, proposal.NodeID, proposal.EffectClass, proposal.Irreversible,
		proposal.Deadline, sealedProposal, proposal.ApprovalID, proposal.ApprovalCost)
	if err != nil {
		return Result{}, false, fmt.Errorf("%w: insert prepare: %v", ErrUncertain, err)
	}
	if command.RowsAffected() == 1 {
		if proposal.Irreversible {
			if err := gateway.consumeApproval(
				ctx, tx, proposal, now,
			); err != nil {
				return Result{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, false, fmt.Errorf("%w: commit prepare: %v", ErrUncertain, err)
		}
		return Result{ProposalID: proposal.ID, State: StatePrepared}, false, nil
	}
	var result Result
	var storedHash string
	err = tx.QueryRow(ctx, `
		SELECT proposal_id,state,COALESCE(external_id,''),COALESCE(evidence_hash,''),
			COALESCE(safe_error_code,''),proposal_hash
		FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2
		  AND (proposal_id=$3 OR (provider=$4 AND idempotency_key=$5))
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID,
		proposal.Provider, proposal.IdempotencyKey).Scan(
		&result.ProposalID, &result.State, &result.ExternalID,
		&result.EvidenceHash.Digest, &result.SafeErrorCode, &storedHash,
	)
	if err != nil {
		return Result{}, false, fmt.Errorf("%w: inspect idempotency: %v", ErrUncertain, err)
	}
	if storedHash != proposalHash || result.ProposalID != proposal.ID {
		return Result{}, false, ErrConflict
	}
	if result.EvidenceHash.Digest != "" {
		result.EvidenceHash.Algorithm = "sha256"
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, false, fmt.Errorf("%w: commit idempotency read: %v", ErrUncertain, err)
	}
	return result, true, nil
}

func (gateway *Gateway) setState(
	ctx context.Context,
	proposal Proposal,
	state State,
	externalID, safeCode string,
) error {
	if !state.Valid() {
		return fmt.Errorf("invalid effect state %q", state)
	}
	now := gateway.now()
	command, err := gateway.pool.Exec(ctx, `
		UPDATE workforce_effect_operations
		SET state=$1,external_id=NULLIF($2,''),safe_error_code=NULLIF($3,''),updated_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND proposal_id=$7
		  AND state IN ('prepared','dispatching','externally_ambiguous')
	`, state, externalID, safeCode, now, gateway.tenantID,
		proposal.OrganizationID, proposal.ID)
	if err != nil {
		return fmt.Errorf("%w: update effect state: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// beginDispatch is the single authorization-spend point. It serializes the
// prepared effect with runtime-lease cancellation, signed-authority
// invalidation, and approval revocation, revalidates all three while their
// durable rows are locked, and only then makes the dispatching transition.
func (gateway *Gateway) beginDispatch(
	ctx context.Context,
	proposal Proposal,
	preflightTime time.Time,
) error {
	tx, err := gateway.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("%w: begin dispatch authorization: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var effectState State
	if err := tx.QueryRow(ctx, `
		SELECT state
		FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
		FOR UPDATE
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID).Scan(&effectState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRejected
		}
		return fmt.Errorf("%w: lock prepared effect: %v", ErrUncertain, err)
	}
	if effectState != StatePrepared {
		return ErrConflict
	}

	var runtimeState string
	var runtimeSeat contracts.SeatID
	var runtimeFence contracts.FenceToken
	var runtimeExpiry time.Time
	if err := tx.QueryRow(ctx, `
		SELECT state,seat_id,fence,expires_at
		FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
		FOR UPDATE
	`, gateway.tenantID, proposal.OrganizationID, proposal.LeaseID).Scan(
		&runtimeState, &runtimeSeat, &runtimeFence, &runtimeExpiry,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRejected
		}
		return fmt.Errorf("%w: lock runtime lease: %v", ErrUncertain, err)
	}

	dispatchTime := gateway.now()
	if dispatchTime.IsZero() || dispatchTime.Location() != time.UTC ||
		dispatchTime.Before(preflightTime) || !proposal.Deadline.After(dispatchTime) ||
		runtimeState != string(lease.StateActive) ||
		runtimeSeat != proposal.SeatID || runtimeFence != proposal.Fence ||
		!runtimeExpiry.After(dispatchTime) {
		return ErrRejected
	}

	var authoritySeat contracts.SeatID
	var authorityExpiry time.Time
	if err := tx.QueryRow(ctx, `
		SELECT authority.seat_id,authority.expires_at
		FROM workforce_authority_leases authority
		WHERE authority.tenant_id=$1 AND authority.organization_id=$2
		  AND authority.lease_id=$3
		  AND NOT EXISTS (
			SELECT 1
			FROM workforce_authority_lease_invalidations invalidation
			WHERE invalidation.tenant_id=authority.tenant_id
			  AND invalidation.organization_id=authority.organization_id
			  AND invalidation.lease_id=authority.lease_id
		  )
		FOR SHARE OF authority
	`, gateway.tenantID, proposal.OrganizationID, proposal.LeaseID).Scan(
		&authoritySeat, &authorityExpiry,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRejected
		}
		return fmt.Errorf("%w: lock signed lease authority: %v", ErrUncertain, err)
	}
	if authoritySeat != proposal.SeatID || !authorityExpiry.After(dispatchTime) {
		return ErrRejected
	}
	if err := gateway.reverifyPreparedApproval(
		ctx, tx, proposal, dispatchTime,
	); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_effect_operations
		SET state='dispatching',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND proposal_id=$4
		  AND state='prepared'
	`, dispatchTime, gateway.tenantID, proposal.OrganizationID, proposal.ID)
	if err != nil {
		return fmt.Errorf("%w: mark effect dispatching: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit dispatch authorization: %v", ErrUncertain, err)
	}
	return nil
}

func (gateway *Gateway) setProbe(
	ctx context.Context,
	proposal Proposal,
	outcome skills.ProbeOutcome,
	externalID, safeCode string,
) error {
	if !outcome.Valid() {
		return fmt.Errorf("invalid probe outcome %q", outcome)
	}
	now := gateway.now()
	command, err := gateway.pool.Exec(ctx, `
		UPDATE workforce_effect_operations
		SET state='externally_ambiguous',external_id=COALESCE(NULLIF($1,''),external_id),
			safe_error_code=$2,last_probe_outcome=$3,last_probe_at=$4,updated_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND proposal_id=$7
		  AND state IN ('dispatching','externally_ambiguous')
	`, externalID, safeCode, outcome, now, gateway.tenantID,
		proposal.OrganizationID, proposal.ID)
	if err != nil {
		return fmt.Errorf("%w: persist probe: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (gateway *Gateway) setProbeOutcome(
	ctx context.Context,
	proposal Proposal,
	outcome skills.ProbeOutcome,
) error {
	if !outcome.Valid() {
		return fmt.Errorf("invalid probe outcome %q", outcome)
	}
	command, err := gateway.pool.Exec(ctx, `
		UPDATE workforce_effect_operations
		SET last_probe_outcome=$1,last_probe_at=$2,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND proposal_id=$5
		  AND state='succeeded'
	`, outcome, gateway.now(), gateway.tenantID, proposal.OrganizationID, proposal.ID)
	if err != nil {
		return fmt.Errorf("%w: persist probe outcome: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (gateway *Gateway) commitEvidence(
	ctx context.Context,
	proposal Proposal,
	observation DispatchResult,
) (Result, error) {
	if !observation.Started || strings.TrimSpace(observation.ExternalID) == "" ||
		len(observation.Observation) == 0 || len(observation.Observation) > 256<<10 ||
		observation.ObservedAt.IsZero() || observation.ObservedAt.Location() != time.UTC {
		_ = gateway.setState(ctx, proposal, StateExternallyAmbiguous,
			observation.ExternalID, "invalid_observation")
		return Result{}, ErrAmbiguous
	}
	sum := sha256.Sum256(observation.Observation)
	evidenceHash := hex.EncodeToString(sum[:])
	sealed, err := gateway.vault.SealRecord(vault.AD{
		User: gateway.tenantID, Store: "workforce.effect.evidence",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, observation.Observation)
	if err != nil {
		return Result{}, fmt.Errorf("%w: seal evidence: %v", ErrUncertain, err)
	}
	// Evidence is inserted and its exact proposal row is transitioned in one
	// transaction. The primary keys and state predicate provide the needed
	// exclusion; read committed also lets independent proposals complete
	// concurrently without PostgreSQL predicate-serialization aborts.
	tx, err := gateway.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Result{}, fmt.Errorf("%w: begin evidence: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_effect_evidence (
			tenant_id,organization_id,proposal_id,evidence_hash,
			external_id,observation,observed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID, evidenceHash,
		observation.ExternalID, sealed, observation.ObservedAt); err != nil {
		return Result{}, fmt.Errorf("%w: insert evidence: %v", ErrUncertain, err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_effect_operations
		SET state='succeeded',external_id=$1,evidence_hash=$2,
			safe_error_code=NULL,updated_at=$3
		WHERE tenant_id=$4 AND organization_id=$5 AND proposal_id=$6
		  AND state IN ('dispatching','externally_ambiguous')
	`, observation.ExternalID, evidenceHash, observation.ObservedAt,
		gateway.tenantID, proposal.OrganizationID, proposal.ID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: commit observation: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return Result{}, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("%w: commit evidence: %v", ErrUncertain, err)
	}
	return Result{
		ProposalID: proposal.ID, State: StateSucceeded,
		ExternalID:   observation.ExternalID,
		EvidenceHash: contracts.ContentHash{Algorithm: "sha256", Digest: evidenceHash},
		ObservedAt:   observation.ObservedAt,
	}, nil
}

type storedResult struct {
	result       Result
	proposalHash string
}

// LoadResult returns the durable projection for one exact compiled proposal
// without dispatching, probing, or requiring still-live execution authority.
func (gateway *Gateway) LoadResult(
	ctx context.Context,
	proposal Proposal,
) (Result, error) {
	if err := proposal.Validate(); err != nil {
		return Result{}, err
	}
	stored, found, err := gateway.lookup(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	if !found || stored.proposalHash != ProposalHash(proposal) {
		return Result{}, ErrConflict
	}
	return stored.result, nil
}

func (gateway *Gateway) lookup(
	ctx context.Context,
	proposal Proposal,
) (storedResult, bool, error) {
	var stored storedResult
	var observedAt *time.Time
	err := gateway.pool.QueryRow(ctx, `
		SELECT operation.proposal_id,operation.state,
			COALESCE(operation.external_id,''),
			COALESCE(operation.evidence_hash,''),
			evidence.observed_at,
			COALESCE(operation.safe_error_code,''),operation.proposal_hash
		FROM workforce_effect_operations operation
		LEFT JOIN workforce_effect_evidence evidence
		  ON evidence.tenant_id=operation.tenant_id
		 AND evidence.organization_id=operation.organization_id
		 AND evidence.proposal_id=operation.proposal_id
		WHERE operation.tenant_id=$1 AND operation.organization_id=$2
		  AND operation.proposal_id=$3
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID).Scan(
		&stored.result.ProposalID, &stored.result.State, &stored.result.ExternalID,
		&stored.result.EvidenceHash.Digest, &observedAt,
		&stored.result.SafeErrorCode, &stored.proposalHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedResult{}, false, nil
	}
	if err != nil {
		return storedResult{}, false, fmt.Errorf("%w: lookup effect: %v", ErrUncertain, err)
	}
	if stored.result.EvidenceHash.Digest != "" {
		stored.result.EvidenceHash.Algorithm = "sha256"
	}
	if observedAt != nil {
		stored.result.ObservedAt = observedAt.UTC()
	}
	return stored, true, nil
}

// ProposalHash binds every dispatch-relevant proposal field to one compiled
// plan authorization.
func ProposalHash(proposal Proposal) string {
	canonical := make([]byte, 0, len(proposal.Input)+512)
	for _, part := range [][]byte{
		[]byte(proposal.ID), []byte(proposal.OrganizationID), []byte(proposal.IntentID),
		[]byte(proposal.NodeID),
		[]byte(proposal.SeatID), []byte(proposal.LeaseID),
		[]byte(strconv.FormatUint(uint64(proposal.Fence), 10)),
		[]byte(proposal.Provider), []byte(proposal.Operation),
		[]byte(proposal.SkillID), []byte(proposal.EffectClass),
		[]byte(strconv.FormatBool(proposal.Irreversible)),
		[]byte(proposal.IdempotencyKey), []byte(proposal.SkillDigest.Algorithm),
		[]byte(proposal.SkillDigest.Digest), []byte(proposal.OperationDigest.Algorithm),
		[]byte(proposal.OperationDigest.Digest), []byte(proposal.ApprovalID),
		[]byte(strconv.FormatUint(proposal.ApprovalCost, 10)), proposal.Input,
		[]byte(proposal.Deadline.Format(time.RFC3339Nano)),
	} {
		canonical = binary.BigEndian.AppendUint64(canonical, uint64(len(part)))
		canonical = append(canonical, part...)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// reverifyPreparedApproval re-establishes owner authority for an irreversible
// effect that was prepared by an earlier attempt and is about to be dispatched
// now. It re-checks the signed batch, its revocation and expiry, and this
// proposal's exact recorded consumption. It never charges again: the
// consumption row written at preparation is the authority being confirmed.
func (gateway *Gateway) reverifyPreparedApproval(
	ctx context.Context,
	tx pgx.Tx,
	proposal Proposal,
	now time.Time,
) error {
	if !proposal.Irreversible {
		return nil
	}
	var ceiling uint64
	var expiresAt time.Time
	var revokedAt *time.Time
	var canonicalHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT aggregate_ceiling_microunits,expires_at,revoked_at,
			canonical_hash,sealed_batch
		FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		FOR UPDATE
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID).Scan(
		&ceiling, &expiresAt, &revokedAt, &canonicalHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) || revokedAt != nil ||
		err == nil && !expiresAt.After(now) {
		return ErrRejected
	}
	if err != nil {
		return fmt.Errorf("%w: reload effect approval: %v", ErrUncertain, err)
	}
	batch, err := approval.OpenSealedBatch(
		gateway.vault, gateway.tenantID, proposal.OrganizationID,
		proposal.ApprovalID, sealed, canonicalHash, gateway.authority,
	)
	if err != nil {
		return ErrRejected
	}
	if !batch.Authorizes(proposal.IntentID) || !batch.ExpiresAt.After(now) ||
		!batch.ExpiresAt.Equal(expiresAt) ||
		batch.AggregateCeilingMicrounits != ceiling {
		return ErrRejected
	}
	var recorded uint64
	if err := tx.QueryRow(ctx, `
		SELECT cost_microunits FROM workforce_approval_batch_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		  AND intent_id=$4 AND idempotency_key=$5
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID,
		proposal.IntentID,
		"effect:"+proposal.ID+":"+proposal.IdempotencyKey).Scan(&recorded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRejected
		}
		return fmt.Errorf("%w: reload effect consumption: %v", ErrUncertain, err)
	}
	if recorded != proposal.ApprovalCost {
		return ErrRejected
	}
	return nil
}

func (gateway *Gateway) consumeApproval(
	ctx context.Context,
	tx pgx.Tx,
	proposal Proposal,
	now time.Time,
) error {
	var ceiling, consumed uint64
	var expiresAt time.Time
	var revokedAt *time.Time
	var canonicalHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT aggregate_ceiling_microunits,consumed_microunits,expires_at,
			revoked_at,canonical_hash,sealed_batch
		FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		FOR UPDATE
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID).Scan(
		&ceiling, &consumed, &expiresAt, &revokedAt, &canonicalHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) || revokedAt != nil ||
		err == nil && !expiresAt.After(now) {
		return ErrRejected
	}
	if err != nil {
		return fmt.Errorf("%w: load effect approval: %v", ErrUncertain, err)
	}
	// Spending authority comes from the owner-signed sealed batch, never from
	// the projection columns beside it: ceiling, expiry, and intent membership
	// are exactly what a database-write compromise would otherwise widen.
	batch, err := approval.OpenSealedBatch(
		gateway.vault, gateway.tenantID, proposal.OrganizationID,
		proposal.ApprovalID, sealed, canonicalHash, gateway.authority,
	)
	if err != nil {
		return ErrRejected
	}
	if !batch.Authorizes(proposal.IntentID) || !batch.ExpiresAt.After(now) ||
		batch.AggregateCeilingMicrounits != ceiling ||
		!batch.ExpiresAt.Equal(expiresAt) {
		return ErrRejected
	}
	// Remaining authority is measured against the append-only consumption
	// ledger, not the running counter beside it: the counter is the one piece
	// of approval state that legitimately changes, so it is the one a rollback
	// would target to buy spend the owner never granted.
	var recorded uint64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_microunits),0)
		FROM workforce_approval_batch_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID).Scan(
		&recorded,
	); err != nil {
		return fmt.Errorf("%w: measure effect approval consumption: %v", ErrUncertain, err)
	}
	if recorded != consumed {
		return ErrRejected
	}
	ceilingBatch := batch.AggregateCeilingMicrounits
	if recorded > ceilingBatch || proposal.ApprovalCost > ceilingBatch-recorded {
		return ErrRejected
	}
	key := "effect:" + proposal.ID + ":" + proposal.IdempotencyKey
	var prior uint64
	err = tx.QueryRow(ctx, `
		SELECT cost_microunits FROM workforce_approval_batch_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		  AND intent_id=$4 AND idempotency_key=$5
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID,
		proposal.IntentID, key).Scan(&prior)
	if err == nil {
		if prior != proposal.ApprovalCost {
			return ErrRejected
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: inspect effect approval consumption: %v", ErrUncertain, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_approval_batch_consumptions (
			tenant_id,organization_id,batch_id,intent_id,idempotency_key,
			cost_microunits,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, gateway.tenantID, proposal.OrganizationID, proposal.ApprovalID,
		proposal.IntentID, key, proposal.ApprovalCost, now); err != nil {
		return fmt.Errorf("%w: consume effect approval: %v", ErrUncertain, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_approval_batches
		SET consumed_microunits=consumed_microunits+$1
		WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
	`, proposal.ApprovalCost, gateway.tenantID, proposal.OrganizationID,
		proposal.ApprovalID); err != nil {
		return fmt.Errorf("%w: update effect approval ceiling: %v", ErrUncertain, err)
	}
	return nil
}

// compiledPlanEnvelope is the authenticated subset of one sealed compiled plan.
// The plan is written by the compiler, which cannot be imported here, so only
// the dispatch-authorizing fields are decoded.
type compiledPlanEnvelope struct {
	PlanHash  contracts.ContentHash `json:"plan_hash"`
	Operation Proposal              `json:"operation"`
}

// requireCompiledProposal admits a dispatch only when the durable compiled plan
// authorizes this exact proposal. The relational hash column alone is not
// sufficient authority: it is a projection, so the Vault-sealed plan is opened
// and the operation the compiler actually approved is compared field for field.
func (gateway *Gateway) requireCompiledProposal(
	ctx context.Context,
	proposal Proposal,
	proposalHash string,
) error {
	var compiled, planHash string
	var sealed []byte
	err := gateway.pool.QueryRow(ctx, `
		SELECT effect_proposal_hash,plan_hash,sealed_plan
		FROM workforce_compiled_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, gateway.tenantID, proposal.OrganizationID, proposal.ID).Scan(
		&compiled, &planHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRejected
	}
	if err != nil {
		return fmt.Errorf("%w: load compiled dispatch authority: %v", ErrUncertain, err)
	}
	if compiled != proposalHash {
		return ErrRejected
	}
	opened, err := gateway.vault.OpenRecord(vault.AD{
		User: gateway.tenantID, Store: "workforce.compiled.plan",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, sealed)
	if err != nil {
		return fmt.Errorf("%w: open compiled plan: %v", ErrUncertain, err)
	}
	var envelope compiledPlanEnvelope
	if err := json.Unmarshal(opened, &envelope); err != nil {
		return fmt.Errorf("%w: decode compiled plan: %v", ErrUncertain, err)
	}
	if envelope.PlanHash.Digest != planHash ||
		ProposalHash(envelope.Operation) != proposalHash {
		return ErrRejected
	}
	return nil
}

func (gateway *Gateway) admit(ctx context.Context, proposal Proposal) (circuit.Permit, error) {
	keys, err := circuit.Keys(proposal.OrganizationID, proposal.Provider,
		string(proposal.SkillID), string(proposal.EffectClass))
	if err != nil {
		return circuit.Permit{}, err
	}
	return gateway.breakers.Admit(ctx, keys, proposal.Irreversible)
}
