package lease_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
)

const postgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lease integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	integrationPool, err = waitForPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "lease integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, integrationPool, baseTime()); err != nil {
		integrationPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "lease integration migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	integrationPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_LeaseStore_ConcurrentAcquireFenceRenewCancelAndReacquire(t *testing.T) {
	now := baseTime()
	store, err := lease.New(integrationPool, "tenant:lease", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest("lifecycle", now)
	insertAuthority(t, "tenant:lease", request, 1)
	results := make(chan struct {
		grant lease.Grant
		err   error
	}, 2)
	var group sync.WaitGroup
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			grant, acquireErr := store.Acquire(context.Background(), request)
			results <- struct {
				grant lease.Grant
				err   error
			}{grant, acquireErr}
		}()
	}
	group.Wait()
	close(results)
	var grant lease.Grant
	success, held := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			success++
			grant = result.grant
		case errors.Is(result.err, lease.ErrHeld):
			held++
		default:
			t.Fatalf("unexpected acquire result: %v", result.err)
		}
	}
	if success != 1 || held != 1 || grant.Fence == 0 {
		t.Fatalf("success=%d held=%d grant=%+v", success, held, grant)
	}
	if authorized, err := store.Authorize(context.Background(), request.OrganizationID,
		request.ID, grant.Fence); err != nil || authorized.Fence != grant.Fence {
		t.Fatalf("current authorization = %+v, %v", authorized, err)
	}
	recovered, err := store.Recover(
		context.Background(), request.OrganizationID, request.ID,
		request.WakeID, request.SeatID, request.NodeID,
	)
	if err != nil || recovered.Fence != grant.Fence {
		t.Fatalf("bounded recovery = %+v, %v", recovered, err)
	}
	if _, err := store.Recover(
		context.Background(), request.OrganizationID, request.ID,
		request.WakeID, request.SeatID, dependency.NodeID("node:other"),
	); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("mismatched recovery = %v", err)
	}
	if _, err := store.Authorize(context.Background(), request.OrganizationID,
		request.ID, grant.Fence+1); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("stale authorization = %v", err)
	}
	renewed, err := store.Renew(context.Background(), request.OrganizationID,
		request.ID, grant.Fence, now.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if renewed.RenewedAt == nil || renewed.Fence != grant.Fence {
		t.Fatalf("renewal changed authority: %+v", renewed)
	}
	if err := store.Cancel(context.Background(), request.OrganizationID,
		request.ID, grant.Fence, "owner_cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authorize(context.Background(), request.OrganizationID,
		request.ID, grant.Fence); !errors.Is(err, lease.ErrCancelled) {
		t.Fatalf("cancelled authorization = %v", err)
	}
	if _, err := store.Renew(context.Background(), request.OrganizationID,
		request.ID, grant.Fence, now.Add(time.Hour)); !errors.Is(err, lease.ErrCancelled) {
		t.Fatalf("cancelled renewal = %v", err)
	}
	if err := store.Cancel(context.Background(), request.OrganizationID,
		request.ID, grant.Fence, "again"); !errors.Is(err, lease.ErrCancelled) {
		t.Fatalf("cancelled cancellation = %v", err)
	}
	second := validRequest("reacquired", now)
	second.OrganizationID = request.OrganizationID
	second.SeatID = request.SeatID
	second.NodeID = request.NodeID
	insertAuthority(t, "tenant:lease", second, 1)
	next, err := store.Acquire(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if next.Fence <= grant.Fence {
		t.Fatalf("reacquired fence=%d, prior=%d", next.Fence, grant.Fence)
	}
	if err := store.Cancel(context.Background(), second.OrganizationID,
		second.ID, grant.Fence, "stale"); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("stale cancellation = %v", err)
	}
}

