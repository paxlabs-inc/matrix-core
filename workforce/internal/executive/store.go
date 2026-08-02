package executive

import (
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
	"matrix/workforce/internal/mission"
)

// Store owns one tenant and organization's Executive policy, review,
// decision, founder escalation, incident, and authorization-consumption state.
type Store struct {
	pool              *pgxpool.Pool
	vault             *vault.UserVault
	mission           *mission.Store
	tenantID          string
	organizationID    contracts.OrganizationID
	founderKeyID      string
	founderPublicKey  ed25519.PublicKey
	controllerKeyID   string
	controllerPrivate ed25519.PrivateKey
	controllerPublic  ed25519.PublicKey
	now               func() time.Time
}

// NewStore constructs a Vault-separated Executive authority store. The caller
// owns the pool and Mission store and must keep the controller key outside all
// seat and Auditor processes.
func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	missionStore *mission.Store,
	tenantID string,
	organizationID contracts.OrganizationID,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	controllerKeyID string,
	controllerPrivate ed25519.PrivateKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || missionStore == nil || strings.TrimSpace(tenantID) == "" ||
		organizationID == "" || validateToken("founder key id", founderKeyID) != nil ||
		len(founderPublicKey) != ed25519.PublicKeySize ||
		validateToken("controller key id", controllerKeyID) != nil ||
		len(controllerPrivate) != ed25519.PrivateKeySize || now == nil {
		return nil, fmt.Errorf("executive: store dependencies and authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("executive: Vault tenant does not match store tenant")
	}
	privateCopy := append(ed25519.PrivateKey(nil), controllerPrivate...)
	controllerPublic := controllerPrivate.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, vault: userVault, mission: missionStore,
		tenantID: tenantID, organizationID: organizationID,
		founderKeyID:      founderKeyID,
		founderPublicKey:  append(ed25519.PublicKey(nil), founderPublicKey...),
		controllerKeyID:   controllerKeyID,
		controllerPrivate: privateCopy,
		controllerPublic:  append(ed25519.PublicKey(nil), controllerPublic...),
		now:               now,
	}, nil
}

// InstallPolicy verifies the live founder company root, compiles the exact
// delegation, and atomically installs an immutable policy version and compiled
// projection. Reusing the same canonical version is idempotent.
func (store *Store) InstallPolicy(
	ctx context.Context,
	delegation DelegationPolicy,
	seats []contracts.Seat,
	mandates []contracts.Mandate,
) (CompiledAuthority, bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return CompiledAuthority{}, false, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: load current company authority: %w", err)
	}
	if !current.Executable(now) || current.Authority.Mission.OrganizationID != store.organizationID {
		return CompiledAuthority{}, false, ErrAuthorityNotCurrent
	}
	compiled, err := Compile(CompileInput{
		Authority: current.Authority, Delegation: delegation,
		Seats: seats, Mandates: mandates,
		FounderKeyID: store.founderKeyID, FounderPublicKey: store.founderPublicKey,
		ControllerPublicKey: store.controllerPublic, At: now,
	})
	if err != nil {
		return CompiledAuthority{}, false, err
	}
	policyCanonical, policyHash, policySealed, err := store.prepare(
		store.policyAD(delegation.ID, delegation.Version), &delegation,
	)
	if err != nil {
		return CompiledAuthority{}, false, err
	}
	_ = policyCanonical
	compiledCanonical, compiledHash, compiledSealed, err := store.prepare(
		store.compiledAD(compiled.ID, compiled.PolicyVersion), &compiled,
	)
	if err != nil {
		return CompiledAuthority{}, false, err
	}
	_ = compiledCanonical

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: begin policy installation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.verifyMissionProjectionTx(ctx, tx, current); err != nil {
		return CompiledAuthority{}, false, err
	}
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|executive-policy|" + string(delegation.ID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: lock policy: %w", err)
	}
	var currentVersion uint64
	var currentHash string
	err = tx.QueryRow(ctx, `
		SELECT head.version,record.canonical_hash
		FROM workforce_executive_policy_heads head
		JOIN workforce_executive_policies record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.policy_id=head.policy_id AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.policy_id=$3
		FOR UPDATE OF head
	`, store.tenantID, store.organizationID, delegation.ID).Scan(&currentVersion, &currentHash)
	if err == nil {
		if delegation.Version == currentVersion && policyHash == currentHash {
			existing, state, loadErr := store.loadCurrentAuthorityTx(ctx, tx)
			if loadErr != nil {
				return CompiledAuthority{}, false, loadErr
			}
			if state != "active" {
				return CompiledAuthority{}, false, ErrConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return CompiledAuthority{}, false, fmt.Errorf("executive: commit deduplicated policy: %w", err)
			}
			return existing, true, nil
		}
		if delegation.Version != currentVersion+1 {
			return CompiledAuthority{}, false, ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CompiledAuthority{}, false, fmt.Errorf("executive: inspect policy head: %w", err)
	} else if delegation.Version != 1 {
		return CompiledAuthority{}, false, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_policies (
			tenant_id,organization_id,policy_id,version,canonical_hash,sealed_record,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, store.tenantID, store.organizationID, delegation.ID, delegation.Version,
		policyHash, policySealed, delegation.EffectiveAt, delegation.ExpiresAt, now); err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: insert policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_compiled_authorities (
			tenant_id,organization_id,compiled_authority_id,policy_id,policy_version,
			canonical_hash,sealed_record,effective_at,expires_at,compiled_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.tenantID, store.organizationID, compiled.ID, compiled.PolicyID,
		compiled.PolicyVersion, compiledHash, compiledSealed, compiled.EffectiveAt,
		compiled.ExpiresAt, compiled.CompiledAt); err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: insert compiled authority: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_policy_heads (
			tenant_id,organization_id,policy_id,version,compiled_authority_id,
			compiled_hash,state,effective_at,expires_at,revoked_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'active',$7,$8,NULL,$9)
		ON CONFLICT (tenant_id,organization_id,policy_id) DO UPDATE SET
			version=EXCLUDED.version,compiled_authority_id=EXCLUDED.compiled_authority_id,
			compiled_hash=EXCLUDED.compiled_hash,state='active',
			effective_at=EXCLUDED.effective_at,expires_at=EXCLUDED.expires_at,
			revoked_at=NULL,updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, delegation.ID, delegation.Version,
		compiled.ID, compiledHash, delegation.EffectiveAt, delegation.ExpiresAt, now); err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: advance policy head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompiledAuthority{}, false, fmt.Errorf("executive: commit policy installation: %w", err)
	}
	return compiled, false, nil
}

