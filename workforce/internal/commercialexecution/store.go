package commercialexecution

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/lease"
)

type IssuerPolicy struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	Phases    []Phase
}

func (value IssuerPolicy) validate() error {
	if token("issuer key id", value.KeyID) != nil || len(value.PublicKey) != ed25519.PublicKeySize ||
		len(value.Phases) == 0 || len(value.Phases) > len(phaseOrder) {
		return fmt.Errorf("commercial execution: evidence issuer policy is invalid")
	}
	previous := -1
	for _, phase := range value.Phases {
		ordinal := phaseOrdinal(phase)
		if ordinal < 0 || ordinal <= previous {
			return fmt.Errorf("commercial execution: issuer phases must be ordered and unique")
		}
		previous = ordinal
	}
	return nil
}

type evidenceIssuer struct {
	publicKey ed25519.PublicKey
	phases    []Phase
}

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	leases         *lease.Store
	tenantID       string
	organizationID contracts.OrganizationID
	controllerKey  string
	controllerPub  ed25519.PublicKey
	issuers        map[string]evidenceIssuer
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	leaseStore *lease.Store,
	tenantID string,
	organizationID contracts.OrganizationID,
	controllerKeyID string,
	controllerPublicKey ed25519.PublicKey,
	issuerPolicies []IssuerPolicy,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || leaseStore == nil || tenantID == "" ||
		token("organization id", string(organizationID)) != nil ||
		token("controller key id", controllerKeyID) != nil ||
		len(controllerPublicKey) != ed25519.PublicKeySize || len(issuerPolicies) == 0 || now == nil ||
		userVault.User() != tenantID {
		return nil, fmt.Errorf("commercial execution: store dependencies and authorities are required")
	}
	issuers := make(map[string]evidenceIssuer, len(issuerPolicies))
	for _, policy := range issuerPolicies {
		if policy.validate() != nil || policy.KeyID == controllerKeyID {
			return nil, fmt.Errorf("commercial execution: evidence issuer registry is invalid")
		}
		if _, exists := issuers[policy.KeyID]; exists {
			return nil, fmt.Errorf("commercial execution: evidence issuer is duplicated")
		}
		issuers[policy.KeyID] = evidenceIssuer{
			publicKey: append(ed25519.PublicKey(nil), policy.PublicKey...),
			phases:    slices.Clone(policy.Phases),
		}
	}
	return &Store{pool: pool, vault: userVault, leases: leaseStore, tenantID: tenantID,
		organizationID: organizationID, controllerKey: controllerKeyID,
		controllerPub: append(ed25519.PublicKey(nil), controllerPublicKey...),
		issuers:       issuers, now: now}, nil
}

