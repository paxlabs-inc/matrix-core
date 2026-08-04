package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrShardCapacityExhausted = errors.New("shard_capacity_exhausted")

type Shard struct {
	ID, ProjectID, EnvironmentID, RouterURL, State string
	Capacity                                       int
}

type Allocation struct {
	UserID, ShardID, State, OperationKey string
}

type ShardCapacity struct {
	ShardID            string
	Capacity, Occupied int
}

type RailwayOperation struct {
	ID                                    int64
	OperationKey, UserID, ShardID         string
	ProjectID, EnvironmentID, Kind, State string
	ServiceID, VolumeID, LastError        string
	Attempt                               int
	Evidence                              json.RawMessage
	UpdatedAt                             time.Time
}

func (d *DB) BeginRailwayOperation(ctx context.Context, userID, kind string) (*RailwayOperation, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var shardID, projectID, environmentID, allocationKey string
	err = tx.QueryRow(ctx, `SELECT a.shard_id,s.project_id,s.environment_id,a.operation_key
		FROM railway_allocations a JOIN railway_shards s ON s.id=a.shard_id
		JOIN users u ON u.id=a.user_id
		WHERE a.user_id=$1 AND a.state<>'released' AND u.railway_shard_id=a.shard_id
		FOR UPDATE OF a`, userID).Scan(&shardID, &projectID, &environmentID, &allocationKey)
	if err != nil {
		return nil, fmt.Errorf("operation assignment: %w", err)
	}
	key := kind + ":" + userID
	if kind == "ensure" {
		key = allocationKey
	}
	_, err = tx.Exec(ctx, `INSERT INTO railway_operations(
			operation_key,user_id,shard_id,project_id,environment_id,kind,state)
		VALUES($1,$2,$3,$4,$5,$6,'intent') ON CONFLICT(operation_key) DO NOTHING`,
		key, userID, shardID, projectID, environmentID, kind)
	if err != nil {
		return nil, fmt.Errorf("begin operation: %w", err)
	}
	op, err := scanRailwayOperation(tx.QueryRow(ctx, railwayOperationSelect+` WHERE operation_key=$1 FOR UPDATE`, key))
	if err != nil {
		return nil, err
	}
	if op.UserID != userID || op.ShardID != shardID || op.ProjectID != projectID || op.EnvironmentID != environmentID || op.Kind != kind {
		return nil, errors.New("operation evidence does not match assigned shard")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return op, nil
}

const railwayOperationSelect = `SELECT id,operation_key,user_id::text,shard_id,project_id,environment_id,
	kind,state,COALESCE(service_id,''),COALESCE(volume_id,''),attempt,evidence,
	COALESCE(last_error,''),updated_at FROM railway_operations`

type rowScanner interface{ Scan(...any) error }

func scanRailwayOperation(row rowScanner) (*RailwayOperation, error) {
	var op RailwayOperation
	err := row.Scan(&op.ID, &op.OperationKey, &op.UserID, &op.ShardID, &op.ProjectID,
		&op.EnvironmentID, &op.Kind, &op.State, &op.ServiceID, &op.VolumeID,
		&op.Attempt, &op.Evidence, &op.LastError, &op.UpdatedAt)
	return &op, err
}

func (d *DB) MarkRailwayOperationRunning(ctx context.Context, id int64, phase string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE railway_operations SET state='running',attempt=attempt+1,
		evidence=evidence||jsonb_build_object('phase',$2::text,'attempt_started_at',now()),
		last_error=NULL,updated_at=now() WHERE id=$1 AND state NOT IN ('succeeded','failed')`, id, phase)
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("mark operation running: %w", err)
	}
	return nil
}

func (d *DB) RecordRailwayService(ctx context.Context, id int64, serviceID string) error {
	return d.recordRailwayResource(ctx, id, "service_id", serviceID, "service_observed_at")
}

func (d *DB) RecordRailwayVolume(ctx context.Context, id int64, volumeID string) error {
	return d.recordRailwayResource(ctx, id, "volume_id", volumeID, "volume_observed_at")
}

func (d *DB) recordRailwayResource(ctx context.Context, id int64, column, value, evidenceKey string) error {
	q := `UPDATE railway_operations SET ` + column + `=$2,evidence=evidence||jsonb_build_object('` + evidenceKey + `',now()),updated_at=now() WHERE id=$1`
	tag, err := d.pool.Exec(ctx, q, id, value)
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("record railway resource: %w", err)
	}
	return nil
}

func (d *DB) FinishRailwayOperation(ctx context.Context, id int64, state, phase, lastError string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE railway_operations SET state=$2,
		evidence=evidence||jsonb_build_object('phase',$3::text,'observed_at',now()),
		last_error=NULLIF($4::text,''),reconciled_at=CASE WHEN $2::text IN ('succeeded','failed') THEN now() ELSE reconciled_at END,
		updated_at=now() WHERE id=$1`, id, state, phase, lastError)
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("finish railway operation: %w", err)
	}
	return nil
}