// RevokePolicy verifies and atomically applies a founder-signed revocation.
// Revocation blocks new decisions and consumption while preserving evidence.
func (store *Store) RevokePolicy(ctx context.Context, value PolicyRevocation) (bool, error) {
	if value.OrganizationID != store.organizationID {
		return false, ErrUnauthorized
	}
	if err := VerifyPolicyRevocation(value, store.founderKeyID, store.founderPublicKey); err != nil {
		return false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.RevokedAt.After(now) {
		return false, fmt.Errorf("executive: future policy revocation is invalid")
	}
	_, hash, sealed, err := store.prepare(store.revocationAD(value.ID), &value)
	if err != nil {
		return false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("executive: begin policy revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|executive-policy|" + string(value.PolicyID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return false, fmt.Errorf("executive: lock policy revocation: %w", err)
	}
	var currentVersion uint64
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT version,state FROM workforce_executive_policy_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND policy_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, value.PolicyID).Scan(&currentVersion, &state); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("executive: load policy for revocation: %w", err)
	}
	if currentVersion != value.PolicyVersion {
		return false, ErrConflict
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_executive_policy_revocations
		WHERE tenant_id=$1 AND organization_id=$2 AND revocation_id=$3
	`, store.tenantID, store.organizationID, value.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("executive: commit deduplicated revocation: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("executive: inspect policy revocation: %w", err)
	}
	if state == "revoked" {
		return false, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_policy_revocations (
			tenant_id,organization_id,revocation_id,policy_id,policy_version,
			canonical_hash,sealed_record,revoked_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, store.tenantID, store.organizationID, value.ID, value.PolicyID,
		value.PolicyVersion, hash, sealed, value.RevokedAt, now); err != nil {
		return false, fmt.Errorf("executive: insert policy revocation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_executive_policy_heads
		SET state='revoked',revoked_at=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND policy_id=$5 AND version=$6
	`, value.RevokedAt, now, store.tenantID, store.organizationID,
		value.PolicyID, value.PolicyVersion); err != nil {
		return false, fmt.Errorf("executive: apply policy revocation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("executive: commit policy revocation: %w", err)
	}
	return false, nil
}

// DecideCommand carries one immutable signed request, its fresh reviews, and
// caller-chosen idempotency identity. Decision IDs are derived by the store.
type DecideCommand struct {
	Request        DecisionRequest
	Reviews        []Review
	IdempotencyKey string
}

// DecisionResult returns the durable decision plus optional founder escalation
// and incident records produced atomically with it.
type DecisionResult struct {
	Decision       Decision
	FounderRequest *FounderDecisionRequest
	Incident       *DecisionIncident
	Deduplicated   bool
}

// Decide verifies current founder roots, seat signatures, exact clauses,
// rolling limits, review independence, and split/downgrade resistance before
// atomically recording one signed decision.
func (store *Store) Decide(ctx context.Context, command DecideCommand) (DecisionResult, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 255 ||
		command.Request.OrganizationID != store.organizationID {
		return DecisionResult{}, ErrUnauthorized
	}
	if err := command.Request.Validate(); err != nil {
		return DecisionResult{}, err
	}
	requestCanonical, err := contracts.EncodeCanonical(&command.Request)
	if err != nil {
		return DecisionResult{}, err
	}
	requestHash := hashBytes(requestCanonical)
	now, err := store.currentTime()
	if err != nil {
		return DecisionResult{}, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("executive: load current company authority: %w", err)
	}
	if !current.Executable(now) || current.Authority.Mission.OrganizationID != store.organizationID {
		return DecisionResult{}, ErrAuthorityNotCurrent
	}
	aggregationKey := semanticAggregationKey(command.Request)
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DecisionResult{}, fmt.Errorf("executive: begin decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.verifyMissionProjectionTx(ctx, tx, current); err != nil {
		return DecisionResult{}, err
	}
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|executive-decision|" + aggregationKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return DecisionResult{}, fmt.Errorf("executive: lock decision scope: %w", err)
	}
	if existing, found, err := store.findIdempotentDecisionTx(
		ctx, tx, command.IdempotencyKey, command.Request.ID, requestHash.Digest,
	); err != nil {
		return DecisionResult{}, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return DecisionResult{}, fmt.Errorf("executive: commit deduplicated decision: %w", err)
		}
		existing.Deduplicated = true
		return existing, nil
	}
	authority, state, err := store.loadCurrentAuthorityTx(ctx, tx)
	if err != nil {
		return DecisionResult{}, err
	}
	if err := compiledMatchesMission(authority, current.Authority); err != nil {
		return DecisionResult{}, err
	}
	clause, clauseFound := findClause(authority.Clauses, command.Request.ClauseID)
	clauseWindow := authority.AggregationWindowSeconds
	if clauseFound {
		clauseWindow = clause.AggregationWindowSeconds
	}
	scopeCapital, scopeExposure, err := store.rollingUseTx(
		ctx, tx, aggregationKey, now.Add(-time.Duration(clauseWindow)*time.Second), true,
	)
	if err != nil {
		return DecisionResult{}, err
	}
	globalCapital, globalExposure, err := store.rollingUseTx(
		ctx, tx, "", now.Add(-time.Duration(authority.AggregationWindowSeconds)*time.Second), false,
	)
	if err != nil {
		return DecisionResult{}, err
	}
	evaluated, err := evaluate(authority, command.Request, command.Reviews, EvaluationContext{
		At:                              now,
		ScopeRollingCapitalMicrounits:   scopeCapital,
		ScopeRollingExposureMicrounits:  scopeExposure,
		GlobalRollingCapitalMicrounits:  globalCapital,
		GlobalRollingExposureMicrounits: globalExposure,
		PolicyRevoked:                   state == "revoked",
	})
	if err != nil {
		return DecisionResult{}, err
	}
	result, records, err := store.buildDecisionRecords(
		command.Request, command.Reviews, authority, requestHash, evaluated, now,
	)
	if err != nil {
		return DecisionResult{}, err
	}
	requestSealed, err := store.vault.SealRecord(store.requestAD(command.Request.ID), requestCanonical)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("executive: seal decision request: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_decision_requests (
			tenant_id,organization_id,request_id,policy_id,policy_version,clause_id,
			action,initiative_id,aggregation_key,action_family,capital_microunits,
			exposure_microunits,canonical_hash,sealed_record,idempotency_key,
			created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`, store.tenantID, store.organizationID, command.Request.ID, command.Request.PolicyID,
		command.Request.PolicyVersion, command.Request.ClauseID, command.Request.Action,
		command.Request.InitiativeID, aggregationKey, command.Request.Action.family(),
		command.Request.CapitalMicrounits, command.Request.ExposureMicrounits,
		requestHash.Digest, requestSealed, command.IdempotencyKey,
		command.Request.CreatedAt, command.Request.ExpiresAt); err != nil {
		return DecisionResult{}, fmt.Errorf("executive: insert decision request: %w", err)
	}
	for _, prepared := range records.reviews {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_executive_reviews (
				tenant_id,organization_id,review_id,request_id,kind,outcome,
				reviewer_seat_id,canonical_hash,sealed_record,reviewed_at,expires_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, store.tenantID, store.organizationID, prepared.value.ID,
			prepared.value.RequestID, prepared.value.Kind, prepared.value.Outcome,
			prepared.value.ReviewerSeatID, prepared.hash, prepared.sealed,
			prepared.value.ReviewedAt, prepared.value.ExpiresAt); err != nil {
			return DecisionResult{}, fmt.Errorf("executive: insert independent review: %w", err)
		}
	}
	if records.founder != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_founder_decision_requests (
				tenant_id,organization_id,founder_request_id,request_id,reserved_kind,
				canonical_hash,sealed_record,created_at,expires_at,state
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending')
		`, store.tenantID, store.organizationID, records.founder.value.ID,
			records.founder.value.RequestID, records.founder.value.ReservedKind,
			records.founder.hash, records.founder.sealed,
			records.founder.value.CreatedAt, records.founder.value.ExpiresAt); err != nil {
			return DecisionResult{}, fmt.Errorf("executive: insert founder request: %w", err)
		}
	}
	if records.incident != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_executive_decision_incidents (
				tenant_id,organization_id,incident_id,request_id,kind,
				canonical_hash,sealed_record,state,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8)
		`, store.tenantID, store.organizationID, records.incident.value.ID,
			records.incident.value.RequestID, records.incident.value.Kind,
			records.incident.hash, records.incident.sealed,
			records.incident.value.CreatedAt); err != nil {
			return DecisionResult{}, fmt.Errorf("executive: insert decision incident: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_decisions (
			tenant_id,organization_id,decision_id,request_id,outcome,action,
			policy_id,policy_version,clause_id,capital_microunits,exposure_microunits,
			rolling_capital_microunits,rolling_exposure_microunits,
			canonical_hash,sealed_record,created_at,authorized_until,next_review_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
	`, store.tenantID, store.organizationID, records.decision.value.ID,
		records.decision.value.RequestID, records.decision.value.Outcome,
		records.decision.value.Action, records.decision.value.PolicyID,
		records.decision.value.PolicyVersion, records.decision.value.ClauseID,
		records.decision.value.CapitalMicrounits, records.decision.value.ExposureMicrounits,
		records.decision.value.RollingCapitalMicrounits,
		records.decision.value.RollingExposureMicrounits,
		records.decision.hash, records.decision.sealed,
		records.decision.value.CreatedAt, records.decision.value.AuthorizedUntil,
		records.decision.value.NextReviewAt); err != nil {
		return DecisionResult{}, fmt.Errorf("executive: insert decision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DecisionResult{}, fmt.Errorf("executive: commit decision: %w", err)
	}
	return result, nil
}