func (store *Store) Start(ctx context.Context, plan Plan) (Snapshot, bool, error) {
	if store == nil || VerifyPlan(plan, store.controllerKey, store.controllerPub) != nil ||
		plan.Body.OrganizationID != store.organizationID {
		return Snapshot{}, false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return Snapshot{}, false, err
	}
	if plan.Body.CreatedAt.After(now.Add(5*time.Minute)) || !plan.Body.Deadline.After(now) {
		return Snapshot{}, false, ErrUnauthorized
	}
	if err := store.authorize(ctx, plan.Body.Authority, plan.Body.Scope.Policies); err != nil {
		return Snapshot{}, false, err
	}
	canonical, err := contracts.EncodeCanonical(&plan)
	if err != nil {
		return Snapshot{}, false, err
	}
	hash, err := PlanHash(plan)
	if err != nil {
		return Snapshot{}, false, err
	}
	sealed, err := store.vault.SealRecord(store.planAD(plan.Body.ID), canonical)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: seal plan: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: begin plan: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(plan.Body.ID)); err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: lock plan: %w", err)
	}
	var existingID, existingHash string
	err = tx.QueryRow(ctx, `
		SELECT execution_id,plan_hash FROM workforce_commercial_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.tenantID, store.organizationID, plan.Body.IdempotencyKey).Scan(&existingID, &existingHash)
	if err == nil {
		if existingID != string(plan.Body.ID) || existingHash != hash.Digest {
			return Snapshot{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Snapshot{}, false, err
		}
		view, err := store.Load(ctx, plan.Body.ID)
		return view, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, fmt.Errorf("commercial execution: inspect plan replay: %w", err)
	}
	if err := store.validatePlanLineage(ctx, tx, plan.Body, now); err != nil {
		return Snapshot{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_commercial_executions (
			tenant_id,organization_id,execution_id,initiative_id,work_order_id,
			work_order_hash,product_execution_id,customer_connection_id,
			customer_connection_version,financial_connection_id,financial_connection_version,
			gate_id,metric_id,metric_version,metric_hash,state,current_phase,version,idempotency_key,
			plan_hash,controller_key_id,sealed_plan,deadline,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
		          'pending_external','acquisition',1,$16,$17,$18,$19,$20,$21,$21)
	`, store.tenantID, store.organizationID, plan.Body.ID, plan.Body.InitiativeID,
		plan.Body.WorkOrderID, plan.Body.WorkOrderHash.Digest, plan.Body.Scope.ProductExecutionID,
		plan.Body.Scope.CustomerConnectionID, plan.Body.Scope.CustomerConnectionVersion,
		plan.Body.Scope.FinancialConnectionID, plan.Body.Scope.FinancialConnectionVersion,
		plan.Body.Scope.Gate.ID, plan.Body.Scope.Gate.Metric.ID, plan.Body.Scope.Gate.Metric.Version,
		plan.Body.Scope.Gate.Metric.DefinitionHash.Digest, plan.Body.IdempotencyKey,
		hash.Digest, store.controllerKey, sealed,
		plan.Body.Deadline, now); err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: persist plan: %w", err)
	}
	for ordinal, phase := range phaseOrder {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_commercial_execution_steps (
				tenant_id,organization_id,execution_id,phase,ordinal,state,attempt,
				active_evidence_id,safe_code,updated_at,completed_at
			) VALUES ($1,$2,$3,$4,$5,'pending_external',1,NULL,NULL,$6,NULL)
		`, store.tenantID, store.organizationID, plan.Body.ID, phase, ordinal+1, now); err != nil {
			return Snapshot{}, false, fmt.Errorf("commercial execution: persist phase plan: %w", err)
		}
	}
	if err := store.insertTransition(ctx, tx, plan.Body.ID, PhaseAcquisition, "started",
		"", StatePendingExternal, "", plan.Body.Authority, plan.Body.IdempotencyKey, now); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: commit plan: %w", err)
	}
	view, err := store.Load(ctx, plan.Body.ID)
	return view, true, err
}

func (store *Store) Load(ctx context.Context, executionID ExecutionID) (Snapshot, error) {
	if store == nil || token("execution id", string(executionID)) != nil {
		return Snapshot{}, ErrUnauthorized
	}
	var view Snapshot
	var sealed []byte
	var planHash, controllerKey string
	err := store.pool.QueryRow(ctx, `
		SELECT state,current_phase,version,plan_hash,controller_key_id,sealed_plan,created_at,updated_at
		FROM workforce_commercial_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
	`, store.tenantID, store.organizationID, executionID).Scan(
		&view.State, &view.CurrentPhase, &view.Version, &planHash, &controllerKey,
		&sealed, &view.CreatedAt, &view.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrConflict
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("commercial execution: load execution: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.planAD(executionID), sealed)
	if err != nil {
		return Snapshot{}, ErrIntegrity
	}
	plan, err := contracts.DecodeCanonical[Plan, *Plan](opened)
	if err != nil || controllerKey != store.controllerKey ||
		VerifyPlan(plan, store.controllerKey, store.controllerPub) != nil {
		return Snapshot{}, ErrIntegrity
	}
	hash, err := PlanHash(plan)
	if err != nil || hash.Digest != planHash || plan.Body.ID != executionID ||
		plan.Body.OrganizationID != store.organizationID {
		return Snapshot{}, ErrIntegrity
	}
	view.Plan = plan
	view.CreatedAt = view.CreatedAt.UTC()
	view.UpdatedAt = view.UpdatedAt.UTC()
	rows, err := store.pool.Query(ctx, `
		SELECT phase,ordinal,state,attempt,active_evidence_id,updated_at
		FROM workforce_commercial_execution_steps
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		ORDER BY ordinal
	`, store.tenantID, store.organizationID, executionID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("commercial execution: load phases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var step StepView
		var evidenceID *string
		if err := rows.Scan(&step.Phase, &step.Ordinal, &step.State, &step.Attempt,
			&evidenceID, &step.UpdatedAt); err != nil {
			return Snapshot{}, fmt.Errorf("commercial execution: scan phase: %w", err)
		}
		if evidenceID != nil {
			value := EvidenceID(*evidenceID)
			step.ActiveEvidenceID = &value
		}
		step.UpdatedAt = step.UpdatedAt.UTC()
		view.Steps = append(view.Steps, step)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("commercial execution: iterate phases: %w", err)
	}
	if len(view.Steps) != len(phaseOrder) || !view.State.Valid() || !view.CurrentPhase.Valid() {
		return Snapshot{}, ErrIntegrity
	}
	return view, nil
}

func (store *Store) ListOpenIncidents(ctx context.Context) ([]IncidentView, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT incident_id,execution_id,phase,kind,safe_code,state,created_at,resolved_at
		FROM workforce_commercial_execution_incidents
		WHERE tenant_id=$1 AND organization_id=$2 AND state IN ('open','escalated')
		ORDER BY created_at,incident_id
	`, store.tenantID, store.organizationID)
	if err != nil {
		return nil, fmt.Errorf("commercial execution: list incidents: %w", err)
	}
	defer rows.Close()
	views := make([]IncidentView, 0)
	for rows.Next() {
		var view IncidentView
		if err := rows.Scan(&view.ID, &view.ExecutionID, &view.Phase, &view.Kind,
			&view.SafeCode, &view.State, &view.CreatedAt, &view.ResolvedAt); err != nil {
			return nil, fmt.Errorf("commercial execution: scan incident: %w", err)
		}
		view.CreatedAt = view.CreatedAt.UTC()
		if view.ResolvedAt != nil {
			value := view.ResolvedAt.UTC()
			view.ResolvedAt = &value
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (store *Store) authorize(ctx context.Context, binding LeaseBinding, policies []contracts.PolicyRef) error {
	if binding.Validate() != nil || binding.OrganizationID != store.organizationID {
		return ErrUnauthorized
	}
	grant, err := store.leases.Authorize(ctx, store.organizationID, binding.LeaseID, binding.Fence)
	if err != nil || grant.SeatID != binding.SeatID || grant.State != lease.StateActive {
		return ErrUnauthorized
	}
	for _, required := range policies {
		if !slices.Contains(grant.Policies, required) {
			return ErrUnauthorized
		}
	}
	return nil
}

func (store *Store) validatePlanLineage(ctx context.Context, tx pgx.Tx, plan PlanBody, now time.Time) error {
	var initiativeID, workOrderHash string
	var workOrderDeadline time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,canonical_hash,deadline FROM workforce_company_work_orders
		WHERE tenant_id=$1 AND organization_id=$2 AND work_order_id=$3 FOR SHARE
	`, store.tenantID, store.organizationID, plan.WorkOrderID).Scan(
		&initiativeID, &workOrderHash, &workOrderDeadline,
	); err != nil || initiativeID != plan.InitiativeID || workOrderHash != plan.WorkOrderHash.Digest ||
		plan.Deadline.After(workOrderDeadline) {
		return fmt.Errorf("%w: Work Order lineage is not exact and current", ErrUnauthorized)
	}
	var gateBound bool
	if err := tx.QueryRow(ctx, `
		SELECT $4 = ANY(business_outcome_gate_ids)
		FROM workforce_company_work_order_bindings
		WHERE tenant_id=$1 AND organization_id=$2 AND work_order_id=$3
	`, store.tenantID, store.organizationID, plan.WorkOrderID, plan.Scope.Gate.ID).Scan(&gateBound); err != nil || !gateBound {
		return fmt.Errorf("%w: business gate is absent from the Work Order", ErrUnauthorized)
	}
	var productInitiative, productPhase, productHash string
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,phase,canonical_hash FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR SHARE
	`, store.tenantID, store.organizationID, plan.Scope.ProductExecutionID).Scan(
		&productInitiative, &productPhase, &productHash,
	); err != nil || productInitiative != plan.InitiativeID || productPhase != "launched" ||
		productHash != plan.Scope.ProductExecutionHash.Digest {
		return fmt.Errorf("%w: product is not authoritatively launched", ErrUnauthorized)
	}
	var customerVersion, financialVersion uint64
	var customerState, financialState string
	var customerExpires, financialExpires time.Time
	if err := tx.QueryRow(ctx, `
		SELECT version,state,expires_at FROM workforce_customer_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 FOR SHARE
	`, store.tenantID, store.organizationID, plan.Scope.CustomerConnectionID).Scan(
		&customerVersion, &customerState, &customerExpires,
	); err != nil || customerVersion != plan.Scope.CustomerConnectionVersion ||
		customerState != "active" || !customerExpires.After(now) {
		return fmt.Errorf("%w: customer connection is not exact and active", ErrUnauthorized)
	}
	if err := tx.QueryRow(ctx, `
		SELECT version,state,expires_at FROM workforce_financial_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 FOR SHARE
	`, store.tenantID, store.organizationID, plan.Scope.FinancialConnectionID).Scan(
		&financialVersion, &financialState, &financialExpires,
	); err != nil || financialVersion != plan.Scope.FinancialConnectionVersion ||
		financialState != "active" || !financialExpires.After(now) {
		return fmt.Errorf("%w: financial connection is not exact and active", ErrUnauthorized)
	}
	var metricExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_business_metric_definitions
			WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3
			  AND version=$4 AND definition_hash=$5 AND initiative_id=$6
		)
	`, store.tenantID, store.organizationID, plan.Scope.Gate.Metric.ID,
		plan.Scope.Gate.Metric.Version, plan.Scope.Gate.Metric.DefinitionHash.Digest,
		plan.InitiativeID).Scan(&metricExists); err != nil || !metricExists {
		return fmt.Errorf("%w: preregistered metric is unavailable", ErrUnauthorized)
	}
	return nil
}

func (store *Store) insertTransition(
	ctx context.Context,
	tx pgx.Tx,
	executionID ExecutionID,
	phase Phase,
	kind string,
	from State,
	to State,
	evidenceID EvidenceID,
	authority LeaseBinding,
	idempotencyKey string,
	now time.Time,
) error {
	id, err := randomID("comtrans-")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_transitions (
			tenant_id,organization_id,transition_id,execution_id,phase,kind,
			from_state,to_state,evidence_id,lease_id,fence,seat_id,idempotency_key,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,NULLIF($9,''),$10,$11,$12,$13,$14)
	`, store.tenantID, store.organizationID, id, executionID, phase, kind, from, to,
		evidenceID, authority.LeaseID, authority.Fence, authority.SeatID, idempotencyKey, now)
	if err != nil {
		return fmt.Errorf("commercial execution: persist transition: %w", err)
	}
	return nil
}

