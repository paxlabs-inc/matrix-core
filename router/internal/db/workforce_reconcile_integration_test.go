package db

import (
	"context"
	"os"
	"testing"
)

func TestListWorkforceReconcileUsersRealPostgres(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_POSTGRES_URL is unset")
	}
	ctx := context.Background()
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const shardID = "test-workforce-reconcile"
	_, err = database.pool.Exec(ctx, `INSERT INTO railway_shards(id,project_id,environment_id,router_url,capacity,state)
		VALUES($1,'workforce-project','workforce-environment','https://test.invalid',10,'active')
		ON CONFLICT(id) DO UPDATE SET capacity=10,state='active'`, shardID)
	if err != nil {
		t.Fatal(err)
	}
	testUsers := []struct {
		id       string
		state    string
		provider string
		envID    string
		shardID  *string
	}{
		{id: "00000000-0000-0000-0000-000000000241", state: StateActive, provider: "railway", envID: "service-active", shardID: stringPointer(shardID)},
		{id: "00000000-0000-0000-0000-000000000242", state: StateProvisioning, provider: "railway", envID: "service-provisioning", shardID: stringPointer(shardID)},
		{id: "00000000-0000-0000-0000-000000000243", state: StateSuspended, provider: "railway", envID: "service-suspended", shardID: stringPointer(shardID)},
		{id: "00000000-0000-0000-0000-000000000244", state: StateActive, provider: "fly", envID: "machine-active", shardID: nil},
		{id: "00000000-0000-0000-0000-000000000245", state: StateActive, provider: "railway", envID: "", shardID: stringPointer(shardID)},
	}
	ids := make([]string, 0, len(testUsers))
	for _, user := range testUsers {
		ids = append(ids, user.id)
		if _, err := database.pool.Exec(ctx, `INSERT INTO users(id,state,provider,env_id,railway_shard_id)
			VALUES($1,$2,$3,NULLIF($4,''),$5)
			ON CONFLICT(id) DO UPDATE SET state=$2,provider=$3,env_id=NULLIF($4,''),railway_shard_id=$5`,
			user.id, user.state, user.provider, user.envID, user.shardID); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = database.pool.Exec(ctx, `DELETE FROM users WHERE id=ANY($1::uuid[])`, ids)
		_, _ = database.pool.Exec(ctx, `DELETE FROM railway_shards WHERE id=$1`, shardID)
	}()
	users, err := database.ListWorkforceReconcileUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{testUsers[0].id: true, testUsers[1].id: true}
	for _, user := range users {
		if wanted[user.ID] {
			delete(wanted, user.ID)
		}
		for _, excluded := range testUsers[2:] {
			if user.ID == excluded.id {
				t.Fatalf("ineligible user returned: %+v", user)
			}
		}
	}
	if len(wanted) != 0 {
		t.Fatalf("eligible users missing: %v", wanted)
	}
}

func stringPointer(value string) *string { return &value }
