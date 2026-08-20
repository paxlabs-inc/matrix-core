package productexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/initiative"
	"centra/workforce/internal/productcapability"
	"centra/workforce/internal/squad"
	"centra/workforce/internal/workorder"
	"centra/workforce/scheduler"
)

func (store *Store) Start(ctx context.Context, request StartRequest) (View, error) {
	if err := request.Validate(); err != nil || request.OrganizationID != store.organizationID {
		return View{}, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return View{}, err
	}
	if request.CreatedAt.After(now) || !request.SquadRequirement.ExpiresAt.After(now) {
		return View{}, ErrUnauthorized
	}
	if existing, found, err := store.findStartReplay(ctx, request); err != nil || found {
		return existing, err
	}
	currentAuthority, err := store.mission.LoadCurrent(ctx)
	if err != nil || !currentAuthority.Executable(now) ||
		currentAuthority.Authority.Mission.Version != request.CompanyAuthority.CurrentMissionVersion ||
		currentAuthority.Authority.Constitution.Version != request.CompanyAuthority.CurrentConstitutionVersion ||
		currentAuthority.Authority.Capital.Version != request.CompanyAuthority.CurrentCapitalEnvelopeVersion ||
		currentAuthority.Authority.IssuerPolicy.Version != request.CompanyAuthority.Policy.Version {
		return View{}, ErrUnauthorized
	}
	authority := request.CompanyAuthority
	authority.Policy = currentAuthority.Authority.IssuerPolicy
	authority.At = now
	if err := authority.Validate(store.organizationID); err != nil {
		return View{}, ErrUnauthorized
	}
	checkpoint, err := store.lifecycle.Load(ctx, store.organizationID, request.InitiativeID)
	if err != nil || checkpoint.State != companylifecycle.StateFund {
		return View{}, fmt.Errorf("%w: initiative must be funded", ErrInvalidPhase)
	}
	currentPlan, err := store.plans.LoadCurrent(ctx, string(request.InitiativeID), authority)
	if err != nil {
		return View{}, fmt.Errorf("product execution: load active initiative plan: %w", err)
	}
	if currentPlan.State != "active" {
		return View{}, ErrUnauthorized
	}
	plan := currentPlan.Plan
	planHash := plan.Hash
	if planHash.Validate() != nil {
		return View{}, ErrIntegrity
	}
	orders, err := store.stageOrders(plan, request, authority)
	if err != nil {
		return View{}, err
	}
	assignmentResult, err := store.squads.Assign(ctx, request.SquadRequirement)
	if err != nil {
		return View{}, fmt.Errorf("product execution: form dynamic squad: %w", err)
	}
	assignment := assignmentResult.Assignment
	if !assignment.ExpiresAt.After(now) {
		return View{}, ErrUnauthorized
	}
	bindings, err := store.bindStages(ctx, request, assignment, orders)
	if err != nil {
		return View{}, err
	}
	start := StartRecord{
		SchemaVersion: SchemaVersion, Request: request, PlanID: plan.ID,
		PlanVersion: plan.Version, PlanHash: planHash, Assignment: assignment,
	}
	if err := start.Validate(); err != nil {
		return View{}, err
	}
	canonical, err := contracts.EncodeCanonical(start)
	if err != nil {
		return View{}, err
	}
	canonicalHash, err := contracts.HashCanonical(start)
	if err != nil {
		return View{}, err
	}
	sealed, err := store.vault.SealRecord(store.startAD(request.ID), canonical)
	if err != nil {
		return View{}, err
	}
	initialCheckpoint := productcapability.Checkpoint{
		SchemaVersion: productcapability.CheckpointSchemaVersion,
		ID:            productcapability.CheckpointID("checkpoint:product-execution:" + string(request.ID)),
		Version:       1, OrganizationID: store.organizationID,
		InitiativeID: productcapability.InitiativeID(request.InitiativeID),
		HandoffID:    request.HandoffID, ProjectID: request.ProjectID,
		WorkspaceID: request.WorkspaceID, Phase: productcapability.PhaseIntake,
		Source: request.BaselineSource, BrainViewDigest: request.BrainViewDigest,
		CompletedRecordIDs: []productcapability.RecordID{},
		CommittedEffectIDs: []string{}, ReconciledEffectIDs: []string{},
		IdempotencyKey: "product-execution:" + string(request.ID) + ":checkpoint:1",
		UpdatedAt:      now,
	}
	if _, _, err := store.products.AdvanceCheckpoint(ctx, 0, initialCheckpoint); err != nil {
		return View{}, fmt.Errorf("product execution: create restart checkpoint: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return View{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+string(store.organizationID)+"|product-execution|"+string(request.ID)); err != nil {
		return View{}, err
	}
	var existingID string
	err = tx.QueryRow(ctx, `
		SELECT execution_id FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2
		  AND (execution_id=$3 OR idempotency_key=$4)
	`, store.tenantID, store.organizationID, request.ID, request.IdempotencyKey).Scan(&existingID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return View{}, err
		}
		return store.findStartReplayExact(ctx, request, ExecutionID(existingID))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return View{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_product_executions (
			tenant_id,organization_id,execution_id,initiative_id,plan_id,plan_version,
			plan_hash,squad_assignment_id,project_id,workspace_id,phase,version,
			checkpoint_version,idempotency_key,canonical_hash,sealed_start,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'product_queued',1,1,$11,$12,$13,$14,$14)
	`, store.tenantID, store.organizationID, request.ID, request.InitiativeID,
		plan.ID, plan.Version, planHash.Digest, assignment.ID, request.ProjectID,
		request.WorkspaceID, request.IdempotencyKey, canonicalHash.Digest, sealed, now); err != nil {
		return View{}, fmt.Errorf("product execution: insert execution head: %w", err)
	}
	for _, binding := range bindings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_product_execution_stage_bindings (
				tenant_id,organization_id,execution_id,stage,plan_node_id,work_order_id,
				need_id,seat_id,department_id,seat_role,mandate_id,mandate_version,
				mandate_digest,goal_id,intent_id,wake_id,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		`, store.tenantID, store.organizationID, request.ID, binding.Stage,
			binding.PlanNodeID, binding.WorkOrderID, binding.NeedID, binding.SeatID,
			binding.DepartmentID, binding.Role, binding.MandateID, binding.MandateVersion,
			binding.MandateDigest.Digest, binding.GoalID, binding.IntentID, binding.WakeID,
			now); err != nil {
			return View{}, fmt.Errorf("product execution: insert %s binding: %w", binding.Stage, err)
		}
	}
	if err := store.dispatchStageTx(ctx, tx, start, bindings[0], orders[StageProduct], now); err != nil {
		return View{}, err
	}
	stage := StageProduct
	if err := store.appendEventTx(ctx, tx, request.ID, PhaseProductQueued,
		"stage_dispatched", &stage, string(bindings[0].WakeID),
		"start:"+request.IdempotencyKey, now); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("product execution: commit start: %w", err)
	}
	return store.Load(ctx, request.ID)
}

