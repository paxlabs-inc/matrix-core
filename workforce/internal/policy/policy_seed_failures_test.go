package policy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrationSeedActivationFailsClosedAtEveryDatabaseBoundary(t *testing.T) {
	ctx := context.Background()
	for _, boundary := range []string{"record", "head", "department", "seat", "commit"} {
		t.Run(boundary, func(t *testing.T) {
			store, _, seed, _, _, scope := policyFixture(t)
			functionName := "fail_seed_" + boundary + "_" + scope
			triggerName := "trigger_seed_" + boundary + "_" + scope
			table := "workforce_authority_records"
			clause := "BEFORE INSERT"
			options := ""
			switch boundary {
			case "head":
				table = "workforce_authority_heads"
			case "department":
				table = "workforce_organization_departments"
			case "seat":
				table = "workforce_organization_seats"
			case "commit":
				table = "workforce_organization_seats"
				clause = "AFTER INSERT"
				options = "DEFERRABLE INITIALLY DEFERRED"
			}
			if _, err := policyPool.Exec(ctx, `
				CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
				BEGIN
					RAISE EXCEPTION 'forced seed boundary failure';
				END;
				$$ LANGUAGE plpgsql
			`); err != nil {
				t.Fatal(err)
			}
			triggerSQL := "CREATE "
			if boundary == "commit" {
				triggerSQL += "CONSTRAINT "
			}
			triggerSQL += "TRIGGER " + triggerName + " " + clause + " ON " + table + " " +
				options + " FOR EACH ROW EXECUTE FUNCTION " + functionName + "()"
			if _, err := policyPool.Exec(ctx, triggerSQL); err != nil {
				_, _ = policyPool.Exec(ctx, `DROP FUNCTION `+functionName+`()`)
				t.Fatal(err)
			}
			defer func() {
				_, _ = policyPool.Exec(
					context.Background(), `DROP TRIGGER IF EXISTS `+triggerName+` ON `+table,
				)
				_, _ = policyPool.Exec(
					context.Background(), `DROP FUNCTION IF EXISTS `+functionName+`()`,
				)
			}()
			if _, err := store.PublishSeed(ctx, seed); err == nil {
				t.Fatalf("seed crossed failing %s boundary", boundary)
			}
			var count int
			if err := policyPool.QueryRow(ctx, `
				SELECT COUNT(*) FROM workforce_authority_records
				WHERE tenant_id=$1 AND organization_id=$2
			`, store.root.TenantID, store.root.OrganizationID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("failed %s seed retained %d records", boundary, count)
			}
		})
	}
}

func TestIntegrationSeedActivationReportsLockAndInspectionFailure(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		lockTx, err := policyPool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lockTx.Rollback(context.Background()) }()
		lock := store.root.TenantID + "|" + string(store.root.OrganizationID) + "|seed"
		if _, err := lockTx.Exec(
			context.Background(),
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			lock,
		); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := store.PublishSeed(ctx, seed); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock failure = %v, want deadline", err)
		}
	})

	t.Run("inspection", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		if _, err := policyPool.Exec(
			context.Background(),
			`ALTER TABLE workforce_authority_records RENAME TO workforce_authority_records_unavailable`,
		); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = policyPool.Exec(
				context.Background(),
				`ALTER TABLE workforce_authority_records_unavailable RENAME TO workforce_authority_records`,
			)
		}()
		if _, err := store.PublishSeed(context.Background(), seed); err == nil {
			t.Fatal("seed activation ignored missing authority records")
		}
	})
}

func TestIntegrationRuntimeKindInvalidatesBothLeaseFamilies(t *testing.T) {
	store, _, _, _, _, _ := policyFixture(t)
	tx, err := policyPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.invalidateLeases(
		context.Background(), tx, KindRuntime, "runtime-authority:test", 1,
		"runtime authority changed", policyNow(),
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}