func (d *DB) NonTerminalRailwayOperations(ctx context.Context) ([]RailwayOperation, error) {
	rows, err := d.pool.Query(ctx, railwayOperationSelect+`
		WHERE state IN ('intent','running','unknown','cleanup_pending') ORDER BY updated_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RailwayOperation
	for rows.Next() {
		op, err := scanRailwayOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *op)
	}
	return out, rows.Err()
}

func (d *DB) ValidateRailwayOperation(ctx context.Context, id int64) error {
	var valid bool
	err := d.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM railway_operations o
		JOIN railway_allocations a ON a.user_id=o.user_id AND a.shard_id=o.shard_id AND a.state<>'released'
		JOIN railway_shards s ON s.id=o.shard_id
		JOIN users u ON u.id=o.user_id AND u.railway_shard_id=o.shard_id
		WHERE o.id=$1 AND o.project_id=s.project_id AND o.environment_id=s.environment_id
	)`, id).Scan(&valid)
	if err != nil {
		return fmt.Errorf("validate operation evidence: %w", err)
	}
	if !valid {
		return errors.New("operation evidence no longer matches assigned shard")
	}
	return nil
}

func (d *DB) ReserveRailwayShard(ctx context.Context, userID string) (*Allocation, error) {
	for attempt := 0; attempt < 4; attempt++ {
		allocation, err := d.reserveRailwayShard(ctx, userID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
			return allocation, err
		}
	}
	return nil, errors.New("reserve shard: serialization retry exhausted")
}

func (d *DB) reserveRailwayShard(ctx context.Context, userID string) (*Allocation, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("reserve shard: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var a Allocation
	err = tx.QueryRow(ctx, `SELECT user_id, shard_id, state, operation_key FROM railway_allocations WHERE user_id=$1 AND state<>'released'`, userID).
		Scan(&a.UserID, &a.ShardID, &a.State, &a.OperationKey)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("reserve existing: %w", err)
	}
	var shardID string
	err = tx.QueryRow(ctx, `
		SELECT s.id
		FROM railway_shards s
		WHERE s.state='active'
		  AND (SELECT count(*) FROM railway_allocations a WHERE a.shard_id=s.id AND a.state<>'released') < s.capacity
		ORDER BY s.id
		FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&shardID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShardCapacityExhausted
	}
	if err != nil {
		return nil, fmt.Errorf("select shard: %w", err)
	}
	a = Allocation{UserID: userID, ShardID: shardID, State: "reserved", OperationKey: "ensure:" + userID}
	_, err = tx.Exec(ctx, `INSERT INTO railway_allocations(user_id,shard_id,state,operation_key) VALUES($1,$2,$3,$4)`, a.UserID, a.ShardID, a.State, a.OperationKey)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE users SET railway_shard_id=$2, updated_at=now() WHERE id=$1 AND railway_shard_id IS NULL`, userID, shardID)
	}
	if err != nil {
		return nil, fmt.Errorf("bind shard: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit shard: %w", err)
	}
	return &a, nil
}

func (d *DB) Shard(ctx context.Context, id string) (*Shard, error) {
	var s Shard
	err := d.pool.QueryRow(ctx, `SELECT id,project_id,environment_id,router_url,capacity,state FROM railway_shards WHERE id=$1`, id).
		Scan(&s.ID, &s.ProjectID, &s.EnvironmentID, &s.RouterURL, &s.Capacity, &s.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup shard: %w", err)
	}
	return &s, nil
}

func (d *DB) RailwayShards(ctx context.Context) ([]Shard, error) {
	rows, err := d.pool.Query(ctx, `SELECT id,project_id,environment_id,router_url,capacity,state FROM railway_shards ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	defer rows.Close()
	var out []Shard
	for rows.Next() {
		var s Shard
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.EnvironmentID, &s.RouterURL, &s.Capacity, &s.State); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) SetAllocationState(ctx context.Context, userID, state string, released bool) error {
	q := `UPDATE railway_allocations SET state=$2, updated_at=now(), released_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE user_id=$1`
	tag, err := d.pool.Exec(ctx, q, userID, state, released)
	if err != nil {
		return fmt.Errorf("allocation state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrUserNotFound
	}
	return nil
}

func (d *DB) SetShardState(ctx context.Context, shardID, state string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE railway_shards SET state=$2,updated_at=now() WHERE id=$1`, shardID, state)
	if err != nil {
		return fmt.Errorf("shard state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrUserNotFound
	}
	return nil
}

func (d *DB) RailwayCapacity(ctx context.Context) ([]ShardCapacity, error) {
	rows, err := d.pool.Query(ctx, `SELECT s.id,s.capacity,count(a.user_id)::int
		FROM railway_shards s LEFT JOIN railway_allocations a ON a.shard_id=s.id AND a.state<>'released'
		GROUP BY s.id,s.capacity ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("capacity: %w", err)
	}
	defer rows.Close()
	var out []ShardCapacity
	for rows.Next() {
		var c ShardCapacity
		if err := rows.Scan(&c.ShardID, &c.Capacity, &c.Occupied); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) ClaimIngressReplay(ctx context.Context, shardID, replayID string, expiresAt time.Time) (bool, error) {
	tag, err := d.pool.Exec(ctx, `INSERT INTO railway_ingress_replays(shard_id,replay_id,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, shardID, replayID, expiresAt)
	if err != nil {
		return false, fmt.Errorf("claim replay: %w", err)
	}
	_, _ = d.pool.Exec(ctx, `DELETE FROM railway_ingress_replays WHERE expires_at < now()`)
	return tag.RowsAffected() == 1, nil
}