func (store *Store) findStartReplay(ctx context.Context, request StartRequest) (View, bool, error) {
	var id ExecutionID
	err := store.pool.QueryRow(ctx, `
		SELECT execution_id FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2
		  AND (execution_id=$3 OR idempotency_key=$4)
	`, store.tenantID, store.organizationID, request.ID, request.IdempotencyKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, false, nil
	}
	if err != nil {
		return View{}, false, err
	}
	view, err := store.findStartReplayExact(ctx, request, id)
	return view, true, err
}

func (store *Store) findStartReplayExact(
	ctx context.Context,
	request StartRequest,
	id ExecutionID,
) (View, error) {
	existing, err := store.loadStart(ctx, id)
	if err != nil {
		return View{}, err
	}
	existingHash, err := contracts.HashCanonical(existing.Request)
	if err != nil {
		return View{}, err
	}
	requestHash, err := contracts.HashCanonical(request)
	if err != nil || existingHash != requestHash || id != request.ID {
		return View{}, ErrConflict
	}
	return store.Load(ctx, id)
}

func (store *Store) stageOrders(
	plan initiative.Plan,
	request StartRequest,
	authority workorder.CompanyAuthority,
) (map[Stage]workorder.CompanyOrder, error) {
	nodes := make(map[string]initiative.CompiledNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.Template.ID] = node
	}
	result := make(map[Stage]workorder.CompanyOrder, len(orderedStages))
	for _, selected := range request.Stages {
		node, exists := nodes[selected.PlanNodeID]
		if !exists || node.Template.Kind != initiative.NodeWorkOrder ||
			node.State != initiative.StatePending || node.Order == nil {
			return nil, fmt.Errorf("product execution: %s is not a pending Work Order node", selected.Stage)
		}
		order := *node.Order
		if err := workorder.VerifyCompany(order, authority); err != nil {
			return nil, fmt.Errorf("product execution: verify %s Work Order: %w", selected.Stage, err)
		}
		if order.Binding.InitiativeID != string(request.InitiativeID) ||
			order.Binding.InitiativePlanID != plan.ID ||
			order.Binding.InitiativePlanVersion != plan.Version ||
			order.Binding.PlanNodeID != selected.PlanNodeID ||
			!order.Deadline.After(request.CreatedAt) {
			return nil, fmt.Errorf("product execution: %s Work Order scope or deadline drifted", selected.Stage)
		}
		if selected.Stage == StageBuild &&
			(order.ProjectID != request.ProjectID || order.WorkspaceID != request.WorkspaceID ||
				!slices.Contains(order.Departments, contracts.DepartmentDeveloper)) {
			return nil, fmt.Errorf("product execution: build Work Order lacks exact Developer scope")
		}
		result[selected.Stage] = order
	}
	return result, nil
}

