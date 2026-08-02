package policy

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
)

func TestPolicyClosedAndIncompleteDatabasesFailClosed(t *testing.T) {
	ctx := context.Background()
	store, privateKey, _, writeGrant, revokeGrant, scope := policyFixture(t)
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	closedPool, err := pgxpool.New(ctx, policyPool.Config().ConnString())
	if err != nil {
		t.Fatalf("construct closed pool: %v", err)
	}
	closedPool.Close()
	closed := *store
	closed.pool = closedPool
	if err := closed.PublishPolicy(ctx, policy, writeGrant); err == nil {
		t.Fatal("published authority through closed pool")
	}
	if _, err := closed.PublishSeed(ctx, seedForClosedStore(t, store, privateKey)); err == nil {
		t.Fatal("published seed through closed pool")
	}
	if err := closed.AuthorizeLease(ctx, "lease"); err == nil {
		t.Fatal("authorized lease through closed pool")
	}
	if _, err := closed.LoadPolicy(ctx, policy.ID, policy.Version); err == nil {
		t.Fatal("loaded authority through closed pool")
	}
	if _, err := closed.LoadCurrentPolicyRefs(ctx); err == nil {
		t.Fatal("listed policies through closed pool")
	}
	if _, err := closed.LoadCurrentRuntimeAuthority(ctx, "runtime"); err == nil {
		t.Fatal("loaded current runtime authority through closed pool")
	}
	if _, err := closed.LoadMandate(ctx, "mandate", 1); err == nil {
		t.Fatal("loaded mandate through closed pool")
	}
	if _, err := closed.LoadSeat(ctx, "seat", 1); err == nil {
		t.Fatal("loaded seat through closed pool")
	}
	if _, err := closed.LoadCurrentSeat(ctx, "seat"); err == nil {
		t.Fatal("loaded current seat through closed pool")
	}
	if _, err := closed.LoadLease(ctx, "lease"); err == nil {
		t.Fatal("loaded lease through closed pool")
	}
	if err := closed.requireCurrent(
		ctx, KindPolicy, "policy", 1, policyNow(),
	); err == nil {
		t.Fatal("accepted current authority through closed pool")
	}
	if err := closed.requirePolicyReference(
		ctx,
		contracts.PolicyRef{
			ID: "policy", Version: 1,
			Hash: contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)},
		},
		policyNow(),
	); err == nil {
		t.Fatal("accepted policy reference through closed pool")
	}
	revocation := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: string(policy.ID), Version: 1,
		OwnerID: store.root.OwnerID, Reason: "closed pool",
		RevokedAt: policyNow(),
	}
	if err := SignRevocation(&revocation, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := closed.Revoke(ctx, revocation, revokeGrant); err == nil {
		t.Fatal("revoked authority through closed pool")
	}

	schema := strings.ReplaceAll("empty_"+scope, "-", "_")
	if _, err := policyPool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create incomplete schema: %v", err)
	}
	defer func() {
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()
	config, err := pgxpool.ParseConfig(policyPool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	incompletePool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("construct incomplete-schema pool: %v", err)
	}
	defer incompletePool.Close()
	incomplete := *store
	incomplete.pool = incompletePool
	if err := incomplete.PublishPolicy(ctx, policy, writeGrant); err == nil {
		t.Fatal("published authority without authority tables")
	}
}

func seedForClosedStore(
	t *testing.T,
	store *Store,
	privateKey ed25519.PrivateKey,
) Seed {
	t.Helper()
	seed, err := BuildSeed(
		store.root.OrganizationID,
		store.root.OwnerID,
		"Closed store",
		policyNow(),
		store.root.KeyID,
		privateKey,
	)
	if err != nil {
		t.Fatalf("build closed-store seed: %v", err)
	}
	return seed
}

