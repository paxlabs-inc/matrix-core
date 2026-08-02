package companyruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

type CycleRuntimeBinding struct {
	ModelProvider string
	ModelID       string
	MGSReference  string
	MGSDigest     string
}

func (value CycleRuntimeBinding) Validate() error {
	if !validToken(value.ModelProvider) || !validToken(value.ModelID) ||
		value.MGSReference == "" || len(value.MGSReference) > 512 || len(value.MGSDigest) != 64 {
		return fmt.Errorf("company runtime: cycle execution binding is invalid")
	}
	for _, character := range value.MGSDigest {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return fmt.Errorf("company runtime: cycle MGS digest is invalid")
		}
	}
	return nil
}

type CycleDispatchResult struct {
	CycleID      string   `json:"cycle_id"`
	WorkOrderID  string   `json:"work_order_id"`
	GoalID       string   `json:"goal_id"`
	IntentIDs    []string `json:"intent_ids"`
	WakeID       string   `json:"wake_id"`
	Deduplicated bool     `json:"deduplicated"`
}

type CycleDispatcher struct {
	store     *Store
	scheduler *scheduler.Store
	runtime   CycleRuntimeBinding
}

func NewCycleDispatcher(
	store *Store,
	schedulerStore *scheduler.Store,
	runtime CycleRuntimeBinding,
) (*CycleDispatcher, error) {
	if store == nil || schedulerStore == nil || runtime.Validate() != nil {
		return nil, fmt.Errorf("company runtime: cycle dispatcher dependencies are required")
	}
	return &CycleDispatcher{store: store, scheduler: schedulerStore, runtime: runtime}, nil
}