func (store *Store) bindStages(
	ctx context.Context,
	request StartRequest,
	assignment squad.Assignment,
	orders map[Stage]workorder.CompanyOrder,
) ([]StageBinding, error) {
	result := make([]StageBinding, 0, len(request.Stages))
	for _, selected := range request.Stages {
		var chosen *squad.AssignmentMember
		for index := range assignment.Members {
			member := &assignment.Members[index]
			if !slices.Contains(member.NeedIDs, selected.NeedID) ||
				selected.Stage == StageVerification && member.Role != contracts.SeatAuditor ||
				selected.Stage != StageVerification && member.Role == contracts.SeatAuditor {
				continue
			}
			chosen = member
			break
		}
		if chosen == nil {
			return nil, fmt.Errorf("product execution: squad has no independent %s worker", selected.Stage)
		}
		var departmentKind contracts.DepartmentKind
		var active bool
		if err := store.pool.QueryRow(ctx, `
			SELECT department.department_kind,seat.active
			FROM workforce_organization_seats seat
			JOIN workforce_organization_departments department
			  ON department.tenant_id=seat.tenant_id
			 AND department.organization_id=seat.organization_id
			 AND department.department_id=seat.department_id
			WHERE seat.tenant_id=$1 AND seat.organization_id=$2 AND seat.seat_id=$3
			  AND seat.department_id=$4
		`, store.tenantID, store.organizationID, chosen.SeatID, chosen.DepartmentID).Scan(
			&departmentKind, &active,
		); err != nil || !active || !slices.Contains(orders[selected.Stage].Departments, departmentKind) {
			return nil, fmt.Errorf("product execution: %s worker is outside Work Order department authority", selected.Stage)
		}
		binding := StageBinding{
			Stage: selected.Stage, PlanNodeID: selected.PlanNodeID,
			WorkOrderID: orders[selected.Stage].ID, NeedID: selected.NeedID,
			SeatID: chosen.SeatID, DepartmentID: chosen.DepartmentID, Role: chosen.Role,
			MandateID: chosen.MandateID, MandateVersion: chosen.MandateVersion,
			MandateDigest: chosen.MandateDigest,
			GoalID:        stableStageID("goal", request.ID, selected.Stage),
			IntentID:      contracts.IntentID(stableStageID("intent", request.ID, selected.Stage)),
			WakeID:        contracts.WakeID(stableStageID("wake", request.ID, selected.Stage)),
		}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		result = append(result, binding)
	}
	return result, nil
}