func TestIntegration_LeaseStore_ExpiryPolicyDriftAndUncertaintyFailClosed(t *testing.T) {
	now := baseTime()
	store, err := lease.New(integrationPool, "tenant:expiry", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest("expiry", now)
	insertAuthority(t, "tenant:expiry", request, 1)
	grant, err := store.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	now = request.ExpiresAt
	if _, err := store.Authorize(context.Background(), request.OrganizationID,
		request.ID, grant.Fence); !errors.Is(err, lease.ErrExpired) {
		t.Fatalf("expired authorization = %v", err)
	}
	if _, err := store.Renew(context.Background(), request.OrganizationID,
		request.ID, grant.Fence, now.Add(time.Hour)); !errors.Is(err, lease.ErrExpired) {
		t.Fatalf("expired renewal = %v", err)
	}
	reacquire := validRequest("after-expiry", now)
	reacquire.ExpiresAt = now.Add(time.Hour)
	insertAuthority(t, "tenant:expiry", reacquire, 1)
	next, err := store.Acquire(context.Background(), reacquire)
	if err != nil {
		t.Fatal(err)
	}
	insertAuthority(t, "tenant:expiry", reacquire, 2)
	if _, err := store.Authorize(context.Background(), reacquire.OrganizationID,
		reacquire.ID, next.Fence); !errors.Is(err, lease.ErrPolicyMismatch) {
		t.Fatalf("policy drift authorization = %v", err)
	}
	closed, err := pgxpool.New(context.Background(),
		"postgres://postgres:password@127.0.0.1:1/database?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	uncertain, err := lease.New(closed, "tenant:uncertain", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uncertain.Acquire(context.Background(), validRequest("uncertain", now)); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("closed acquire = %v", err)
	}
	if _, err := uncertain.Authorize(context.Background(), reacquire.OrganizationID,
		reacquire.ID, next.Fence); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("closed authorize = %v", err)
	}
	if _, err := store.Authorize(context.Background(), "organization:missing",
		"lease:missing", 1); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("missing authorization = %v", err)
	}
	badMandate := validRequest("bad-mandate", now)
	badMandate.ExpiresAt = now.Add(time.Hour)
	insertAuthority(t, "tenant:expiry", badMandate, 1)
	badMandate.MandateVersion = 2
	if _, err := store.Acquire(context.Background(), badMandate); !errors.Is(err, lease.ErrPolicyMismatch) {
		t.Fatalf("stale mandate acquisition = %v", err)
	}
	badPolicy := validRequest("bad-policy", now)
	badPolicy.ExpiresAt = now.Add(time.Hour)
	insertAuthority(t, "tenant:expiry", badPolicy, 1)
	badPolicy.Policies[0].Hash.Digest = strings.Repeat("c", 64)
	if _, err := store.Acquire(context.Background(), badPolicy); !errors.Is(err, lease.ErrPolicyMismatch) {
		t.Fatalf("stale policy acquisition = %v", err)
	}
}