func (dispatcher *CycleDispatcher) Authority(
	ctx context.Context,
) (workorder.CompanyCycleAuthority, error) {
	configuration, err := dispatcher.store.LoadCurrent(ctx)
	if err != nil {
		return workorder.CompanyCycleAuthority{}, err
	}
	var companyState string
	if err := dispatcher.store.pool.QueryRow(ctx, `
		SELECT state FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, dispatcher.store.tenantID, dispatcher.store.organizationID).Scan(&companyState); err != nil {
		return workorder.CompanyCycleAuthority{}, err
	}
	if companyState != "active" {
		return workorder.CompanyCycleAuthority{}, fmt.Errorf("company runtime: company is not active")
	}
	configurationHash, err := contracts.HashCanonical(&configuration)
	if err != nil {
		return workorder.CompanyCycleAuthority{}, err
	}
	now, err := dispatcher.store.currentTime()
	if err != nil {
		return workorder.CompanyCycleAuthority{}, err
	}
	return workorder.CompanyCycleAuthority{
		RuntimeConfigID: configuration.ID, RuntimeConfigVersion: configuration.Version,
		RuntimeConfigHash: configurationHash,
		MissionVersion:    configuration.MissionVersion, MissionHash: configuration.MissionHash,
		ConstitutionVersion:      configuration.ConstitutionVersion,
		ConstitutionHash:         configuration.ConstitutionHash,
		CapitalEnvelopeVersion:   configuration.CapitalEnvelopeVersion,
		CapitalEnvelopeHash:      configuration.CapitalEnvelopeHash,
		AggregateLimitMicrounits: configuration.Procedure.MaximumCapitalMicrounits,
		ControllerKeyID:          dispatcher.store.controllerKeyID,
		ControllerPublicKey:      dispatcher.store.controllerPublic,
		EffectiveAt:              configuration.EffectiveAt, At: now, ExpiresAt: configuration.ExpiresAt,
	}, nil
}

func (dispatcher *CycleDispatcher) Dispatch(
	ctx context.Context,
	plan portfolio.CyclePlan,
	configuration StartConfiguration,
) (CycleDispatchResult, error) {
	if err := plan.Validate(); err != nil {
		return CycleDispatchResult{}, err
	}
	if plan.OrganizationID != dispatcher.store.organizationID {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle organization mismatch")
	}
	authority, err := dispatcher.Authority(ctx)
	if err != nil {
		return CycleDispatchResult{}, err
	}
	configurationHash, err := contracts.HashCanonical(&configuration)
	if err != nil || authority.RuntimeConfigHash != configurationHash ||
		authority.RuntimeConfigVersion != configuration.Version {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle configuration is no longer current")
	}
	now := authority.At
	deadline := plan.NextAt
	if configuration.ExpiresAt.Before(deadline) {
		deadline = configuration.ExpiresAt
	}
	if !deadline.After(now) {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle deadline elapsed")
	}
	cycleLimit := configuration.Procedure.MaximumCapitalMicrounits / 100
	if cycleLimit == 0 {
		cycleLimit = 1
	}
	criteria := make([]string, len(plan.RequiredCapabilities))
	for index, capability := range plan.RequiredCapabilities {
		criteria[index] = "evidence_hash: capability:" + capability
	}
	slices.Sort(criteria)
	order := workorder.CompanyCycleOrder{
		SchemaVersion: workorder.CompanyCycleOrderSchemaVersion,
		ID:            "cycle-order:" + plan.ID, OrganizationID: dispatcher.store.organizationID,
		ControllerID: "company-controller:" + string(dispatcher.store.organizationID), Version: 1,
		Objective: "Complete the evidence-backed " + string(plan.Kind) + " company cycle",
		Scope:     "company-cycle:" + string(plan.Kind), Departments: slices.Clone(plan.Departments),
		Priority: 100, Budget: workorder.Budget{MaxTasks: 8, MaxSpendMicrounits: cycleLimit},
		Deadline: deadline, Autonomy: "bounded_auto", AcceptanceCriteria: criteria,
		ModelProvider: dispatcher.runtime.ModelProvider, ModelID: dispatcher.runtime.ModelID,
		MGSReference: dispatcher.runtime.MGSReference, MGSDigest: dispatcher.runtime.MGSDigest,
		Binding: workorder.CompanyCycleBinding{
			RuntimeConfigID: configuration.ID, RuntimeConfigVersion: configuration.Version,
			RuntimeConfigHash: configurationHash,
			MissionVersion:    configuration.MissionVersion, MissionHash: configuration.MissionHash,
			ConstitutionVersion:    configuration.ConstitutionVersion,
			ConstitutionHash:       configuration.ConstitutionHash,
			CapitalEnvelopeVersion: configuration.CapitalEnvelopeVersion,
			CapitalEnvelopeHash:    configuration.CapitalEnvelopeHash,
			CycleID:                plan.ID, CadenceKind: string(plan.Kind),
			RequiredCapabilities:     slices.Clone(plan.RequiredCapabilities),
			IndependentAudit:         plan.IndependentAudit,
			MaximumCycleMicrounits:   cycleLimit,
			AggregateLimitMicrounits: configuration.Procedure.MaximumCapitalMicrounits,
		},
		CreatedAt: plan.DueAt, IdempotencyKey: "company-cycle:" + plan.ID,
	}
	if err := workorder.SignCompanyCycle(&order, authority, dispatcher.store.controllerPrivate); err != nil {
		return CycleDispatchResult{}, err
	}
	return dispatcher.persist(ctx, plan, order, configurationHash, now)
}

func (dispatcher *CycleDispatcher) Reconcile(ctx context.Context) error {
	now, err := dispatcher.store.currentTime()
	if err != nil {
		return err
	}
	_, err = dispatcher.store.pool.Exec(ctx, `
		UPDATE workforce_company_cycle_runs run
		SET state=CASE WHEN graph.state='completed' THEN 'completed' ELSE 'failed' END,
		    updated_at=$1
		FROM workforce_company_cycle_dispatches dispatch
		JOIN workforce_work_nodes graph
		  ON graph.tenant_id=dispatch.tenant_id
		 AND graph.organization_id=dispatch.organization_id
		 AND graph.node_id=dispatch.goal_id
		WHERE run.tenant_id=dispatch.tenant_id
		  AND run.organization_id=dispatch.organization_id
		  AND run.cycle_id=dispatch.cycle_id
		  AND run.tenant_id=$2 AND run.organization_id=$3
		  AND run.state='dispatched'
		  AND graph.state IN ('completed','failed','cancelled')
	`, now, dispatcher.store.tenantID, dispatcher.store.organizationID)
	if err != nil {
		return fmt.Errorf("company runtime: reconcile cycle execution: %w", err)
	}
	return nil
}

func (dispatcher *CycleDispatcher) persist(
	ctx context.Context,
	plan portfolio.CyclePlan,
	order workorder.CompanyCycleOrder,
	configurationHash contracts.ContentHash,
	now time.Time,
) (CycleDispatchResult, error) {
	canonical, err := contracts.EncodeCanonical(&order)
	if err != nil {
		return CycleDispatchResult{}, err
	}
	hash := sha256.Sum256(canonical)
	canonicalHash := hex.EncodeToString(hash[:])
	sealed, err := dispatcher.store.vault.SealRecord(dispatcher.orderAD(order), canonical)
	if err != nil {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: seal cycle Work Order: %w", err)
	}
	goalID := "goal:cycle:" + order.ID
	wakeID := "wake:cycle:" + order.ID + ":1"
	intentIDs := make([]string, len(order.Departments))
	for index, department := range order.Departments {
		intentIDs[index] = fmt.Sprintf("intent:cycle:%s:%02d:%s", order.ID, index+1, department)
	}
	result := CycleDispatchResult{
		CycleID: plan.ID, WorkOrderID: order.ID, GoalID: goalID,
		IntentIDs: slices.Clone(intentIDs), WakeID: wakeID,
	}
	tx, err := dispatcher.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CycleDispatchResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		dispatcher.store.tenantID+"|"+string(dispatcher.store.organizationID)+"|cycle-dispatch|"+plan.ID); err != nil {
		return CycleDispatchResult{}, err
	}
	var existingID, existingHash string
	err = tx.QueryRow(ctx, `
		SELECT work_order_id,canonical_hash FROM workforce_company_cycle_orders
		WHERE tenant_id=$1 AND organization_id=$2 AND cycle_id=$3
	`, dispatcher.store.tenantID, dispatcher.store.organizationID, plan.ID).Scan(
		&existingID, &existingHash,
	)
	if err == nil {
		if existingID != order.ID || existingHash != canonicalHash {
			return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle dispatch conflict")
		}
		result.Deduplicated = true
		if err := tx.Commit(ctx); err != nil {
			return CycleDispatchResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CycleDispatchResult{}, err
	}
	var cycleState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM workforce_company_cycle_runs
		WHERE tenant_id=$1 AND organization_id=$2 AND cycle_id=$3 FOR UPDATE
	`, dispatcher.store.tenantID, dispatcher.store.organizationID, plan.ID).Scan(&cycleState); err != nil {
		return CycleDispatchResult{}, err
	}
	if cycleState != "planned" {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle is not dispatchable")
	}
	var committedSpend uint64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE
			WHEN wake.state='completed' THEN wake.actual_spend_microunits
			WHEN wake.state IN ('queued','dispatched') THEN wake.budget_spend_microunits
			ELSE 0 END),0)
		FROM workforce_company_cycle_dispatches dispatch
		JOIN workforce_scheduled_wakes wake
		  ON wake.tenant_id=dispatch.tenant_id AND wake.organization_id=dispatch.organization_id
		 AND wake.wake_id=dispatch.wake_id
		WHERE dispatch.tenant_id=$1 AND dispatch.organization_id=$2
	`, dispatcher.store.tenantID, dispatcher.store.organizationID).Scan(&committedSpend); err != nil {
		return CycleDispatchResult{}, err
	}
	if committedSpend > order.Binding.AggregateLimitMicrounits ||
		order.Budget.MaxSpendMicrounits > order.Binding.AggregateLimitMicrounits-committedSpend {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle aggregate capital limit reached")
	}
	type seatAuthority struct {
		departmentID string
		seatID       string
		mandateID    string
		version      uint64
	}
	authorities := make([]seatAuthority, len(order.Departments))
	for index, department := range order.Departments {
		if err := tx.QueryRow(ctx, `
			SELECT seat.department_id,seat.seat_id,seat.mandate_id,seat.mandate_version
			FROM workforce_organization_seats seat
			JOIN workforce_organization_departments department
			  ON department.tenant_id=seat.tenant_id
			 AND department.organization_id=seat.organization_id
			 AND department.department_id=seat.department_id
			WHERE seat.tenant_id=$1 AND seat.organization_id=$2
			  AND department.department_kind=$3 AND seat.seat_role='lead'
			  AND seat.active=TRUE AND department.enabled=TRUE
		`, dispatcher.store.tenantID, dispatcher.store.organizationID, department).Scan(
			&authorities[index].departmentID, &authorities[index].seatID,
			&authorities[index].mandateID, &authorities[index].version,
		); err != nil {
			return CycleDispatchResult{}, fmt.Errorf("company runtime: resolve cycle department lead: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,title,state,base_priority,
			created_at,updated_at,deadline,contested,version
		) VALUES ($1,$2,$3,'goal',$4,'pending',$5,$6,$6,$7,FALSE,1)
	`, dispatcher.store.tenantID, dispatcher.store.organizationID, goalID,
		order.Objective, order.Priority, now, order.Deadline); err != nil {
		return CycleDispatchResult{}, err
	}
	for index, intentID := range intentIDs {
		state := "pending"
		if index == 0 {
			state = "eligible"
		}
		title := fmt.Sprintf("%s — %s", order.Objective, order.Departments[index])
		if len(title) > 512 {
			title = title[:512]
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_work_nodes (
				tenant_id,organization_id,node_id,node_kind,owner_seat_id,
				owner_department_id,title,state,base_priority,created_at,updated_at,
				deadline,contested,version
			) VALUES ($1,$2,$3,'intent',$4,$5,$6,$7,$8,$9,$9,$10,FALSE,1)
		`, dispatcher.store.tenantID, dispatcher.store.organizationID, intentID,
			authorities[index].seatID, authorities[index].departmentID, title,
			state, order.Priority, now, order.Deadline); err != nil {
			return CycleDispatchResult{}, err
		}
		if index > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_work_edges (
					tenant_id,organization_id,prerequisite_node_id,dependent_node_id,
					edge_kind,created_at
				) VALUES ($1,$2,$3,$4,'dependency',$5)
			`, dispatcher.store.tenantID, dispatcher.store.organizationID,
				intentIDs[index-1], intentID, now); err != nil {
				return CycleDispatchResult{}, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id,organization_id,prerequisite_node_id,dependent_node_id,edge_kind,created_at
		) VALUES ($1,$2,$3,$4,'dependency',$5)
	`, dispatcher.store.tenantID, dispatcher.store.organizationID,
		intentIDs[len(intentIDs)-1], goalID, now); err != nil {
		return CycleDispatchResult{}, err
	}
	wake := scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1", WakeID: wakeID,
		ScheduleID: "schedule:cycle:" + order.ID, TenantID: dispatcher.store.tenantID,
		OrganizationID: string(dispatcher.store.organizationID),
		SeatID:         authorities[0].seatID, MandateID: authorities[0].mandateID,
		MandateVersion: authorities[0].version, Trigger: scheduler.TriggerRecurring,
		Reason: "founder-authorized recurring company cycle", ScheduledAt: now,
		Budget: scheduler.Budget{
			MaxTasks: order.Budget.MaxTasks, MaxSpendMicrounits: order.Budget.MaxSpendMicrounits,
		},
		Model:          scheduler.ModelBinding{Provider: order.ModelProvider, ModelID: order.ModelID},
		MGS:            scheduler.MGSBinding{Reference: order.MGSReference, Digest: order.MGSDigest},
		IdempotencyKey: "cycle-wake:" + plan.ID,
		CoalesceKey:    "company-cycle:" + string(plan.Kind), GraphScope: intentIDs[0],
	}
	if _, err := dispatcher.scheduler.EnqueueTx(ctx, tx, wake, now); err != nil {
		return CycleDispatchResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_cycle_orders (
			tenant_id,organization_id,work_order_id,cycle_id,runtime_config_id,
			runtime_config_version,runtime_config_hash,controller_id,controller_key_id,
			canonical_hash,sealed_order,deadline,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, dispatcher.store.tenantID, dispatcher.store.organizationID, order.ID, plan.ID,
		order.Binding.RuntimeConfigID, order.Binding.RuntimeConfigVersion,
		configurationHash.Digest, order.ControllerID, order.Signature.KeyID,
		canonicalHash, sealed, order.Deadline, order.CreatedAt); err != nil {
		return CycleDispatchResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_cycle_dispatches (
			tenant_id,organization_id,work_order_id,cycle_id,goal_id,
			initial_intent_id,wake_id,dispatched_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, dispatcher.store.tenantID, dispatcher.store.organizationID, order.ID,
		plan.ID, goalID, intentIDs[0], wakeID, now); err != nil {
		return CycleDispatchResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_company_cycle_runs SET state='dispatched',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND cycle_id=$4 AND state='planned'
	`, now, dispatcher.store.tenantID, dispatcher.store.organizationID, plan.ID); err != nil {
		return CycleDispatchResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: commit cycle dispatch: %w", err)
	}
	return result, nil
}

func (dispatcher *CycleDispatcher) orderAD(order workorder.CompanyCycleOrder) vault.AD {
	return vault.AD{
		User: dispatcher.store.tenantID, Store: "workforce.company-cycle-work-order",
		Stream: string(dispatcher.store.organizationID) + "/" + order.ID,
		Schema: workorder.CompanyCycleOrderSchemaVersion,
	}
}