// LoadCurrentAuthority authenticates and returns the current compiled policy.
// Expired or revoked heads fail closed.
func (store *Store) LoadCurrentAuthority(ctx context.Context) (CompiledAuthority, error) {
	now, err := store.currentTime()
	if err != nil {
		return CompiledAuthority{}, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: load current company authority: %w", err)
	}
	if !current.Executable(now) {
		return CompiledAuthority{}, ErrAuthorityNotCurrent
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: begin current-authority read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.verifyMissionProjectionTx(ctx, tx, current); err != nil {
		return CompiledAuthority{}, err
	}
	compiled, state, err := store.loadCurrentAuthorityTx(ctx, tx)
	if err != nil {
		return CompiledAuthority{}, err
	}
	if state != "active" || now.Before(compiled.EffectiveAt) || !now.Before(compiled.ExpiresAt) {
		return CompiledAuthority{}, ErrAuthorityNotCurrent
	}
	if err := compiledMatchesMission(compiled, current.Authority); err != nil {
		return CompiledAuthority{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompiledAuthority{}, fmt.Errorf("executive: commit current-authority read: %w", err)
	}
	return compiled, nil
}

// LoadDecision authenticates one immutable controller-signed decision.
func (store *Store) LoadDecision(ctx context.Context, id DecisionID) (Decision, error) {
	return store.loadDecisionRecord(ctx, store.pool, id)
}

// LoadFounderDecisionRequest authenticates one immutable founder escalation.
func (store *Store) LoadFounderDecisionRequest(
	ctx context.Context,
	id FounderRequestID,
) (FounderDecisionRequest, error) {
	return store.loadFounderRequestRecord(ctx, store.pool, id)
}

// LoadDecisionIncident authenticates one immutable founder-visible incident.
func (store *Store) LoadDecisionIncident(ctx context.Context, id IncidentID) (DecisionIncident, error) {
	return store.loadIncidentRecord(ctx, store.pool, id)
}

// ConsumeDecision atomically binds an unexpired authorized decision to its
// exact downstream operation and one effect identity. A decision is one-use;
// emergency pauses and non-authorizing outcomes cannot be consumed as effects.
func (store *Store) ConsumeDecision(
	ctx context.Context,
	decisionID DecisionID,
	operation OperationBinding,
	effectID string,
) (DecisionConsumption, bool, error) {
	if validateToken("decision id", string(decisionID)) != nil ||
		validateToken("effect id", effectID) != nil {
		return DecisionConsumption{}, false, ErrUnauthorized
	}
	if err := operation.Validate(); err != nil || operation.Class != OperationBoundedCompanyDecision {
		return DecisionConsumption{}, false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return DecisionConsumption{}, false, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return DecisionConsumption{}, false, fmt.Errorf("executive: load current company authority: %w", err)
	}
	if !current.Executable(now) {
		return DecisionConsumption{}, false, ErrAuthorityNotCurrent
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DecisionConsumption{}, false, fmt.Errorf("executive: begin decision consumption: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.verifyMissionProjectionTx(ctx, tx, current); err != nil {
		return DecisionConsumption{}, false, err
	}
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|executive-consume|" + string(decisionID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return DecisionConsumption{}, false, fmt.Errorf("executive: lock decision consumption: %w", err)
	}
	authority, state, err := store.loadCurrentAuthorityTx(ctx, tx)
	if err != nil {
		return DecisionConsumption{}, false, err
	}
	if state != "active" || compiledMatchesMission(authority, current.Authority) != nil {
		return DecisionConsumption{}, false, ErrAuthorityNotCurrent
	}
	decision, err := store.loadDecisionRecord(ctx, tx, decisionID)
	if err != nil {
		return DecisionConsumption{}, false, err
	}
	if decision.Outcome != DecisionAuthorized || decision.PolicyID != authority.PolicyID ||
		decision.PolicyVersion != authority.PolicyVersion || decision.Operation != operation ||
		!now.Before(decision.AuthorizedUntil) {
		return DecisionConsumption{}, false, ErrAuthorityNotCurrent
	}
	var existingID string
	err = tx.QueryRow(ctx, `
		SELECT consumption_id FROM workforce_executive_decision_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND decision_id=$3
	`, store.tenantID, store.organizationID, decisionID).Scan(&existingID)
	if err == nil {
		existing, loadErr := store.loadConsumptionRecord(ctx, tx, ConsumptionID(existingID))
		if loadErr != nil {
			return DecisionConsumption{}, false, loadErr
		}
		if existing.Operation != operation || existing.EffectID != effectID {
			return DecisionConsumption{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return DecisionConsumption{}, false, fmt.Errorf("executive: commit deduplicated consumption: %w", err)
		}
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DecisionConsumption{}, false, fmt.Errorf("executive: inspect decision consumption: %w", err)
	}
	identityHash := sha256.Sum256([]byte(string(decisionID) + "\x00" + operation.Hash.Digest + "\x00" + effectID))
	value := DecisionConsumption{
		SchemaVersion:  ConsumptionSchemaVersion,
		ID:             ConsumptionID("consumption:" + hex.EncodeToString(identityHash[:20])),
		OrganizationID: store.organizationID,
		DecisionID:     decisionID, Operation: operation, EffectID: effectID, ConsumedAt: now,
	}
	if err := signConsumption(&value, store.controllerKeyID, store.controllerPrivate); err != nil {
		return DecisionConsumption{}, false, err
	}
	_, hash, sealed, err := store.prepare(store.consumptionAD(value.ID), &value)
	if err != nil {
		return DecisionConsumption{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_executive_decision_consumptions (
			tenant_id,organization_id,consumption_id,decision_id,operation_hash,
			effect_id,canonical_hash,sealed_record,consumed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, store.tenantID, store.organizationID, value.ID, value.DecisionID,
		value.Operation.Hash.Digest, value.EffectID, hash, sealed, value.ConsumedAt); err != nil {
		return DecisionConsumption{}, false, fmt.Errorf("executive: insert decision consumption: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DecisionConsumption{}, false, fmt.Errorf("executive: commit decision consumption: %w", err)
	}
	return value, false, nil
}

type preparedReview struct {
	value  Review
	hash   string
	sealed []byte
}

type preparedFounderRequest struct {
	value  FounderDecisionRequest
	hash   string
	sealed []byte
}

type preparedIncident struct {
	value  DecisionIncident
	hash   string
	sealed []byte
}

type preparedDecision struct {
	value  Decision
	hash   string
	sealed []byte
}

type preparedDecisionRecords struct {
	reviews  []preparedReview
	founder  *preparedFounderRequest
	incident *preparedIncident
	decision preparedDecision
}

func (store *Store) buildDecisionRecords(
	request DecisionRequest,
	reviews []Review,
	authority CompiledAuthority,
	requestHash contracts.ContentHash,
	evaluated evaluation,
	now time.Time,
) (DecisionResult, preparedDecisionRecords, error) {
	identity := requestHash.Digest[:40]
	decisionID := DecisionID("decision:" + identity)
	var founderValue *FounderDecisionRequest
	var incidentValue *DecisionIncident
	var founderID *FounderRequestID
	var incidentID *IncidentID
	reasons := canonicalReasons(evaluated.reasons)
	if len(reasons) == 0 {
		return DecisionResult{}, preparedDecisionRecords{}, fmt.Errorf("executive: evaluation produced no safe reason")
	}
	authorizedUntil := minTime(request.ExpiresAt, authority.ExpiresAt)
	if evaluated.outcome == DecisionFounderRequired {
		id := FounderRequestID("founder-request:" + identity)
		founderID = &id
		value := FounderDecisionRequest{
			SchemaVersion: FounderRequestSchemaVersion,
			ID:            id, OrganizationID: store.organizationID,
			RequestID: request.ID, RequestHash: requestHash,
			InitiativeID: request.InitiativeID, Action: request.Action,
			Operation: request.Operation, ReservedKind: evaluated.reservedKind,
			PolicyID: request.PolicyID, PolicyVersion: request.PolicyVersion,
			ClauseID: request.ClauseID, Reasons: reasons,
			Evidence:           slices.Clone(request.Evidence),
			CapitalMicrounits:  request.CapitalMicrounits,
			ExposureMicrounits: request.ExposureMicrounits,
			CreatedAt:          now, ExpiresAt: authorizedUntil,
		}
		if err := signFounderRequest(&value, store.controllerKeyID, store.controllerPrivate); err != nil {
			return DecisionResult{}, preparedDecisionRecords{}, err
		}
		founderValue = &value
	}
	if evaluated.incidentKind.Valid() {
		id := IncidentID("incident:" + identity)
		incidentID = &id
		reviewIDs := make([]ReviewID, len(evaluated.reviewBindings))
		for index := range evaluated.reviewBindings {
			reviewIDs[index] = evaluated.reviewBindings[index].ID
		}
		slices.SortFunc(reviewIDs, func(left, right ReviewID) int {
			return stringsCompare(string(left), string(right))
		})
		value := DecisionIncident{
			SchemaVersion: IncidentSchemaVersion,
			ID:            id, OrganizationID: store.organizationID, RequestID: request.ID,
			Kind: evaluated.incidentKind, ReviewIDs: reviewIDs, Reasons: reasons, CreatedAt: now,
		}
		if err := signIncident(&value, store.controllerKeyID, store.controllerPrivate); err != nil {
			return DecisionResult{}, preparedDecisionRecords{}, err
		}
		incidentValue = &value
	}
	decision := Decision{
		SchemaVersion: DecisionSchemaVersion,
		ID:            decisionID, OrganizationID: store.organizationID,
		RequestID: request.ID, RequestHash: requestHash,
		InitiativeID: request.InitiativeID, Action: request.Action,
		LifecycleState: request.LifecycleState, Operation: request.Operation,
		Outcome: evaluated.outcome, ReasonCode: evaluated.reasonCode,
		Reason: strings.Join(reasons, "; "), PolicyID: request.PolicyID,
		PolicyVersion: request.PolicyVersion, PolicyHash: request.PolicyHash,
		ClauseID: request.ClauseID, Reviews: slices.Clone(evaluated.reviewBindings),
		CapitalMicrounits:         request.CapitalMicrounits,
		ExposureMicrounits:        request.ExposureMicrounits,
		RollingCapitalMicrounits:  evaluated.rollingCapital,
		RollingExposureMicrounits: evaluated.rollingExposure,
		FounderRequestID:          founderID, IncidentID: incidentID,
		CreatedAt: now, AuthorizedUntil: authorizedUntil, NextReviewAt: request.NextReviewAt,
	}
	if err := signDecision(&decision, store.controllerKeyID, store.controllerPrivate); err != nil {
		return DecisionResult{}, preparedDecisionRecords{}, err
	}
	prepared := preparedDecisionRecords{}
	usedReviews := make(map[ReviewID]Review, len(reviews))
	for _, review := range reviews {
		usedReviews[review.ID] = review
	}
	for _, binding := range evaluated.reviewBindings {
		review, exists := usedReviews[binding.ID]
		if !exists {
			return DecisionResult{}, preparedDecisionRecords{}, ErrIntegrity
		}
		_, hash, sealed, err := store.prepare(store.reviewAD(review.ID), &review)
		if err != nil {
			return DecisionResult{}, preparedDecisionRecords{}, err
		}
		if hash != binding.Hash.Digest {
			return DecisionResult{}, preparedDecisionRecords{}, ErrIntegrity
		}
		prepared.reviews = append(prepared.reviews, preparedReview{value: review, hash: hash, sealed: sealed})
	}
	if founderValue != nil {
		_, hash, sealed, err := store.prepare(store.founderRequestAD(founderValue.ID), founderValue)
		if err != nil {
			return DecisionResult{}, preparedDecisionRecords{}, err
		}
		prepared.founder = &preparedFounderRequest{value: *founderValue, hash: hash, sealed: sealed}
	}
	if incidentValue != nil {
		_, hash, sealed, err := store.prepare(store.incidentAD(incidentValue.ID), incidentValue)
		if err != nil {
			return DecisionResult{}, preparedDecisionRecords{}, err
		}
		prepared.incident = &preparedIncident{value: *incidentValue, hash: hash, sealed: sealed}
	}
	_, hash, sealed, err := store.prepare(store.decisionAD(decision.ID), &decision)
	if err != nil {
		return DecisionResult{}, preparedDecisionRecords{}, err
	}
	prepared.decision = preparedDecision{value: decision, hash: hash, sealed: sealed}
	return DecisionResult{
		Decision: decision, FounderRequest: founderValue, Incident: incidentValue,
	}, prepared, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) findIdempotentDecisionTx(
	ctx context.Context,
	tx pgx.Tx,
	idempotencyKey string,
	requestID RequestID,
	requestHash string,
) (DecisionResult, bool, error) {
	var existingRequestID string
	var existingHash string
	var decisionID string
	err := tx.QueryRow(ctx, `
		SELECT request.request_id,request.canonical_hash,decision.decision_id
		FROM workforce_executive_decision_requests request
		JOIN workforce_executive_decisions decision
		  ON decision.tenant_id=request.tenant_id
		 AND decision.organization_id=request.organization_id
		 AND decision.request_id=request.request_id
		WHERE request.tenant_id=$1 AND request.organization_id=$2
		  AND request.idempotency_key=$3
	`, store.tenantID, store.organizationID, idempotencyKey).Scan(
		&existingRequestID, &existingHash, &decisionID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionResult{}, false, nil
	}
	if err != nil {
		return DecisionResult{}, false, fmt.Errorf("executive: inspect decision idempotency: %w", err)
	}
	if RequestID(existingRequestID) != requestID || existingHash != requestHash {
		return DecisionResult{}, false, ErrConflict
	}
	decision, err := store.loadDecisionRecord(ctx, tx, DecisionID(decisionID))
	if err != nil {
		return DecisionResult{}, false, err
	}
	result := DecisionResult{Decision: decision}
	if decision.FounderRequestID != nil {
		value, err := store.loadFounderRequestRecord(ctx, tx, *decision.FounderRequestID)
		if err != nil {
			return DecisionResult{}, false, err
		}
		result.FounderRequest = &value
	}
	if decision.IncidentID != nil {
		value, err := store.loadIncidentRecord(ctx, tx, *decision.IncidentID)
		if err != nil {
			return DecisionResult{}, false, err
		}
		result.Incident = &value
	}
	return result, true, nil
}

func (store *Store) loadCurrentAuthorityTx(
	ctx context.Context,
	tx pgx.Tx,
) (CompiledAuthority, string, error) {
	var state string
	var compiledID string
	var policyVersion uint64
	var compiledHash string
	var compiledSealed []byte
	var policyHash string
	var policySealed []byte
	err := tx.QueryRow(ctx, `
		SELECT head.state,head.compiled_authority_id,head.version,
		       compiled.canonical_hash,compiled.sealed_record,
		       policy.canonical_hash,policy.sealed_record
		FROM workforce_executive_policy_heads head
		JOIN workforce_executive_compiled_authorities compiled
		  ON compiled.tenant_id=head.tenant_id AND compiled.organization_id=head.organization_id
		 AND compiled.compiled_authority_id=head.compiled_authority_id
		 AND compiled.policy_version=head.version
		JOIN workforce_executive_policies policy
		  ON policy.tenant_id=head.tenant_id AND policy.organization_id=head.organization_id
		 AND policy.policy_id=head.policy_id AND policy.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.version DESC LIMIT 1
	`, store.tenantID, store.organizationID).Scan(
		&state, &compiledID, &policyVersion, &compiledHash, &compiledSealed,
		&policyHash, &policySealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompiledAuthority{}, "", ErrNotFound
	}
	if err != nil {
		return CompiledAuthority{}, "", fmt.Errorf("executive: load current policy authority: %w", err)
	}
	policyCanonical, err := store.vault.OpenRecord(
		store.policyAD(PolicyID("executive-policy:"+string(store.organizationID)), policyVersion),
		policySealed,
	)
	if err != nil || hashBytes(policyCanonical).Digest != policyHash {
		return CompiledAuthority{}, "", ErrIntegrity
	}
	delegation, err := contracts.DecodeCanonical[DelegationPolicy, *DelegationPolicy](policyCanonical)
	if err != nil || VerifyDelegationPolicy(delegation, store.founderKeyID, store.founderPublicKey) != nil {
		return CompiledAuthority{}, "", ErrIntegrity
	}
	compiledCanonical, err := store.vault.OpenRecord(
		store.compiledAD(compiledID, policyVersion), compiledSealed,
	)
	if err != nil || hashBytes(compiledCanonical).Digest != compiledHash {
		return CompiledAuthority{}, "", ErrIntegrity
	}
	compiled, err := contracts.DecodeCanonical[CompiledAuthority, *CompiledAuthority](compiledCanonical)
	if err != nil || compiled.PolicyHash.Digest != policyHash ||
		compiled.PolicyID != delegation.ID || compiled.PolicyVersion != delegation.Version {
		return CompiledAuthority{}, "", ErrIntegrity
	}
	return compiled, state, nil
}

func (store *Store) loadDecisionRecord(
	ctx context.Context,
	query rowQuerier,
	id DecisionID,
) (Decision, error) {
	var hash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_executive_decisions
		WHERE tenant_id=$1 AND organization_id=$2 AND decision_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrNotFound
	}
	if err != nil {
		return Decision{}, fmt.Errorf("executive: load decision: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.decisionAD(id), sealed)
	if err != nil || hashBytes(canonical).Digest != hash {
		return Decision{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Decision, *Decision](canonical)
	if err != nil || VerifyDecision(value, store.controllerKeyID, store.controllerPublic) != nil {
		return Decision{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) loadFounderRequestRecord(
	ctx context.Context,
	query rowQuerier,
	id FounderRequestID,
) (FounderDecisionRequest, error) {
	var hash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_founder_decision_requests
		WHERE tenant_id=$1 AND organization_id=$2 AND founder_request_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return FounderDecisionRequest{}, ErrNotFound
	}
	if err != nil {
		return FounderDecisionRequest{}, fmt.Errorf("executive: load founder decision request: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.founderRequestAD(id), sealed)
	if err != nil || hashBytes(canonical).Digest != hash {
		return FounderDecisionRequest{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[FounderDecisionRequest, *FounderDecisionRequest](canonical)
	if err != nil || VerifyFounderDecisionRequest(value, store.controllerKeyID, store.controllerPublic) != nil {
		return FounderDecisionRequest{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) loadIncidentRecord(
	ctx context.Context,
	query rowQuerier,
	id IncidentID,
) (DecisionIncident, error) {
	var hash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_executive_decision_incidents
		WHERE tenant_id=$1 AND organization_id=$2 AND incident_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionIncident{}, ErrNotFound
	}
	if err != nil {
		return DecisionIncident{}, fmt.Errorf("executive: load decision incident: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.incidentAD(id), sealed)
	if err != nil || hashBytes(canonical).Digest != hash {
		return DecisionIncident{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[DecisionIncident, *DecisionIncident](canonical)
	if err != nil || VerifyDecisionIncident(value, store.controllerKeyID, store.controllerPublic) != nil {
		return DecisionIncident{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) loadConsumptionRecord(
	ctx context.Context,
	query rowQuerier,
	id ConsumptionID,
) (DecisionConsumption, error) {
	var hash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_executive_decision_consumptions
		WHERE tenant_id=$1 AND organization_id=$2 AND consumption_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionConsumption{}, ErrNotFound
	}
	if err != nil {
		return DecisionConsumption{}, fmt.Errorf("executive: load decision consumption: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.consumptionAD(id), sealed)
	if err != nil || hashBytes(canonical).Digest != hash {
		return DecisionConsumption{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[DecisionConsumption, *DecisionConsumption](canonical)
	if err != nil || VerifyDecisionConsumption(value, store.controllerKeyID, store.controllerPublic) != nil {
		return DecisionConsumption{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) rollingUseTx(
	ctx context.Context,
	tx pgx.Tx,
	aggregationKey string,
	since time.Time,
	scoped bool,
) (uint64, uint64, error) {
	query := `
		SELECT COALESCE(SUM(decision.capital_microunits),0)::BIGINT,
		       COALESCE(SUM(decision.exposure_microunits),0)::BIGINT
		FROM workforce_executive_decisions decision
		JOIN workforce_executive_decision_requests request
		  ON request.tenant_id=decision.tenant_id
		 AND request.organization_id=decision.organization_id
		 AND request.request_id=decision.request_id
		WHERE decision.tenant_id=$1 AND decision.organization_id=$2
		  AND decision.outcome IN ('authorized','emergency_paused')
		  AND decision.created_at >= $3`
	arguments := []any{store.tenantID, store.organizationID, since}
	if scoped {
		query += ` AND request.aggregation_key=$4`
		arguments = append(arguments, aggregationKey)
	}
	var capital int64
	var exposure int64
	if err := tx.QueryRow(ctx, query, arguments...).Scan(&capital, &exposure); err != nil {
		return 0, 0, fmt.Errorf("executive: load rolling authority use: %w", err)
	}
	if capital < 0 || exposure < 0 {
		return 0, 0, ErrIntegrity
	}
	return uint64(capital), uint64(exposure), nil
}

func (store *Store) verifyMissionProjectionTx(
	ctx context.Context,
	tx pgx.Tx,
	current mission.CurrentAuthority,
) error {
	var organizationVersion uint64
	var missionVersion uint64
	var constitutionVersion uint64
	var capitalVersion uint64
	var issuerVersion uint64
	var state string
	var issuerRevokedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT organization_v2_version,mission_version,constitution_version,
		       capital_envelope_version,issuer_policy_version,state,issuer_revoked_at
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2 FOR SHARE
	`, store.tenantID, store.organizationID).Scan(
		&organizationVersion, &missionVersion, &constitutionVersion,
		&capitalVersion, &issuerVersion, &state, &issuerRevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAuthorityNotCurrent
	}
	if err != nil {
		return fmt.Errorf("executive: lock company authority projection: %w", err)
	}
	authority := current.Authority
	if state != "active" || issuerRevokedAt != nil ||
		organizationVersion != authority.Organization.Version ||
		missionVersion != authority.Mission.Version ||
		constitutionVersion != authority.Constitution.Version ||
		capitalVersion != authority.Capital.Version ||
		issuerVersion != authority.IssuerPolicy.Version {
		return ErrAuthorityNotCurrent
	}
	return nil
}

func compiledMatchesMission(compiled CompiledAuthority, authority mission.ActivationAuthority) error {
	if compiled.OrganizationID != authority.Mission.OrganizationID ||
		compiled.MissionVersion != authority.Mission.Version ||
		compiled.ConstitutionVersion != authority.Constitution.Version ||
		compiled.CapitalEnvelopeVersion != authority.Capital.Version ||
		compiled.IssuerPolicyVersion != authority.IssuerPolicy.Version {
		return ErrAuthorityNotCurrent
	}
	missionHash, err := contracts.HashCanonical(&authority.Mission)
	if err != nil || missionHash != compiled.MissionHash {
		return ErrAuthorityNotCurrent
	}
	constitutionHash, err := contracts.HashCanonical(&authority.Constitution)
	if err != nil || constitutionHash != compiled.ConstitutionHash {
		return ErrAuthorityNotCurrent
	}
	capitalHash, err := contracts.HashCanonical(&authority.Capital)
	if err != nil || capitalHash != compiled.CapitalEnvelopeHash {
		return ErrAuthorityNotCurrent
	}
	issuerHash, err := contracts.HashCanonical(&authority.IssuerPolicy)
	if err != nil || issuerHash != compiled.IssuerPolicyHash {
		return ErrAuthorityNotCurrent
	}
	return nil
}

func (store *Store) prepare(
	associatedData vault.AD,
	value contracts.Validatable,
) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := hashBytes(canonical).Digest
	sealed, err := store.vault.SealRecord(associatedData, canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("executive: seal record: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("executive: time source must return UTC")
	}
	return now, nil
}

func hashBytes(value []byte) contracts.ContentHash {
	digest := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(digest[:])}
}

func semanticAggregationKey(request DecisionRequest) string {
	sum := sha256.Sum256([]byte(
		string(request.OrganizationID) + "\x00" + string(request.InitiativeID) + "\x00" +
			request.Action.family() + "\x00" + request.TargetID + "\x00" +
			request.Jurisdiction + "\x00" + request.Counterparty,
	))
	return hex.EncodeToString(sum[:])
}

func canonicalReasons(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	write := 0
	for _, value := range result {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if write > 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func (store *Store) policyAD(id PolicyID, version uint64) vault.AD {
	return store.recordAD("policy", string(id), fmt.Sprintf("%s.v%d", DelegationPolicySchemaVersion, version))
}

func (store *Store) compiledAD(id string, version uint64) vault.AD {
	return store.recordAD("compiled", id, fmt.Sprintf("%s.v%d", CompiledAuthoritySchemaVersion, version))
}

func (store *Store) revocationAD(id string) vault.AD {
	return store.recordAD("revocation", id, RevocationSchemaVersion)
}

func (store *Store) requestAD(id RequestID) vault.AD {
	return store.recordAD("request", string(id), DecisionRequestSchemaVersion)
}

func (store *Store) reviewAD(id ReviewID) vault.AD {
	return store.recordAD("review", string(id), ReviewSchemaVersion)
}

func (store *Store) decisionAD(id DecisionID) vault.AD {
	return store.recordAD("decision", string(id), DecisionSchemaVersion)
}

func (store *Store) founderRequestAD(id FounderRequestID) vault.AD {
	return store.recordAD("founder-request", string(id), FounderRequestSchemaVersion)
}

func (store *Store) incidentAD(id IncidentID) vault.AD {
	return store.recordAD("incident", string(id), IncidentSchemaVersion)
}

func (store *Store) consumptionAD(id ConsumptionID) vault.AD {
	return store.recordAD("consumption", string(id), ConsumptionSchemaVersion)
}

func (store *Store) recordAD(kind, id, schema string) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.executive." + kind,
		Stream: string(store.organizationID) + "/" + id,
		Schema: schema,
	}
}