func TestLeaseStore_RejectsInvalidInputs(t *testing.T) {
	if _, err := lease.New(nil, "tenant", baseTime); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, err := lease.New(integrationPool, "", baseTime); err == nil {
		t.Fatal("empty tenant accepted")
	}
	if _, err := lease.New(integrationPool, "tenant", nil); err == nil {
		t.Fatal("nil time source accepted")
	}
	store, err := lease.New(integrationPool, "tenant:invalid", baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), lease.Request{}); err == nil {
		t.Fatal("invalid acquisition accepted")
	}
	request := validRequest("future", baseTime().Add(time.Minute))
	insertAuthority(t, "tenant:invalid", request, 1)
	if _, err := store.Acquire(context.Background(), request); !errors.Is(err, lease.ErrExpired) {
		t.Fatalf("future acquisition = %v", err)
	}
	for index, candidate := range []struct {
		organization contracts.OrganizationID
		leaseID      contracts.LeaseID
		fence        contracts.FenceToken
		expires      time.Time
	}{
		{"", "lease", 1, baseTime().Add(time.Hour)},
		{"organization", "", 1, baseTime().Add(time.Hour)},
		{"organization", "lease", 0, baseTime().Add(time.Hour)},
		{"organization", "lease", 1, baseTime()},
	} {
		if _, err := store.Renew(context.Background(), candidate.organization,
			candidate.leaseID, candidate.fence, candidate.expires); err == nil {
			t.Fatalf("invalid renew case %d accepted", index)
		}
	}
	if err := store.Cancel(context.Background(), "organization", "lease", 1, ""); err == nil {
		t.Fatal("empty cancellation reason accepted")
	}
	badClock, err := lease.New(integrationPool, "tenant:clock", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badClock.Acquire(context.Background(), validRequest("clock", baseTime())); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("non-UTC clock = %v", err)
	}
	if _, err := badClock.Renew(context.Background(), "organization:clock",
		"lease:clock", 1, baseTime().Add(time.Hour)); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("non-UTC renew clock = %v", err)
	}
	if err := badClock.Cancel(context.Background(), "organization:clock",
		"lease:clock", 1, "cancel"); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("non-UTC cancel clock = %v", err)
	}
	if _, err := badClock.Authorize(context.Background(), "organization:clock",
		"lease:clock", 1); !errors.Is(err, lease.ErrUncertain) {
		t.Fatalf("non-UTC authorize clock = %v", err)
	}
	if _, err := store.Renew(context.Background(), "organization:missing",
		"lease:missing", 1, baseTime().Add(time.Hour)); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("missing renewal = %v", err)
	}
	noAuthority := validRequest("no-authority", baseTime())
	if _, err := store.Acquire(context.Background(), noAuthority); !errors.Is(err, lease.ErrPolicyMismatch) {
		t.Fatalf("missing authority acquisition = %v", err)
	}
}

