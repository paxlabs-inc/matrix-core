package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

type WorkOrderBudget = workorder.Budget
type WorkOrder = workorder.Order

// SignWorkOrder signs the canonical Work Order payload.
func SignWorkOrder(order *WorkOrder, keyID string, privateKey ed25519.PrivateKey) error {
	return workorder.Sign(order, keyID, privateKey)
}

func verifyWorkOrder(order WorkOrder, key OwnerKey) error {
	return workorder.Verify(order, key.KeyID, key.PublicKey)
}

// WorkOrderResult identifies the committed goal, intents, and first queued wake.
type WorkOrderResult struct {
	SchemaVersion string   `json:"schema_version"`
	WorkOrderID   string   `json:"work_order_id"`
	GoalID        string   `json:"goal_id"`
	IntentIDs     []string `json:"intent_ids"`
	WakeID        string   `json:"wake_id"`
	Deduplicated  bool     `json:"deduplicated"`
	EventCursor   uint64   `json:"event_cursor"`
}

// CreateWorkOrder verifies, compiles, and atomically persists one owner request.
func (service *Service) CreateWorkOrder(
	ctx context.Context,
	principal Principal,
	order WorkOrder,
) (WorkOrderResult, error) {
	if service.vault == nil || service.vault.User() != principal.TenantID ||
		service.scheduler == nil ||
		order.OrganizationID != principal.OrganizationID ||
		order.OwnerID != principal.OwnerID {
		return WorkOrderResult{}, ErrUnauthorized
	}
	key, err := service.commandKey(ctx, principal, order.Signature.KeyID)
	if err != nil || verifyWorkOrder(order, key) != nil {
		return WorkOrderResult{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return WorkOrderResult{}, err
	}
	if order.CreatedAt.After(now.Add(5*time.Minute)) ||
		order.CreatedAt.Before(now.Add(-15*time.Minute)) ||
		!order.Deadline.After(now) {
		return WorkOrderResult{}, fmt.Errorf("controlapi: work order time is outside the acceptance window")
	}
	var companyState string
	var issuerRevokedAt *time.Time
	err = service.pool.QueryRow(ctx, `
		SELECT state,issuer_revoked_at
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, principal.TenantID, principal.OrganizationID).Scan(
		&companyState, &issuerRevokedAt,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkOrderResult{}, err
	}
	if err == nil && (companyState != "active" || issuerRevokedAt != nil) {
		return WorkOrderResult{}, fmt.Errorf("controlapi: company initiation is paused")
	}
	if service.runtimeModelProvider == "" || service.runtimeModelID == "" {
		return WorkOrderResult{}, fmt.Errorf(
			"controlapi: executable runtime model is unavailable",
		)
	}
	if order.ModelProvider != service.runtimeModelProvider ||
		order.ModelID != service.runtimeModelID {
		return WorkOrderResult{}, fmt.Errorf(
			"controlapi: work order model does not match the executable runtime",
		)
	}
	canonical, err := contracts.EncodeCanonical(&order)
	if err != nil {
		return WorkOrderResult{}, err
	}
	sum := sha256.Sum256(canonical)
	orderHash := hex.EncodeToString(sum[:])
	sealedOrder, err := service.vault.SealRecord(vault.AD{
		User: principal.TenantID, Store: "workforce.work-order",
		Stream: string(principal.OrganizationID) + ":" + order.ID,
		Schema: order.SchemaVersion,
	}, canonical)
	if err != nil {
		return WorkOrderResult{}, fmt.Errorf("controlapi: seal work order: %w", err)
	}
	goalID := "goal:" + order.ID
	intentIDs := make([]string, len(order.Departments))
	wakeID := "wake:" + order.ID + ":1"
	scheduleID := "schedule:" + order.ID
	for index, department := range order.Departments {
		intentIDs[index] = fmt.Sprintf("intent:%s:%02d:%s", order.ID, index+1, department)
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return WorkOrderResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		principal.TenantID+"|"+string(principal.OrganizationID)+"|"+order.IdempotencyKey,
	); err != nil {
		return WorkOrderResult{}, err
	}
	var existingID, existingHash, existingGoal, existingWake string
	var existingCursor uint64
	err = tx.QueryRow(ctx, `
		SELECT work_order_id,canonical_hash,goal_id,wake_id,event_cursor
		FROM workforce_work_orders
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, principal.TenantID, principal.OrganizationID, order.IdempotencyKey).Scan(
		&existingID, &existingHash, &existingGoal, &existingWake, &existingCursor,
	)
	if err == nil {
		if existingID != order.ID || existingHash != orderHash {
			return WorkOrderResult{}, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkOrderResult{}, err
		}
		return WorkOrderResult{
			SchemaVersion: SchemaVersion, WorkOrderID: existingID,
			GoalID: existingGoal, IntentIDs: intentIDs, WakeID: existingWake,
			Deduplicated: true, EventCursor: existingCursor,
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WorkOrderResult{}, err
	}
	var departmentCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_organization_departments
		WHERE tenant_id=$1 AND organization_id=$2 AND enabled=TRUE
	`, principal.TenantID, principal.OrganizationID).Scan(&departmentCount); err != nil {
		return WorkOrderResult{}, err
	}
	if departmentCount != 7 {
		return WorkOrderResult{}, fmt.Errorf("controlapi: organization is not activated")
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
		`, principal.TenantID, principal.OrganizationID, department).Scan(
			&authorities[index].departmentID, &authorities[index].seatID,
			&authorities[index].mandateID, &authorities[index].version,
		)
		if err != nil {
			return WorkOrderResult{}, fmt.Errorf("controlapi: resolve department lead: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,title,state,
			base_priority,created_at,updated_at,deadline,contested,version
		) VALUES ($1,$2,$3,'goal',$4,'pending',$5,$6,$6,$7,FALSE,1)
	`, principal.TenantID, principal.OrganizationID, goalID, order.Objective,
		order.Priority, now, order.Deadline); err != nil {
		return WorkOrderResult{}, err
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
				owner_department_id,title,state,base_priority,created_at,
				updated_at,deadline,contested,version
			) VALUES ($1,$2,$3,'intent',$4,$5,$6,$7,$8,$9,$9,$10,FALSE,1)
		`, principal.TenantID, principal.OrganizationID, intentID,
			authorities[index].seatID, authorities[index].departmentID,
			title, state, order.Priority, now, order.Deadline); err != nil {
			return WorkOrderResult{}, err
		}
		if index > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_work_edges (
					tenant_id,organization_id,prerequisite_node_id,
					dependent_node_id,edge_kind,created_at
				) VALUES ($1,$2,$3,$4,'dependency',$5)
			`, principal.TenantID, principal.OrganizationID,
				intentIDs[index-1], intentID, now); err != nil {
				return WorkOrderResult{}, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id,organization_id,prerequisite_node_id,
			dependent_node_id,edge_kind,created_at
		) VALUES ($1,$2,$3,$4,'dependency',$5)
	`, principal.TenantID, principal.OrganizationID,
		intentIDs[len(intentIDs)-1], goalID, now); err != nil {
		return WorkOrderResult{}, err
	}
	wake := scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1", WakeID: wakeID,
		ScheduleID: scheduleID, TenantID: principal.TenantID,
		OrganizationID: string(principal.OrganizationID),
		SeatID:         authorities[0].seatID, MandateID: authorities[0].mandateID,
		MandateVersion: authorities[0].version, Trigger: scheduler.TriggerOnce,
		Reason: "owner-signed work order", ScheduledAt: now,
		Budget: scheduler.Budget{
			MaxTasks:           order.Budget.MaxTasks,
			MaxSpendMicrounits: order.Budget.MaxSpendMicrounits,
		},
		Model: scheduler.ModelBinding{
			Provider: order.ModelProvider, ModelID: order.ModelID,
		},
		MGS: scheduler.MGSBinding{
			Reference: order.MGSReference,
			Digest:    order.MGSDigest,
		},
		IdempotencyKey: "wake:" + order.IdempotencyKey,
		CoalesceKey:    "work-order:" + order.ID,
		GraphScope:     intentIDs[0],
	}
	if err := wake.Validate(); err != nil {
		return WorkOrderResult{}, err
	}
	if _, err := service.scheduler.EnqueueTx(ctx, tx, wake, now); err != nil {
		return WorkOrderResult{}, err
	}
	event := LifecycleEvent{
		SchemaVersion: SchemaVersion, ID: "event:work-order:created:" + order.ID,
		OrganizationID: principal.OrganizationID, Type: "work_order.queued",
		ResourceKind: "work-order", ResourceID: order.ID, ResourceVersion: 1,
		Fields: map[string]any{
			"state": "queued", "goal_id": goalID, "wake_id": wakeID,
		},
		CreatedAt: now,
	}
	if err := validateEvent(event); err != nil {
		return WorkOrderResult{}, err
	}
	fields, _ := json.Marshal(event.Fields)
	if err := tx.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,
			resource_id,resource_version,verified_completion,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,1,FALSE,$7,$8) RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, fields, now).Scan(&event.Cursor); err != nil {
		return WorkOrderResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_orders (
			tenant_id,organization_id,work_order_id,owner_id,version,
			idempotency_key,canonical_hash,sealed_order,goal_id,wake_id,
			event_cursor,created_at
		) VALUES ($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11)
	`, principal.TenantID, principal.OrganizationID, order.ID, principal.OwnerID,
		order.IdempotencyKey, orderHash, sealedOrder, goalID, wakeID,
		event.Cursor, now); err != nil {
		return WorkOrderResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkOrderResult{}, err
	}
	service.broker.publish(topic(principal), event)
	return WorkOrderResult{
		SchemaVersion: SchemaVersion, WorkOrderID: order.ID, GoalID: goalID,
		IntentIDs: intentIDs, WakeID: wakeID, EventCursor: event.Cursor,
	}, nil
}