func (store *Store) recordIncident(
	ctx context.Context,
	tx pgx.Tx,
	executionID ExecutionID,
	phase Phase,
	kind string,
	safeCode string,
	now time.Time,
) error {
	id, err := randomID("cominc-")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_incidents (
			tenant_id,organization_id,incident_id,execution_id,phase,kind,safe_code,
			state,created_at,resolved_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8,NULL)
	`, store.tenantID, store.organizationID, id, executionID, phase, kind, safeCode, now)
	return err
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("commercial execution: time source must return UTC")
	}
	return now, nil
}

func (store *Store) planAD(id ExecutionID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.commercial.execution.plan",
		Stream: string(store.organizationID) + "/" + string(id), Schema: SchemaVersion}
}

func (store *Store) evidenceAD(executionID ExecutionID, evidenceID EvidenceID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.commercial.execution.evidence",
		Stream: string(store.organizationID) + "/" + string(executionID) + "/" + string(evidenceID), Schema: SchemaVersion}
}

func (store *Store) correctionAD(executionID ExecutionID, correctionID CorrectionID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.commercial.execution.correction",
		Stream: string(store.organizationID) + "/" + string(executionID) + "/" + string(correctionID), Schema: SchemaVersion}
}

func (store *Store) recoveryAD(executionID ExecutionID, recoveryID RecoveryID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.commercial.execution.recovery",
		Stream: string(store.organizationID) + "/" + string(executionID) + "/" + string(recoveryID), Schema: SchemaVersion}
}

func (store *Store) lockKey(id ExecutionID) string {
	return store.tenantID + "|" + string(store.organizationID) + "|commercial-execution|" + string(id)
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}

func identifierHash(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