func TestIntegration_LeaseStore_RealDatabaseFailuresFailClosed(t *testing.T) {
	t.Run("acquire", func(t *testing.T) {
		pool := isolatedPool(t, "acquire")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-acquire", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_runtime_leases CASCADE`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(context.Background(), request); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("acquire without lease table = %v", err)
		}
	})
	t.Run("renew", func(t *testing.T) {
		pool := isolatedPool(t, "renew")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_runtime_leases CASCADE`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Renew(context.Background(), "organization:error",
			"lease:error", 1, baseTime().Add(time.Hour)); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("renew without lease table = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		pool := isolatedPool(t, "cancel")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_runtime_leases CASCADE`); err != nil {
			t.Fatal(err)
		}
		if err := store.Cancel(context.Background(), "organization:error",
			"lease:error", 1, "cancel"); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("cancel without lease table = %v", err)
		}
	})
	t.Run("policies", func(t *testing.T) {
		pool := isolatedPool(t, "policies")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-policies", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		grant, err := store.Acquire(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_runtime_lease_policies`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authorize(context.Background(), request.OrganizationID,
			request.ID, grant.Fence); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("authorize without policy table = %v", err)
		}
	})
	t.Run("fence counter", func(t *testing.T) {
		pool := isolatedPool(t, "fence-counter")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-fence", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_fence_counters`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(context.Background(), request); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("acquire without fence counter = %v", err)
		}
	})
	t.Run("authority", func(t *testing.T) {
		pool := isolatedPool(t, "authority")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-authority", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		grant, err := store.Acquire(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_authority_records CASCADE`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authorize(context.Background(), request.OrganizationID,
			request.ID, grant.Fence); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("authorize without authority table = %v", err)
		}
	})
	t.Run("lease scan", func(t *testing.T) {
		pool := isolatedPool(t, "lease-scan")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-lease-scan", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		grant, err := store.Acquire(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
			ALTER TABLE workforce_runtime_leases
			DROP CONSTRAINT workforce_runtime_leases_fence_check;
			ALTER TABLE workforce_runtime_leases
			ALTER COLUMN fence TYPE TEXT USING fence::TEXT;
			UPDATE workforce_runtime_leases SET fence='bad'
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authorize(context.Background(), request.OrganizationID,
			request.ID, grant.Fence); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("authorize malformed lease = %v", err)
		}
	})
	t.Run("policy scan", func(t *testing.T) {
		pool := isolatedPool(t, "policy-scan")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-policy-scan", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		grant, err := store.Acquire(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
			ALTER TABLE workforce_runtime_lease_policies
			DROP CONSTRAINT workforce_runtime_lease_policies_policy_version_check;
			ALTER TABLE workforce_runtime_lease_policies
			ALTER COLUMN policy_version TYPE TEXT USING policy_version::TEXT;
			UPDATE workforce_runtime_lease_policies SET policy_version='bad'
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authorize(context.Background(), request.OrganizationID,
			request.ID, grant.Fence); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("authorize malformed policy = %v", err)
		}
	})
	t.Run("lease insert", func(t *testing.T) {
		pool := isolatedPool(t, "lease-insert")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-lease-insert", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		if _, err := pool.Exec(context.Background(), `
			CREATE FUNCTION reject_runtime_lease() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'rejected'; END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER reject_runtime_lease
			BEFORE INSERT ON workforce_runtime_leases
			FOR EACH ROW EXECUTE FUNCTION reject_runtime_lease()
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(context.Background(), request); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("rejected lease insert = %v", err)
		}
	})
	t.Run("policy bind", func(t *testing.T) {
		pool := isolatedPool(t, "policy-bind")
		store, err := lease.New(pool, "tenant:error", baseTime)
		if err != nil {
			t.Fatal(err)
		}
		request := validRequest("error-policy-bind", baseTime())
		insertAuthorityPool(t, pool, "tenant:error", request, 1)
		if _, err := pool.Exec(context.Background(), `
			CREATE FUNCTION reject_runtime_policy() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'rejected'; END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER reject_runtime_policy
			BEFORE INSERT ON workforce_runtime_lease_policies
			FOR EACH ROW EXECUTE FUNCTION reject_runtime_policy()
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(context.Background(), request); !errors.Is(err, lease.ErrUncertain) {
			t.Fatalf("rejected policy bind = %v", err)
		}
	})
}

func validRequest(label string, now time.Time) lease.Request {
	hash := strings.Repeat("a", 64)
	return lease.Request{
		ID: contracts.LeaseID("lease:" + label), WakeID: contracts.WakeID("wake:" + label),
		OrganizationID: contracts.OrganizationID("organization:" + label),
		SeatID:         contracts.SeatID("seat:" + label), NodeID: dependency.NodeID("node:" + label),
		MandateID: contracts.MandateID("mandate:" + label), MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: contracts.PolicyID("policy:" + label), Version: 1,
			Hash: contracts.ContentHash{Algorithm: "sha256", Digest: hash},
		}},
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func insertAuthority(t *testing.T, tenantID string, request lease.Request, version uint64) {
	t.Helper()
	insertAuthorityPool(t, integrationPool, tenantID, request, version)
}

func insertAuthorityPool(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID string,
	request lease.Request,
	version uint64,
) {
	t.Helper()
	now := baseTime()
	for _, record := range []struct {
		kind, id, hash string
		version        uint64
	}{
		{"mandate", string(request.MandateID), strings.Repeat("b", 64), version},
		{"policy", string(request.Policies[0].ID), request.Policies[0].Hash.Digest, version},
	} {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO workforce_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,
				material_change,created_at
			) VALUES ($1,$2,$3,$4,$5,'owner','key',$6,$7,$8,FALSE,$6)
			ON CONFLICT DO NOTHING
		`, tenantID, request.OrganizationID, record.kind, record.id, record.version,
			now, record.hash, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func baseTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", fmt.Errorf("random container suffix: %w", err)
	}
	name := "workforce-lease-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name, "-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432", postgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL container: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("inspect PostgreSQL port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	_, port, found := strings.Cut(address, ":")
	if !found || port == "" {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL port mapping %q", address)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" + port +
			"/workforce?sslmode=disable", nil
}

func waitForPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for PostgreSQL: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func isolatedPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	schema := "lease_" + strings.ReplaceAll(strings.ToLower(label), "-", "_")
	if _, err := integrationPool.Exec(context.Background(),
		`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := integrationPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(integrationPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = integrationPool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	if err := ledger.ApplyMigrations(context.Background(), pool, baseTime()); err != nil {
		t.Fatal(err)
	}
	return pool
}