func TestPolicyEntryPointsRejectInvalidClock(t *testing.T) {
	store, privateKey, seed, writeGrant, revokeGrant, scope := policyFixture(t)
	store.now = func() time.Time {
		return policyNow().In(time.FixedZone("test", 3600))
	}
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishPolicy(context.Background(), policy, writeGrant); err == nil {
		t.Fatal("PublishPolicy accepted invalid clock")
	}
	lease := validLease(
		scope,
		store.root.OrganizationID,
		seed.Organization.Departments[0].Seats[0],
		seed.Mandates[0],
		policy,
		canonicalHash(t, &policy),
	)
	if err := store.RegisterLease(context.Background(), lease); err == nil {
		t.Fatal("RegisterLease accepted invalid clock")
	}
	if err := store.AuthorizeLease(context.Background(), lease.ID); err == nil {
		t.Fatal("AuthorizeLease accepted invalid clock")
	}
	revocation := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: string(policy.ID), Version: 1,
		OwnerID: store.root.OwnerID, Reason: "invalid clock",
		RevokedAt: policyNow(),
	}
	if err := SignRevocation(&revocation, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := store.Revoke(context.Background(), revocation, revokeGrant); err == nil {
		t.Fatal("Revoke accepted invalid clock")
	}
}

func TestIntegration_LatePolicyDatabaseFailuresRollBack(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _, scope := policyFixture(t)
	tx, err := policyPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.invalidateLeases(
		canceled,
		tx,
		KindPolicy,
		"policy-"+scope,
		2,
		"canceled",
		policyNow(),
	); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("lease invalidation accepted canceled context")
	}
	_ = tx.Rollback(ctx)
}

func TestIntegration_AuthorizeLeaseReportsPolicyTableFailure(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}
	if err := store.PublishSeat(ctx, seat, grant); err != nil {
		t.Fatalf("publish seat: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, grant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, seed.RuntimeAuthority, grant,
	); err != nil {
		t.Fatalf("publish runtime authority: %v", err)
	}
	lease := validLease(
		scope,
		store.root.OrganizationID,
		seat,
		mandate,
		policy,
		canonicalHash(t, &policy),
	)
	if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("register lease: %v", err)
	}
	if _, err := policyPool.Exec(
		ctx,
		`ALTER TABLE workforce_authority_lease_policies
		 RENAME TO workforce_authority_lease_policies_unavailable`,
	); err != nil {
		t.Fatalf("hide policy binding table: %v", err)
	}
	defer func() {
		_, err := policyPool.Exec(context.Background(), `
			ALTER TABLE workforce_authority_lease_policies_unavailable
			RENAME TO workforce_authority_lease_policies
		`)
		if err != nil {
			t.Errorf("restore policy binding table: %v", err)
		}
	}()
	if err := store.AuthorizeLease(ctx, lease.ID); err == nil ||
		errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("policy table failure was not reported as infrastructure error: %v", err)
	}
}

func TestIntegration_RegisterLeaseRollsBackWhenDurableTablesFail(t *testing.T) {
	for _, table := range []string{
		"workforce_authority_leases",
		"workforce_authority_lease_policies",
	} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			store, privateKey, seed, grant, _, scope := policyFixture(t)
			mandate := seed.Mandates[0]
			seat := seed.Organization.Departments[0].Seats[0]
			policy := signedPolicy(t, privateKey, store.root, scope, 1)
			if err := store.PublishMandate(ctx, mandate, grant); err != nil {
				t.Fatalf("publish mandate: %v", err)
			}
			if err := store.PublishSeat(ctx, seat, grant); err != nil {
				t.Fatalf("publish seat: %v", err)
			}
			if err := store.PublishPolicy(ctx, policy, grant); err != nil {
				t.Fatalf("publish policy: %v", err)
			}
			if err := store.PublishRuntimeAuthority(
				ctx, seed.RuntimeAuthority, grant,
			); err != nil {
				t.Fatalf("publish runtime authority: %v", err)
			}
			lease := validLease(
				scope,
				store.root.OrganizationID,
				seat,
				mandate,
				policy,
				canonicalHash(t, &policy),
			)
			if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
				t.Fatal(err)
			}
			unavailable := table + "_unavailable"
			if _, err := policyPool.Exec(
				ctx,
				`ALTER TABLE `+table+` RENAME TO `+unavailable,
			); err != nil {
				t.Fatalf("hide %s: %v", table, err)
			}
			defer func() {
				_, err := policyPool.Exec(
					context.Background(),
					`ALTER TABLE `+unavailable+` RENAME TO `+table,
				)
				if err != nil {
					t.Errorf("restore %s: %v", table, err)
				}
			}()
			if err := store.RegisterLease(ctx, lease); err == nil {
				t.Fatalf("registered lease without %s", table)
			}
			var count int
			queryTable := "workforce_authority_leases"
			if table == queryTable {
				queryTable = unavailable
			}
			if err := policyPool.QueryRow(ctx, `
				SELECT COUNT(*) FROM `+queryTable+`
				WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
			`, store.root.TenantID, store.root.OrganizationID, lease.ID).Scan(&count); err != nil {
				t.Fatalf("count rolled-back leases: %v", err)
			}
			if count != 0 {
				t.Fatalf("failed registration retained %d lease rows", count)
			}
		})
	}
}

