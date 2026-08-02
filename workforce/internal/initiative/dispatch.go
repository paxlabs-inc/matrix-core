package initiative

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

type DispatchResult struct {
	InitiativeID string   `json:"initiative_id"`
	PlanVersion  uint64   `json:"plan_version"`
	PlanNodeID   string   `json:"plan_node_id"`
	WorkOrderID  string   `json:"work_order_id"`
	GoalID       string   `json:"goal_id"`
	IntentIDs    []string `json:"intent_ids"`
	WakeID       string   `json:"wake_id"`
	Deduplicated bool     `json:"deduplicated"`
}

type Dispatcher struct {
	pool           *pgxpool.Pool
	scheduler      *scheduler.Store
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
}

func NewDispatcher(
	pool *pgxpool.Pool,
	schedulerStore *scheduler.Store,
	tenantID string,
	organizationID contracts.OrganizationID,
	now func() time.Time,
) (*Dispatcher, error) {
	if pool == nil || schedulerStore == nil || tenantID == "" || organizationID == "" || now == nil {
		return nil, fmt.Errorf("initiative: company dispatcher dependencies are required")
	}
	return &Dispatcher{
		pool: pool, scheduler: schedulerStore, tenantID: tenantID,
		organizationID: organizationID, now: now,
	}, nil
}

