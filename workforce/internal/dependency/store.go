package dependency

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
)

// Store owns serializable global-work-graph transactions for one tenant.
type Store struct {
	pool     *pgxpool.Pool
	tenantID string
	now      func() time.Time
}

// New constructs a tenant-scoped graph store.
func New(pool *pgxpool.Pool, tenantID string, now func() time.Time) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	switch {
	case pool == nil:
		return nil, fmt.Errorf("dependency: pool is required")
	case tenantID == "":
		return nil, fmt.Errorf("dependency: tenant_id is required")
	case now == nil:
		return nil, fmt.Errorf("dependency: time source is required")
	default:
		return &Store{pool: pool, tenantID: tenantID, now: now}, nil
	}
}

// PutNode creates a node or idempotently returns an identical current version.
func (store *Store) PutNode(ctx context.Context, node Node) error {
	if err := node.Validate(); err != nil {
		return err
	}
	if _, err := store.currentTime(); err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_work_nodes (
			tenant_id, organization_id, node_id, node_kind, owner_seat_id,
			owner_department_id, title, state, base_priority, created_at,
			updated_at, deadline, contested, cancellation_reason,
			terminal_record_id, version
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),
			$15,$16
		)
		ON CONFLICT (tenant_id, organization_id, node_id) DO NOTHING
	`, store.tenantID, node.OrganizationID, node.ID, node.Kind,
		optionalSeat(node.OwnerSeatID), optionalDepartment(node.OwnerDepartmentID),
		node.Title, node.State, node.BasePriority, node.CreatedAt, node.UpdatedAt,
		node.Deadline, node.Contested, node.CancellationReason,
		optionalRecord(node.TerminalRecordID), node.Version)
	if err != nil {
		return fmt.Errorf("dependency put node: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	existing, err := store.getNode(ctx, node.OrganizationID, node.ID)
	if err != nil {
		return err
	}
	if !sameNode(existing, node) {
		return fmt.Errorf("%w: node %q already exists", ErrConflict, node.ID)
	}
	return nil
}

// AddEdge atomically rejects cycles and inserts a dependency.
func (store *Store) AddEdge(ctx context.Context, edge Edge) error {
	if err := edge.Validate(); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = store.addEdgeOnce(ctx, edge)
		if !retryableTransaction(err) {
			return err
		}
	}
	return fmt.Errorf("dependency add edge: serializable retry budget exhausted: %w", err)
}

func (store *Store) addEdgeOnce(ctx context.Context, edge Edge) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("dependency add edge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		store.tenantID+"|"+string(edge.OrganizationID)); err != nil {
		return fmt.Errorf("dependency add edge: lock graph: %w", err)
	}
	snapshot, err := store.loadTx(ctx, tx, edge.OrganizationID)
	if err != nil {
		return err
	}
	cycle, err := WouldCycle(snapshot, edge.Prerequisite, edge.Dependent)
	if err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("%w: %s -> %s", ErrCycle, edge.Prerequisite, edge.Dependent)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id, organization_id, prerequisite_node_id, dependent_node_id,
			edge_kind, required_response_schema, expires_at, timeout_action,
			sla_at, created_at
		) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,''),$9,$10)
		ON CONFLICT DO NOTHING
	`, store.tenantID, edge.OrganizationID, edge.Prerequisite, edge.Dependent,
		edge.Kind, edge.RequiredResponseSchema, edge.ExpiresAt, edge.TimeoutAction,
		edge.SLAAt, edge.CreatedAt)
	if err != nil {
		return fmt.Errorf("dependency add edge: insert: %w", err)
	}
	if command.RowsAffected() == 0 {
		var count int
		err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_work_edges
			WHERE tenant_id=$1 AND organization_id=$2
			  AND prerequisite_node_id=$3 AND dependent_node_id=$4
			  AND edge_kind=$5
			  AND COALESCE(required_response_schema,'')=$6
			  AND expires_at IS NOT DISTINCT FROM $7
			  AND COALESCE(timeout_action,'')=$8
			  AND sla_at IS NOT DISTINCT FROM $9
			  AND created_at=$10
		`, store.tenantID, edge.OrganizationID, edge.Prerequisite, edge.Dependent,
			edge.Kind, edge.RequiredResponseSchema, edge.ExpiresAt,
			edge.TimeoutAction, edge.SLAAt, edge.CreatedAt).Scan(&count)
		if err != nil {
			return fmt.Errorf("dependency add edge: inspect conflict: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("%w: edge already exists", ErrConflict)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dependency add edge: commit: %w", err)
	}
	return nil
}

// Transition applies one optimistic lifecycle transition and propagates
// cancellation or correction contamination to transitive dependents.
func (store *Store) Transition(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	nodeID NodeID,
	expectedVersion uint64,
	state NodeState,
	reason string,
) error {
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return err
	}
	if err := validateToken("node_id", string(nodeID)); err != nil {
		return err
	}
	if expectedVersion == 0 || !state.Valid() {
		return fmt.Errorf("expected version and target state must be valid")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if state == StateCancelled && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("cancelled transition requires reason")
	}
	if state != StateCancelled && reason != "" {
		return fmt.Errorf("transition reason is reserved for cancellation")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("dependency transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	command, err := tx.Exec(ctx, `
		UPDATE workforce_work_nodes
		SET state=$1, cancellation_reason=NULLIF($2,''), contested=($1='contested'),
			updated_at=$3, version=version+1
		WHERE tenant_id=$4 AND organization_id=$5 AND node_id=$6
		  AND version=$7 AND state NOT IN ('completed','cancelled','failed')
	`, state, reason, now, store.tenantID, organizationID, nodeID, expectedVersion)
	if err != nil {
		return fmt.Errorf("dependency transition: update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: stale version or terminal node", ErrConflict)
	}
	if state == StateCancelled || state == StateContested {
		targetState := StateCancelled
		targetReason := "prerequisite_cancelled"
		if state == StateContested {
			targetState = StateContested
			targetReason = ""
		}
		_, err = tx.Exec(ctx, `
			WITH RECURSIVE affected(node_id) AS (
				VALUES ($1::TEXT)
				UNION
				SELECT edge.dependent_node_id
				FROM workforce_work_edges edge
				JOIN affected ON affected.node_id=edge.prerequisite_node_id
				WHERE edge.tenant_id=$2 AND edge.organization_id=$3
			)
			UPDATE workforce_work_nodes node
			SET state=$4, contested=($4='contested'),
				cancellation_reason=CASE WHEN $4='cancelled' THEN $5 ELSE NULL END,
				updated_at=$6, version=node.version+1
			FROM affected
			WHERE node.tenant_id=$2 AND node.organization_id=$3
			  AND node.node_id=affected.node_id AND node.node_id<>$1
			  AND node.state NOT IN ('completed','cancelled','failed')
		`, nodeID, store.tenantID, organizationID, targetState, targetReason, now)
		if err != nil {
			return fmt.Errorf("dependency transition: propagate: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dependency transition: commit: %w", err)
	}
	return nil
}

// FinishWithReceipt closes one graph node only when the exact lineage receipt
// is already durable for that node. This is the sole completion/failure path;
// a bare lifecycle update cannot manufacture terminal truth.
func (store *Store) FinishWithReceipt(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	nodeID NodeID,
	expectedVersion uint64,
	state NodeState,
	receiptID contracts.ReceiptID,
) error {
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return err
	}
	if err := validateToken("node_id", string(nodeID)); err != nil {
		return err
	}
	if err := validateToken("receipt_id", string(receiptID)); err != nil {
		return err
	}
	if expectedVersion == 0 ||
		state != StateCompleted && state != StateFailed {
		return fmt.Errorf(
			"receipt finish requires a version and completed or failed state",
		)
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("dependency finish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var disposition contracts.WakeDisposition
	err = tx.QueryRow(ctx, `
		SELECT disposition
		FROM workforce_execution_receipts
		WHERE tenant_id=$1 AND organization_id=$2
		  AND receipt_id=$3 AND intent_id=$4
	`, store.tenantID, organizationID, receiptID, nodeID).Scan(&disposition)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: receipt does not bind node", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("dependency finish: inspect receipt: %w", err)
	}
	if state == StateCompleted &&
		disposition != contracts.DispositionProgressed &&
		disposition != contracts.DispositionGoalCompleted ||
		state == StateFailed && disposition != contracts.DispositionFailed {
		return fmt.Errorf("%w: receipt disposition does not bind state", ErrConflict)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_work_nodes
		SET state=$1,terminal_record_id=$2,contested=FALSE,
		    cancellation_reason=NULL,updated_at=$3,version=version+1
		WHERE tenant_id=$4 AND organization_id=$5 AND node_id=$6
		  AND version=$7 AND state NOT IN ('completed','cancelled','failed')
		  AND terminal_record_id IS NULL
	`, state, receiptID, now, store.tenantID, organizationID, nodeID,
		expectedVersion)
	if err != nil {
		return fmt.Errorf("dependency finish: update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: stale version or terminal node", ErrConflict)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dependency finish: commit: %w", err)
	}
	return nil
}