func TestIntegration_AuthorityPublishRollsBackAtEveryLateBoundary(t *testing.T) {
	for _, boundary := range []string{"record", "head", "commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store, privateKey, _, grant, _, scope := policyFixture(t)
			policy := signedPolicy(t, privateKey, store.root, scope, 1)
			functionName := "fail_" + scope
			triggerName := "trigger_" + scope
			table := "workforce_authority_records"
			clause := "BEFORE INSERT"
			options := ""
			if boundary == "head" {
				table = "workforce_authority_heads"
			}
			if boundary == "commit" {
				clause = "AFTER INSERT"
				options = "DEFERRABLE INITIALLY DEFERRED"
			}
			if _, err := policyPool.Exec(ctx, `
				CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
				BEGIN
					RAISE EXCEPTION 'forced authority boundary failure';
				END;
				$$ LANGUAGE plpgsql
			`); err != nil {
				t.Fatalf("create failure function: %v", err)
			}
			triggerSQL := `CREATE `
			if boundary == "commit" {
				triggerSQL += `CONSTRAINT `
			}
			triggerSQL += `TRIGGER ` + triggerName + ` ` + clause + ` ON ` + table + ` ` +
				options + ` FOR EACH ROW WHEN (NEW.authority_id = '` + string(policy.ID) +
				`') EXECUTE FUNCTION ` + functionName + `()`
			if _, err := policyPool.Exec(ctx, triggerSQL); err != nil {
				_, _ = policyPool.Exec(ctx, `DROP FUNCTION `+functionName+`()`)
				t.Fatalf("create failure trigger: %v", err)
			}
			defer func() {
				_, _ = policyPool.Exec(
					context.Background(),
					`DROP TRIGGER IF EXISTS `+triggerName+` ON `+table,
				)
				_, _ = policyPool.Exec(
					context.Background(),
					`DROP FUNCTION IF EXISTS `+functionName+`()`,
				)
			}()
			if err := store.PublishPolicy(ctx, policy, grant); err == nil {
				t.Fatalf("publish crossed failing %s boundary", boundary)
			}
			var count int
			if err := policyPool.QueryRow(ctx, `
				SELECT COUNT(*) FROM workforce_authority_records
				WHERE tenant_id=$1 AND organization_id=$2
				  AND authority_kind='policy' AND authority_id=$3
			`, store.root.TenantID, store.root.OrganizationID, policy.ID).Scan(&count); err != nil {
				t.Fatalf("count rolled-back authority: %v", err)
			}
			if count != 0 {
				t.Fatalf("%s failure retained %d authority rows", boundary, count)
			}
		})
	}
}