func (dispatcher *Dispatcher) DispatchReady(
	ctx context.Context,
	plan Plan,
	authority workorder.CompanyAuthority,
	limit uint16,
) ([]DispatchResult, error) {
	if limit == 0 || limit > 1024 || plan.OrganizationID != dispatcher.organizationID {
		return nil, fmt.Errorf("initiative: dispatch limit or organization is invalid")
	}
	if err := VerifyPlan(plan, authority); err != nil {
		return nil, err
	}
	now := dispatcher.now()
	if !validUTC(now) {
		return nil, fmt.Errorf("initiative: dispatcher time source must return UTC")
	}
	nodes := make(map[string]CompiledNode, len(plan.Nodes))
	incoming := make(map[string][]Edge, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.Template.ID] = node
	}
	for _, edge := range plan.Edges {
		incoming[edge.Successor] = append(incoming[edge.Successor], edge)
	}
	results := make([]DispatchResult, 0)
	for _, nodeID := range plan.TopologicalOrder {
		if len(results) >= int(limit) {
			break
		}
		node := nodes[nodeID]
		if node.State != StatePending || node.Template.Kind != NodeWorkOrder || node.Order == nil {
			continue
		}
		ready, scheduledAt, err := dispatcher.ready(ctx, plan, nodes, incoming[nodeID], now)
		if err != nil {
			return nil, err
		}
		if !ready || !scheduledAt.Before(node.Order.Deadline) {
			continue
		}
		result, err := dispatcher.dispatch(ctx, plan, node, authority, scheduledAt, now)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (dispatcher *Dispatcher) ready(
	ctx context.Context,
	plan Plan,
	nodes map[string]CompiledNode,
	edges []Edge,
	now time.Time,
) (bool, time.Time, error) {
	scheduledAt := now
	for _, edge := range edges {
		if edge.Schedule.NotBefore.After(scheduledAt) {
			scheduledAt = edge.Schedule.NotBefore
		}
		outcome, committed, err := dispatcher.nodeOutcome(ctx, plan, nodes, edge.Prerequisite)
		if err != nil {
			return false, time.Time{}, err
		}
		if !committed || edge.When != nil && *edge.When != outcome {
			return false, scheduledAt, nil
		}
	}
	return true, scheduledAt, nil
}

func (dispatcher *Dispatcher) nodeOutcome(
	ctx context.Context,
	plan Plan,
	nodes map[string]CompiledNode,
	nodeID string,
) (GateOutcome, bool, error) {
	node, exists := nodes[nodeID]
	if !exists {
		return "", false, ErrStoreState
	}
	switch node.Template.Kind {
	case NodeWorkOrder:
		var state string
		err := dispatcher.pool.QueryRow(ctx, `
			SELECT graph.state
			FROM workforce_company_work_order_dispatches dispatch
			JOIN workforce_work_nodes graph
			  ON graph.tenant_id=dispatch.tenant_id
			 AND graph.organization_id=dispatch.organization_id
			 AND graph.node_id=dispatch.goal_id
			WHERE dispatch.tenant_id=$1 AND dispatch.organization_id=$2
			  AND dispatch.initiative_id=$3 AND dispatch.plan_version=$4
			  AND dispatch.plan_node_id=$5
		`, dispatcher.tenantID, dispatcher.organizationID, plan.InitiativeID,
			plan.Version, nodeID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("initiative: inspect predecessor dispatch: %w", err)
		}
		switch state {
		case "completed":
			return OutcomeSatisfied, true, nil
		case "failed":
			return OutcomeFailed, true, nil
		case "cancelled":
			return OutcomeExpired, true, nil
		default:
			return "", false, nil
		}
	case NodeOutcomeGate:
		return dispatcher.gateOutcome(ctx, plan, nodeID)
	case NodeBranch:
		if node.Template.Branch == nil {
			return "", false, ErrStoreState
		}
		return dispatcher.nodeOutcome(ctx, plan, nodes, node.Template.Branch.GateNodeID)
	case NodeEvidenceGate:
		if node.Template.Gate == nil {
			return "", false, ErrStoreState
		}
		for _, reference := range node.Template.Gate.Evidence {
			var active bool
			err := dispatcher.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM workforce_company_state_heads head
					JOIN workforce_company_state_records record
					  ON record.tenant_id=head.tenant_id
					 AND record.organization_id=head.organization_id
					 AND record.record_id=head.record_id
					 AND record.version=head.latest_version
					WHERE head.tenant_id=$1 AND head.organization_id=$2
					  AND head.record_id=$3 AND head.latest_version=$4
					  AND head.latest_content_hash=$5 AND head.state='active'
					  AND (head.expires_at IS NULL OR head.expires_at>$6)
					  AND NOT EXISTS (
						SELECT 1 FROM workforce_company_state_contamination contamination
						WHERE contamination.tenant_id=head.tenant_id
						  AND contamination.organization_id=head.organization_id
						  AND contamination.affected_record_id=head.record_id
						  AND contamination.affected_version=head.latest_version
						  AND contamination.state='open' AND contamination.materially_unsafe
					  )
				)
			`, dispatcher.tenantID, dispatcher.organizationID, reference.ID,
				reference.Version, reference.ContentHash.Digest, dispatcher.now()).Scan(&active)
			if err != nil {
				return "", false, fmt.Errorf("initiative: inspect evidence gate: %w", err)
			}
			if !active {
				return "", false, nil
			}
		}
		return OutcomeSatisfied, true, nil
	default:
		return "", false, nil
	}
}

func (dispatcher *Dispatcher) gateOutcome(
	ctx context.Context,
	plan Plan,
	nodeID string,
) (GateOutcome, bool, error) {
	var state string
	err := dispatcher.pool.QueryRow(ctx, `
		SELECT state FROM workforce_company_outcome_gates
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND plan_version=$4 AND node_id=$5
	`, dispatcher.tenantID, dispatcher.organizationID, plan.InitiativeID,
		plan.Version, nodeID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) || state == "open" || state == "contested" {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("initiative: inspect outcome gate: %w", err)
	}
	outcome := GateOutcome(state)
	return outcome, outcome.Valid(), nil
}

func (dispatcher *Dispatcher) dispatch(
	ctx context.Context,
	plan Plan,
	node CompiledNode,
	authority workorder.CompanyAuthority,
	scheduledAt, now time.Time,
) (DispatchResult, error) {
	order := *node.Order
	if err := workorder.VerifyCompany(order, authority); err != nil {
		return DispatchResult{}, err
	}
	goalID := "goal:company:" + order.ID
	wakeID := "wake:company:" + order.ID + ":1"
	intentIDs := make([]string, len(order.Departments))
	for index, department := range order.Departments {
		intentIDs[index] = fmt.Sprintf("intent:company:%s:%02d:%s", order.ID, index+1, department)
	}
	result := DispatchResult{
		InitiativeID: string(plan.InitiativeID), PlanVersion: plan.Version,
		PlanNodeID: node.Template.ID, WorkOrderID: order.ID, GoalID: goalID,
		IntentIDs: slices.Clone(intentIDs), WakeID: wakeID,
	}
	tx, err := dispatcher.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("initiative: begin company dispatch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		dispatcher.tenantID+"|"+string(dispatcher.organizationID)+"|company-dispatch|"+order.ID); err != nil {
		return DispatchResult{}, err
	}
	var existingGoal, existingWake, existingNode string
	err = tx.QueryRow(ctx, `
		SELECT goal_id,wake_id,plan_node_id
		FROM workforce_company_work_order_dispatches
		WHERE tenant_id=$1 AND organization_id=$2 AND work_order_id=$3
	`, dispatcher.tenantID, dispatcher.organizationID, order.ID).Scan(
		&existingGoal, &existingWake, &existingNode,
	)
	if err == nil {
		if existingGoal != goalID || existingWake != wakeID || existingNode != node.Template.ID {
			return DispatchResult{}, ErrStoreConflict
		}
		result.Deduplicated = true
		if err := tx.Commit(ctx); err != nil {
			return DispatchResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DispatchResult{}, err
	}
	var currentPlanID, planState string
	var currentVersion uint64
	if err := tx.QueryRow(ctx, `
		SELECT plan_id,version,state FROM workforce_company_initiative_plan_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 FOR SHARE
	`, dispatcher.tenantID, dispatcher.organizationID, plan.InitiativeID).Scan(
		&currentPlanID, &currentVersion, &planState,
	); err != nil || currentPlanID != plan.ID || currentVersion != plan.Version || planState != "active" {
		return DispatchResult{}, ErrStoreState
	}
	type seatAuthority struct {
		departmentID string
		seatID       string
		mandateID    string
		version      uint64
	}
	authorities := make([]seatAuthority, len(order.Departments))
	for index, department := range order.Departments {
		err := tx.QueryRow(ctx, `
			SELECT seat.department_id,seat.seat_id,seat.mandate_id,seat.mandate_version
			FROM workforce_organization_seats seat
			JOIN workforce_organization_departments department
			  ON department.tenant_id=seat.tenant_id
			 AND department.organization_id=seat.organization_id
			 AND department.department_id=seat.department_id
			WHERE seat.tenant_id=$1 AND seat.organization_id=$2
			  AND department.department_kind=$3 AND seat.seat_role='lead'
			  AND seat.active=TRUE AND department.enabled=TRUE
		`, dispatcher.tenantID, dispatcher.organizationID, department).Scan(
			&authorities[index].departmentID, &authorities[index].seatID,
			&authorities[index].mandateID, &authorities[index].version,
		)
		if err != nil {
			return DispatchResult{}, fmt.Errorf("initiative: resolve company order department lead: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,title,state,base_priority,
			created_at,updated_at,deadline,contested,version
		) VALUES ($1,$2,$3,'goal',$4,'pending',$5,$6,$6,$7,FALSE,1)
	`, dispatcher.tenantID, dispatcher.organizationID, goalID, order.Objective,
		order.Priority, now, order.Deadline); err != nil {
		return DispatchResult{}, err
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
		`, dispatcher.tenantID, dispatcher.organizationID, intentID,
			authorities[index].seatID, authorities[index].departmentID, title,
			state, order.Priority, now, order.Deadline); err != nil {
			return DispatchResult{}, err
		}
		if index > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_work_edges (
					tenant_id,organization_id,prerequisite_node_id,dependent_node_id,
					edge_kind,created_at
				) VALUES ($1,$2,$3,$4,'dependency',$5)
			`, dispatcher.tenantID, dispatcher.organizationID,
				intentIDs[index-1], intentID, now); err != nil {
				return DispatchResult{}, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id,organization_id,prerequisite_node_id,dependent_node_id,
			edge_kind,created_at
		) VALUES ($1,$2,$3,$4,'dependency',$5)
	`, dispatcher.tenantID, dispatcher.organizationID,
		intentIDs[len(intentIDs)-1], goalID, now); err != nil {
		return DispatchResult{}, err
	}
	wake := scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1", WakeID: wakeID,
		ScheduleID: "schedule:company:" + order.ID, TenantID: dispatcher.tenantID,
		OrganizationID: string(dispatcher.organizationID),
		SeatID:         authorities[0].seatID, MandateID: authorities[0].mandateID,
		MandateVersion: authorities[0].version, Trigger: scheduler.TriggerDependency,
		Reason:      "company-controller-signed initiative Work Order",
		ScheduledAt: scheduledAt,
		Budget: scheduler.Budget{
			MaxTasks:           order.Budget.MaxTasks,
			MaxSpendMicrounits: order.Budget.MaxSpendMicrounits,
		},
		Model:          scheduler.ModelBinding{Provider: order.ModelProvider, ModelID: order.ModelID},
		MGS:            scheduler.MGSBinding{Reference: order.MGSReference, Digest: order.MGSDigest},
		IdempotencyKey: "company-dispatch:" + order.IdempotencyKey,
		CoalesceKey:    "company-work-order:" + order.ID, GraphScope: intentIDs[0],
	}
	if _, err := dispatcher.scheduler.EnqueueTx(ctx, tx, wake, now); err != nil {
		return DispatchResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_work_order_dispatches (
			tenant_id,organization_id,work_order_id,initiative_id,plan_version,
			plan_node_id,goal_id,initial_intent_id,wake_id,dispatched_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, dispatcher.tenantID, dispatcher.organizationID, order.ID, plan.InitiativeID,
		plan.Version, node.Template.ID, goalID, intentIDs[0], wakeID, now); err != nil {
		return DispatchResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DispatchResult{}, fmt.Errorf("initiative: commit company dispatch: %w", err)
	}
	return result, nil
}