// FinishGoalFromReceipts closes an eligible root goal only after every direct
// prerequisite is completed and carries a terminal receipt. The goal points at
// the lexicographically last prerequisite receipt as its terminal anchor; the
// graph edges retain the complete aggregate proof set.
func (store *Store) FinishGoalFromReceipts(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	goalID NodeID,
	expectedVersion uint64,
) (contracts.ReceiptID, error) {
	if err := validateToken("organization_id", string(organizationID)); err != nil {
		return "", err
	}
	if err := validateToken("goal_id", string(goalID)); err != nil {
		return "", err
	}
	if expectedVersion == 0 {
		return "", fmt.Errorf("dependency goal finish requires a version")
	}
	now, err := store.currentTime()
	if err != nil {
		return "", err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", fmt.Errorf("dependency goal finish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var kind NodeKind
	var state NodeState
	var version uint64
	err = tx.QueryRow(ctx, `
		SELECT node_kind,state,version
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
		FOR UPDATE
	`, store.tenantID, organizationID, goalID).Scan(&kind, &state, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("dependency goal finish: load goal: %w", err)
	}
	if kind != NodeGoal || state != StateEligible || version != expectedVersion {
		return "", fmt.Errorf("%w: goal is not eligible", ErrConflict)
	}
	rows, err := tx.Query(ctx, `
		SELECT prerequisite.state,prerequisite.terminal_record_id
		FROM workforce_work_edges edge
		JOIN workforce_work_nodes prerequisite
		  ON prerequisite.tenant_id=edge.tenant_id
		 AND prerequisite.organization_id=edge.organization_id
		 AND prerequisite.node_id=edge.prerequisite_node_id
		WHERE edge.tenant_id=$1 AND edge.organization_id=$2
		  AND edge.dependent_node_id=$3
		ORDER BY prerequisite.node_id
	`, store.tenantID, organizationID, goalID)
	if err != nil {
		return "", fmt.Errorf(
			"dependency goal finish: load prerequisites: %w", err,
		)
	}
	var receipts []string
	for rows.Next() {
		var prerequisiteState NodeState
		var receipt *string
		if err := rows.Scan(&prerequisiteState, &receipt); err != nil {
			rows.Close()
			return "", err
		}
		if prerequisiteState != StateCompleted || receipt == nil {
			rows.Close()
			return "", fmt.Errorf(
				"%w: goal prerequisite lacks a completed receipt", ErrConflict,
			)
		}
		receipts = append(receipts, *receipt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	if len(receipts) == 0 {
		return "", fmt.Errorf("%w: goal has no receipt prerequisites", ErrConflict)
	}
	sort.Strings(receipts)
	anchor := contracts.ReceiptID(receipts[len(receipts)-1])
	command, err := tx.Exec(ctx, `
		UPDATE workforce_work_nodes
		SET state='completed',terminal_record_id=$1,updated_at=$2,
		    version=version+1
		WHERE tenant_id=$3 AND organization_id=$4 AND node_id=$5
		  AND version=$6 AND state='eligible' AND node_kind='goal'
		  AND terminal_record_id IS NULL
	`, anchor, now, store.tenantID, organizationID, goalID, expectedVersion)
	if err != nil {
		return "", fmt.Errorf("dependency goal finish: update: %w", err)
	}
	if command.RowsAffected() != 1 {
		return "", fmt.Errorf("%w: stale goal version", ErrConflict)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("dependency goal finish: commit: %w", err)
	}
	return anchor, nil
}

// Resolve loads the durable graph, computes deterministic readiness and
// incidents, and persists the new projections atomically.
func (store *Store) Resolve(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) (Projection, error) {
	now, err := store.currentTime()
	if err != nil {
		return Projection{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Projection{}, fmt.Errorf("dependency resolve: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		store.tenantID+"|"+string(organizationID)); err != nil {
		return Projection{}, fmt.Errorf("dependency resolve: lock graph: %w", err)
	}
	snapshot, err := store.loadTx(ctx, tx, organizationID)
	if err != nil {
		return Projection{}, err
	}
	projection, err := Resolve(snapshot, now)
	if err != nil {
		return Projection{}, err
	}
	for _, node := range projection.Nodes {
		if node.State != StateEligible {
			continue
		}
		_, err := tx.Exec(ctx, `
			UPDATE workforce_work_nodes
			SET state='eligible', updated_at=$1, version=version+1
			WHERE tenant_id=$2 AND organization_id=$3 AND node_id=$4
			  AND state='pending' AND contested=FALSE
		`, now, store.tenantID, organizationID, node.ID)
		if err != nil {
			return Projection{}, fmt.Errorf("dependency resolve: persist eligibility: %w", err)
		}
	}
	for _, incident := range projection.Incidents {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_work_incidents (
				tenant_id, organization_id, incident_id, kind,
				node_ids, explanation, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (tenant_id, organization_id, incident_id) DO NOTHING
		`, store.tenantID, organizationID, incident.ID, incident.Kind,
			nodeStrings(incident.NodeIDs), incident.Explanation, incident.CreatedAt)
		if err != nil {
			return Projection{}, fmt.Errorf("dependency resolve: persist incident: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Projection{}, fmt.Errorf("dependency resolve: commit: %w", err)
	}
	return projection, nil
}

// Snapshot returns the durable organization graph without hidden writes.
func (store *Store) Snapshot(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) (Snapshot, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, fmt.Errorf("dependency snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	snapshot, err := store.loadTx(ctx, tx, organizationID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("dependency snapshot: commit: %w", err)
	}
	return snapshot, nil
}

func (store *Store) loadTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
) (Snapshot, error) {
	rows, err := tx.Query(ctx, `
		SELECT node_id,node_kind,owner_seat_id,owner_department_id,title,state,
			base_priority,created_at,updated_at,deadline,contested,
			COALESCE(cancellation_reason,''),terminal_record_id,version
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY node_id
	`, store.tenantID, organizationID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("dependency load nodes: %w", err)
	}
	nodes := make([]Node, 0)
	for rows.Next() {
		node := Node{OrganizationID: organizationID}
		var ownerSeat, ownerDepartment, terminalRecord *string
		if err := rows.Scan(&node.ID, &node.Kind, &ownerSeat, &ownerDepartment,
			&node.Title, &node.State, &node.BasePriority, &node.CreatedAt,
			&node.UpdatedAt, &node.Deadline, &node.Contested,
			&node.CancellationReason, &terminalRecord, &node.Version); err != nil {
			rows.Close()
			return Snapshot{}, fmt.Errorf("dependency scan node: %w", err)
		}
		node.OwnerSeatID = seatPointer(ownerSeat)
		node.OwnerDepartmentID = departmentPointer(ownerDepartment)
		node.TerminalRecordID = recordPointer(terminalRecord)
		normalizeNodeTimes(&node)
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Snapshot{}, fmt.Errorf("dependency iterate nodes: %w", err)
	}
	rows.Close()
	edgeRows, err := tx.Query(ctx, `
		SELECT prerequisite_node_id,dependent_node_id,edge_kind,
			COALESCE(required_response_schema,''),expires_at,
			COALESCE(timeout_action,''),sla_at,created_at
		FROM workforce_work_edges
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY dependent_node_id,prerequisite_node_id,edge_kind
	`, store.tenantID, organizationID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("dependency load edges: %w", err)
	}
	defer edgeRows.Close()
	edges := make([]Edge, 0)
	for edgeRows.Next() {
		edge := Edge{OrganizationID: organizationID}
		if err := edgeRows.Scan(&edge.Prerequisite, &edge.Dependent, &edge.Kind,
			&edge.RequiredResponseSchema, &edge.ExpiresAt, &edge.TimeoutAction,
			&edge.SLAAt, &edge.CreatedAt); err != nil {
			return Snapshot{}, fmt.Errorf("dependency scan edge: %w", err)
		}
		normalizeEdgeTimes(&edge)
		edges = append(edges, edge)
	}
	if err := edgeRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("dependency iterate edges: %w", err)
	}
	return Snapshot{Nodes: nodes, Edges: edges}, nil
}

func (store *Store) getNode(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	nodeID NodeID,
) (Node, error) {
	node := Node{OrganizationID: organizationID}
	var ownerSeat, ownerDepartment, terminalRecord *string
	err := store.pool.QueryRow(ctx, `
		SELECT node_id,node_kind,owner_seat_id,owner_department_id,title,state,
			base_priority,created_at,updated_at,deadline,contested,
			COALESCE(cancellation_reason,''),terminal_record_id,version
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
	`, store.tenantID, organizationID, nodeID).Scan(
		&node.ID, &node.Kind, &ownerSeat, &ownerDepartment, &node.Title,
		&node.State, &node.BasePriority, &node.CreatedAt, &node.UpdatedAt,
		&node.Deadline, &node.Contested, &node.CancellationReason,
		&terminalRecord, &node.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("dependency get node: %w", err)
	}
	node.OwnerSeatID = seatPointer(ownerSeat)
	node.OwnerDepartmentID = departmentPointer(ownerDepartment)
	node.TerminalRecordID = recordPointer(terminalRecord)
	normalizeNodeTimes(&node)
	return node, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("dependency: time source returned non-UTC timestamp")
	}
	return now, nil
}

func sameNode(left, right Node) bool {
	return left.ID == right.ID && left.OrganizationID == right.OrganizationID &&
		left.Kind == right.Kind && pointerEqual(left.OwnerSeatID, right.OwnerSeatID) &&
		pointerEqual(left.OwnerDepartmentID, right.OwnerDepartmentID) &&
		left.Title == right.Title && left.State == right.State &&
		left.BasePriority == right.BasePriority && left.CreatedAt.Equal(right.CreatedAt) &&
		left.UpdatedAt.Equal(right.UpdatedAt) && timePointerEqual(left.Deadline, right.Deadline) &&
		left.Contested == right.Contested &&
		left.CancellationReason == right.CancellationReason &&
		pointerEqual(left.TerminalRecordID, right.TerminalRecordID) &&
		left.Version == right.Version
}

func pointerEqual[T comparable](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func timePointerEqual(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}

func optionalSeat(value *contracts.SeatID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func optionalDepartment(value *contracts.DepartmentID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func optionalRecord(value *contracts.RecordID) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

func seatPointer(value *string) *contracts.SeatID {
	if value == nil {
		return nil
	}
	converted := contracts.SeatID(*value)
	return &converted
}

func departmentPointer(value *string) *contracts.DepartmentID {
	if value == nil {
		return nil
	}
	converted := contracts.DepartmentID(*value)
	return &converted
}

func recordPointer(value *string) *contracts.RecordID {
	if value == nil {
		return nil
	}
	converted := contracts.RecordID(*value)
	return &converted
}

func nodeStrings(ids []NodeID) []string {
	result := make([]string, len(ids))
	for index, id := range ids {
		result[index] = string(id)
	}
	return result
}

func retryableTransaction(err error) bool {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return false
	}
	return databaseError.Code == "40001" || databaseError.Code == "40P01"
}

func normalizeNodeTimes(node *Node) {
	node.CreatedAt = node.CreatedAt.UTC()
	node.UpdatedAt = node.UpdatedAt.UTC()
	if node.Deadline != nil {
		normalized := node.Deadline.UTC()
		node.Deadline = &normalized
	}
}

func normalizeEdgeTimes(edge *Edge) {
	edge.CreatedAt = edge.CreatedAt.UTC()
	if edge.ExpiresAt != nil {
		normalized := edge.ExpiresAt.UTC()
		edge.ExpiresAt = &normalized
	}
	if edge.SLAAt != nil {
		normalized := edge.SLAAt.UTC()
		edge.SLAAt = &normalized
	}
}