func (store *Store) dispatchStageTx(
	ctx context.Context,
	tx pgx.Tx,
	start StartRecord,
	binding StageBinding,
	order workorder.CompanyOrder,
	now time.Time,
) error {
	var existingGoal, existingIntent, existingWake string
	err := tx.QueryRow(ctx, `
		SELECT goal_id,initial_intent_id,wake_id
		FROM workforce_company_work_order_dispatches
		WHERE tenant_id=$1 AND organization_id=$2 AND work_order_id=$3
	`, store.tenantID, store.organizationID, order.ID).Scan(
		&existingGoal, &existingIntent, &existingWake,
	)
	if err == nil {
		if existingGoal != binding.GoalID || existingIntent != string(binding.IntentID) ||
			existingWake != string(binding.WakeID) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,title,state,base_priority,
			created_at,updated_at,deadline,contested,version
		) VALUES ($1,$2,$3,'goal',$4,'pending',$5,$6,$6,$7,FALSE,1)
	`, store.tenantID, store.organizationID, binding.GoalID, order.Objective,
		order.Priority, now, order.Deadline); err != nil {
		return fmt.Errorf("product execution: insert %s goal: %w", binding.Stage, err)
	}
	title := order.Objective
	if len(title) > 512 {
		title = title[:512]
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,owner_seat_id,owner_department_id,
			title,state,base_priority,created_at,updated_at,deadline,contested,version
		) VALUES ($1,$2,$3,'intent',$4,$5,$6,'eligible',$7,$8,$8,$9,FALSE,1)
	`, store.tenantID, store.organizationID, binding.IntentID, binding.SeatID,
		binding.DepartmentID, title, order.Priority, now, order.Deadline); err != nil {
		return fmt.Errorf("product execution: insert %s intent: %w", binding.Stage, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id,organization_id,prerequisite_node_id,dependent_node_id,edge_kind,created_at
		) VALUES ($1,$2,$3,$4,'dependency',$5)
	`, store.tenantID, store.organizationID, binding.IntentID, binding.GoalID, now); err != nil {
		return err
	}
	wake := scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1", WakeID: string(binding.WakeID),
		ScheduleID: "schedule:" + string(binding.WakeID), TenantID: store.tenantID,
		OrganizationID: string(store.organizationID), SeatID: string(binding.SeatID),
		MandateID: string(binding.MandateID), MandateVersion: binding.MandateVersion,
		Trigger:     scheduler.TriggerDependency,
		Reason:      "verified product execution stage " + string(binding.Stage),
		ScheduledAt: now,
		Budget: scheduler.Budget{
			MaxTasks: order.Budget.MaxTasks, MaxSpendMicrounits: order.Budget.MaxSpendMicrounits,
		},
		Model:          scheduler.ModelBinding{Provider: order.ModelProvider, ModelID: order.ModelID},
		MGS:            scheduler.MGSBinding{Reference: order.MGSReference, Digest: order.MGSDigest},
		IdempotencyKey: "product-execution:" + string(start.Request.ID) + ":" + string(binding.Stage),
		CoalesceKey:    "product-stage:" + string(start.Request.ID) + ":" + string(binding.Stage),
		GraphScope:     string(binding.IntentID),
	}
	if _, err := store.scheduler.EnqueueTx(ctx, tx, wake, now); err != nil {
		return fmt.Errorf("product execution: enqueue %s wake: %w", binding.Stage, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_work_order_dispatches (
			tenant_id,organization_id,work_order_id,initiative_id,plan_version,
			plan_node_id,goal_id,initial_intent_id,wake_id,dispatched_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.tenantID, store.organizationID, order.ID, start.Request.InitiativeID,
		start.PlanVersion, binding.PlanNodeID, binding.GoalID, binding.IntentID,
		binding.WakeID, now); err != nil {
		return fmt.Errorf("product execution: bind %s dispatch: %w", binding.Stage, err)
	}
	return nil
}

func stableStageID(prefix string, executionID ExecutionID, stage Stage) string {
	sum := sha256.Sum256([]byte(string(executionID) + "|" + string(stage) + "|" + prefix))
	return prefix + ":product:" + hex.EncodeToString(sum[:16])
}