func TestIntegration_MaterialPublishRollsBackIfLeaseInvalidationFails(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}
	if err := store.PublishSeat(ctx, seat, grant); err != nil {
		t.Fatalf("publish seat: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, grant); err != nil {
		t.Fatalf("publish policy v1: %v", err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, seed.RuntimeAuthority, grant,
	); err != nil {
		t.Fatalf("publish runtime authority: %v", err)
	}
	lease := validLease(
		scope,
		store.root.OrganizationID,
		seat,
		mandate,
		policy,
		canonicalHash(t, &policy),
	)
	if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("register lease: %v", err)
	}
	functionName := "fail_invalidation_" + scope
	triggerName := "trigger_invalidation_" + scope
	if _, err := policyPool.Exec(ctx, `
		CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'forced invalidation failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER `+triggerName+`
		BEFORE INSERT ON workforce_authority_lease_invalidations
		FOR EACH ROW WHEN (NEW.lease_id = '`+string(lease.ID)+`')
		EXECUTE FUNCTION `+functionName+`()
	`); err != nil {
		t.Fatalf("create invalidation failure trigger: %v", err)
	}
	defer func() {
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP TRIGGER IF EXISTS `+triggerName+
				` ON workforce_authority_lease_invalidations`,
		)
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP FUNCTION IF EXISTS `+functionName+`()`,
		)
	}()
	next := policy
	next.Version = 2
	next.Rules = []contracts.PolicyRule{{
		ClauseID: "changed", Outcome: "deny", Scope: "materially changed",
	}}
	if err := SignPolicy(&next, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign policy v2: %v", err)
	}
	if err := store.PublishPolicy(ctx, next, grant); err == nil {
		t.Fatal("material policy committed without required lease invalidation")
	}
	if _, err := store.LoadPolicy(ctx, next.ID, next.Version); !errors.Is(err, ErrRevoked) {
		t.Fatalf("rolled-back policy v2 load error = %v", err)
	}
}

func TestIntegration_RevocationCommitFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	store, privateKey, _, writeGrant, revokeGrant, scope := policyFixture(t)
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishPolicy(ctx, policy, writeGrant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	functionName := "fail_revocation_" + scope
	triggerName := "trigger_revocation_" + scope
	if _, err := policyPool.Exec(ctx, `
		CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'forced revocation commit failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER `+triggerName+`
		AFTER INSERT ON workforce_authority_revocations
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW WHEN (NEW.authority_id = '`+string(policy.ID)+`')
		EXECUTE FUNCTION `+functionName+`()
	`); err != nil {
		t.Fatalf("create revocation failure trigger: %v", err)
	}
	defer func() {
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP TRIGGER IF EXISTS `+triggerName+
				` ON workforce_authority_revocations`,
		)
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP FUNCTION IF EXISTS `+functionName+`()`,
		)
	}()
	revocation := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: string(policy.ID), Version: policy.Version,
		OwnerID: store.root.OwnerID, Reason: "commit failure test",
		RevokedAt: policyNow(),
	}
	if err := SignRevocation(&revocation, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := store.Revoke(ctx, revocation, revokeGrant); err == nil {
		t.Fatal("revocation survived deferred commit failure")
	}
	loaded, err := store.LoadPolicy(ctx, policy.ID, policy.Version)
	if err != nil || loaded.ID != policy.ID {
		t.Fatalf("commit failure revoked policy: %#v, %v", loaded, err)
	}
}

func TestIntegration_LeaseCommitFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}
	if err := store.PublishSeat(ctx, seat, grant); err != nil {
		t.Fatalf("publish seat: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, grant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, seed.RuntimeAuthority, grant,
	); err != nil {
		t.Fatalf("publish runtime authority: %v", err)
	}
	lease := validLease(
		scope,
		store.root.OrganizationID,
		seat,
		mandate,
		policy,
		canonicalHash(t, &policy),
	)
	if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	functionName := "fail_lease_" + scope
	triggerName := "trigger_lease_" + scope
	if _, err := policyPool.Exec(ctx, `
		CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'forced lease commit failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER `+triggerName+`
		AFTER INSERT ON workforce_authority_lease_policies
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW WHEN (NEW.lease_id = '`+string(lease.ID)+`')
		EXECUTE FUNCTION `+functionName+`()
	`); err != nil {
		t.Fatalf("create lease failure trigger: %v", err)
	}
	defer func() {
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP TRIGGER IF EXISTS `+triggerName+
				` ON workforce_authority_lease_policies`,
		)
		_, _ = policyPool.Exec(
			context.Background(),
			`DROP FUNCTION IF EXISTS `+functionName+`()`,
		)
	}()
	if err := store.RegisterLease(ctx, lease); err == nil {
		t.Fatal("lease survived deferred commit failure")
	}
	if err := store.AuthorizeLease(ctx, lease.ID); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("rolled-back lease authorization = %v, want ErrLeaseInvalid", err)
	}
}
