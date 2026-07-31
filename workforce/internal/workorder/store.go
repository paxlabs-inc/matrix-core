package workorder

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
)

var (
	ErrNotFound  = errors.New("workorder: execution context not found")
	ErrIntegrity = errors.New("workorder: integrity failure")
)

type Store struct {
	pool      *pgxpool.Pool
	vault     *vault.UserVault
	tenantID  string
	keyID     string
	publicKey ed25519.PublicKey
}

type Context struct {
	Order  Order
	Goal   contracts.Goal
	Intent contracts.Intent
	Node   dependency.Node
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID, keyID string,
	publicKey ed25519.PublicKey,
) (*Store, error) {
	if pool == nil || userVault == nil || strings.TrimSpace(tenantID) == "" ||
		strings.TrimSpace(keyID) == "" ||
		len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("workorder: durable store and owner key are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("workorder: Vault user does not match tenant")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, keyID: keyID,
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
	}, nil
}

func (store *Store) LoadContext(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	intentID contracts.IntentID,
) (Context, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Context{}, fmt.Errorf("workorder: begin context read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var sealed []byte
	var expectedHash, orderID, goalID string
	err = tx.QueryRow(ctx, `
		WITH RECURSIVE successors(node_id) AS (
			VALUES ($3::TEXT)
			UNION
			SELECT edge.dependent_node_id
			FROM workforce_work_edges edge
			JOIN successors ON successors.node_id=edge.prerequisite_node_id
			WHERE edge.tenant_id=$1 AND edge.organization_id=$2
		)
		SELECT order_record.sealed_order,order_record.canonical_hash,
		       order_record.work_order_id,order_record.goal_id
		FROM successors
		JOIN workforce_work_orders order_record
		  ON order_record.tenant_id=$1
		 AND order_record.organization_id=$2
		 AND order_record.goal_id=successors.node_id
		LIMIT 1
	`, store.tenantID, organizationID, intentID).Scan(
		&sealed, &expectedHash, &orderID, &goalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, ErrNotFound
	}
	if err != nil {
		return Context{}, fmt.Errorf("workorder: locate context: %w", err)
	}
	node := dependency.Node{
		ID: dependency.NodeID(intentID), OrganizationID: organizationID,
	}
	var ownerSeat, ownerDepartment, terminalRecord *string
	err = tx.QueryRow(ctx, `
		SELECT node_kind,owner_seat_id,owner_department_id,title,state,
		       base_priority,created_at,updated_at,deadline,contested,
		       COALESCE(cancellation_reason,''),terminal_record_id,version
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
	`, store.tenantID, organizationID, intentID).Scan(
		&node.Kind, &ownerSeat, &ownerDepartment, &node.Title, &node.State,
		&node.BasePriority, &node.CreatedAt, &node.UpdatedAt, &node.Deadline,
		&node.Contested, &node.CancellationReason, &terminalRecord,
		&node.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, ErrNotFound
	}
	if err != nil {
		return Context{}, fmt.Errorf("workorder: load intent node: %w", err)
	}
	node.CreatedAt = node.CreatedAt.UTC()
	node.UpdatedAt = node.UpdatedAt.UTC()
	if node.Deadline != nil {
		value := node.Deadline.UTC()
		node.Deadline = &value
	}
	if ownerSeat != nil {
		value := contracts.SeatID(*ownerSeat)
		node.OwnerSeatID = &value
	}
	if ownerDepartment != nil {
		value := contracts.DepartmentID(*ownerDepartment)
		node.OwnerDepartmentID = &value
	}
	if terminalRecord != nil {
		value := contracts.RecordID(*terminalRecord)
		node.TerminalRecordID = &value
	}
	if err := tx.Commit(ctx); err != nil {
		return Context{}, fmt.Errorf("workorder: commit context read: %w", err)
	}
	canonical, err := store.vault.OpenRecord(vault.AD{
		User: store.tenantID, Store: "workforce.work-order",
		Stream: string(organizationID) + ":" + orderID,
		Schema: "workforce.work-order.v1",
	}, sealed)
	if err != nil {
		return Context{}, ErrIntegrity
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return Context{}, ErrIntegrity
	}
	order, err := contracts.DecodeCanonical[Order, *Order](canonical)
	if err != nil {
		return Context{}, ErrIntegrity
	}
	verificationKey, err := store.verificationKey(ctx, order)
	if err != nil ||
		Verify(order, order.Signature.KeyID, verificationKey) != nil ||
		order.ID != orderID || order.OrganizationID != organizationID ||
		node.Kind != dependency.NodeIntent || node.OwnerSeatID == nil {
		return Context{}, ErrIntegrity
	}
	deadline := node.Deadline
	result := Context{
		Order: order, Node: node,
		Goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.GoalID(goalID), OrganizationID: organizationID,
			WorkOrderID:     contracts.WorkOrderID(order.ID),
			Title:           order.Objective,
			SuccessCriteria: append([]string(nil), order.AcceptanceCriteria...),
			CreatedAt:       order.CreatedAt,
		},
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            intentID, OrganizationID: organizationID,
			GoalID: contracts.GoalID(goalID), OwnerSeatID: *node.OwnerSeatID,
			Summary: node.Title, Priority: node.BasePriority,
			CreatedAt: node.CreatedAt, Deadline: deadline,
		},
	}
	if result.Goal.Validate() != nil || result.Intent.Validate() != nil ||
		node.Validate() != nil {
		return Context{}, ErrIntegrity
	}
	return result, nil
}

func (store *Store) verificationKey(
	ctx context.Context,
	order Order,
) (ed25519.PublicKey, error) {
	if order.Signature.KeyID == store.keyID {
		return append(ed25519.PublicKey(nil), store.publicKey...), nil
	}
	var publicKey []byte
	var ownerID contracts.OwnerID
	err := store.pool.QueryRow(ctx, `
		SELECT owner_id,public_key
		FROM workforce_owner_control_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND key_id=$3
		  AND revoked_at IS NULL
	`, store.tenantID, order.OrganizationID, order.Signature.KeyID).Scan(
		&ownerID, &publicKey,
	)
	if err != nil || ownerID != order.OwnerID ||
		len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrIntegrity
	}
	return ed25519.PublicKey(publicKey), nil
}
