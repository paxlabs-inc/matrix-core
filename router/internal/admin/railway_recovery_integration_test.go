package admin

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/router/internal/db"
	"matrix/router/internal/provision"
	"matrix/router/internal/railway"
)

func TestRailwayRecoveryRealProvider(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_POSTGRES_URL")
	token := os.Getenv("ROUTER_TEST_RAILWAY_TOKEN")
	projectID := os.Getenv("ROUTER_TEST_RAILWAY_PROJECT_ID")
	environmentID := os.Getenv("ROUTER_TEST_RAILWAY_ENVIRONMENT_ID")
	image := os.Getenv("ROUTER_TEST_RAILWAY_IMAGE")
	if dsn == "" || token == "" || projectID == "" || environmentID == "" || image == "" {
		t.Skip("real Railway recovery environment is unset")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const (
		userID  = "00000000-0000-0000-0000-000000000221"
		shardID = "test-recovery-shard"
	)
	sqlDB, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(ctx, `INSERT INTO railway_shards(id,project_id,environment_id,router_url,capacity,state)
		VALUES($1,$2,$3,'https://test.invalid',1,'active')
		ON CONFLICT(id) DO UPDATE SET project_id=$2,environment_id=$3,capacity=1,state='active'`,
		shardID, projectID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(ctx, `INSERT INTO users(id,state,provider,railway_shard_id)
		VALUES($1,'provisioning','railway',$2)
		ON CONFLICT(id) DO UPDATE SET state='provisioning',provider='railway',
			env_id=NULL,env_volume_id=NULL,railway_shard_id=$2`, userID, shardID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(ctx, `INSERT INTO railway_allocations(user_id,shard_id,state,operation_key)
		VALUES($1,$2,'provisioning',$3)
		ON CONFLICT(user_id) DO UPDATE SET shard_id=$2,state='provisioning',
			operation_key=$3,released_at=NULL`, userID, shardID, "ensure:"+userID); err != nil {
		t.Fatal(err)
	}

	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := &railway.Provisioner{
		Client:        railway.New(token, projectID, environmentID),
		Image:         image,
		ProbeInterval: time.Second,
	}
	req := provision.CreateRequest{
		UserID: userID,
		Env: map[string]string{
			"MATRIX_USER_ID":  userID,
			"MATRIX_DATA_DIR": "/data",
		},
	}
	service, err := p.FindService(ctx, p.ServiceName(userID))
	if errors.Is(err, provision.ErrNotFound) {
		service, err = p.CreateService(ctx, req)
	}
	if err != nil {
		t.Fatal(err)
	}
	volumeID, err := p.FindVolume(ctx, service.ID, "/data")
	if errors.Is(err, provision.ErrNotFound) {
		volumeID, err = p.CreateVolume(ctx, service.ID, "/data")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = p.DeleteVolume(cleanupCtx, volumeID)
		_ = p.DeleteService(cleanupCtx, service.ID)
		_, _ = sqlDB.Exec(cleanupCtx, `DELETE FROM users WHERE id=$1`, userID)
		_, _ = sqlDB.Exec(cleanupCtx, `DELETE FROM railway_shards WHERE id=$1`, shardID)
	}()

	op, err := store.BeginRailwayOperation(ctx, userID, "ensure")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: store, Provider: "railway"}
	recovered, err := h.ensureRailway(ctx, op, p, req)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != service.ID || recovered.VolumeID != volumeID || !recovered.Ready {
		t.Fatalf("recovery did not reuse provider resources: %+v", recovered)
	}
	if err := store.AttachMachine(ctx, userID, "railway", recovered.ID, recovered.VolumeID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllocationState(ctx, userID, "active", false); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRailwayOperation(ctx, op.ID, "succeeded", "test_ready_and_attached", ""); err != nil {
		t.Fatal(err)
	}

	destroyOp, err := store.BeginRailwayOperation(ctx, userID, "destroy")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.destroyRailway(ctx, destroyOp, p, provision.Ref{
		UserID: userID, EnvID: recovered.ID, VolumeID: recovered.VolumeID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllocationState(ctx, userID, "released", true); err != nil {
		t.Fatal(err)
	}
	capacity, err := store.RailwayCapacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, shard := range capacity {
		if shard.ShardID == shardID && shard.Occupied != 0 {
			t.Fatalf("capacity retained after proven cleanup: %+v", shard)
		}
	}
}
