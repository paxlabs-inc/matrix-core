package db

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

func TestReserveRailwayShardFinalSlotRealPostgres(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_POSTGRES_URL is unset")
	}
	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const shard = "test-final-slot"
	_, err = d.pool.Exec(ctx, `INSERT INTO railway_shards(id,project_id,environment_id,router_url,capacity,state)
		VALUES($1,'test-project','test-environment','https://test.invalid',1,'active')
		ON CONFLICT(id) DO UPDATE SET capacity=1,state='active'`, shard)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"00000000-0000-0000-0000-000000000021", "00000000-0000-0000-0000-000000000022"}
	for _, id := range ids {
		if _, err := d.pool.Exec(ctx, `INSERT INTO users(id,state,provider) VALUES($1,'provisioning','railway') ON CONFLICT(id) DO UPDATE SET railway_shard_id=NULL`, id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = d.pool.Exec(ctx, `DELETE FROM users WHERE id=ANY($1::uuid[])`, ids)
		_, _ = d.pool.Exec(ctx, `DELETE FROM railway_shards WHERE id=$1`, shard)
	}()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range ids {
		wg.Add(1)
		go func(userID string) { defer wg.Done(); _, err := d.ReserveRailwayShard(ctx, userID); results <- err }(id)
	}
	wg.Wait()
	close(results)
	var success, exhausted int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrShardCapacityExhausted):
			exhausted++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if success != 1 || exhausted != 1 {
		t.Fatalf("success=%d exhausted=%d", success, exhausted)
	}
}

func TestRailwayOperationJournalRealPostgres(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_POSTGRES_URL is unset")
	}
	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const (
		shard = "test-operation-journal"
		user  = "00000000-0000-0000-0000-000000000031"
	)
	_, err = d.pool.Exec(ctx, `INSERT INTO railway_shards(id,project_id,environment_id,router_url,capacity,state)
		VALUES($1,'project-evidence','environment-evidence','https://test.invalid',1,'active')
		ON CONFLICT(id) DO UPDATE SET project_id='project-evidence',environment_id='environment-evidence',capacity=1,state='active'`, shard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO users(id,state,provider,railway_shard_id)
		VALUES($1,'provisioning','railway',$2)
		ON CONFLICT(id) DO UPDATE SET state='provisioning',provider='railway',railway_shard_id=$2`, user, shard); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO railway_allocations(user_id,shard_id,state,operation_key)
		VALUES($1,$2,'provisioning',$3)
		ON CONFLICT(user_id) DO UPDATE SET shard_id=$2,state='provisioning',operation_key=$3,released_at=NULL`,
		user, shard, "ensure:"+user); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = d.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
		_, _ = d.pool.Exec(ctx, `DELETE FROM railway_shards WHERE id=$1`, shard)
	}()

	op, err := d.BeginRailwayOperation(ctx, user, "ensure")
	if err != nil {
		t.Fatal(err)
	}
	if op.ProjectID != "project-evidence" || op.EnvironmentID != "environment-evidence" || op.State != "intent" {
		t.Fatalf("unexpected operation evidence: %+v", op)
	}
	if err := d.ValidateRailwayOperation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE railway_shards SET project_id='project-drift' WHERE id=$1`, shard); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateRailwayOperation(ctx, op.ID); err == nil {
		t.Fatal("operation remained valid after registered project evidence drifted")
	}
	if _, err := d.pool.Exec(ctx, `UPDATE railway_shards SET project_id='project-evidence' WHERE id=$1`, shard); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkRailwayOperationRunning(ctx, op.ID, "create_service"); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRailwayService(ctx, op.ID, "service-observed"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishRailwayOperation(ctx, op.ID, "unknown", "service_response_lost", "transport closed"); err != nil {
		t.Fatal(err)
	}
	pending, err := d.NonTerminalRailwayOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *RailwayOperation
	for i := range pending {
		if pending[i].ID == op.ID {
			found = &pending[i]
			break
		}
	}
	if found == nil || found.ServiceID != "service-observed" || found.State != "unknown" || found.Attempt != 1 {
		t.Fatalf("non-terminal operation not recoverable: %+v", found)
	}
	if err := d.RecordRailwayVolume(ctx, op.ID, "volume-observed"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishRailwayOperation(ctx, op.ID, "succeeded", "ready_and_attached", ""); err != nil {
		t.Fatal(err)
	}
	pending, err = d.NonTerminalRailwayOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pending {
		if pending[i].ID == op.ID {
			t.Fatal("terminal operation remained in restart recovery set")
		}
	}
}
