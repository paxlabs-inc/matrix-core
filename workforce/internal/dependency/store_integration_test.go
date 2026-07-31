package dependency_test

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
	"matrix/workforce/internal/ledger"
)

const postgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency integration setup:", err)
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
		fmt.Fprintln(os.Stderr, "dependency integration setup:", err)
		os.Exit(1)
	}
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	if err := ledger.ApplyMigrations(ctx, integrationPool, now); err != nil {
		integrationPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "dependency integration migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	integrationPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_GraphStore_RejectsConcurrentCycleAndPropagatesCancellation(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	store, err := dependency.New(integrationPool, "tenant:dependency", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	organizationID := contracts.OrganizationID("organization:" + strings.ReplaceAll(t.Name(), "/", "-"))
	for _, id := range []dependency.NodeID{"a", "b", "c"} {
		node := dependency.Node{
			ID: id, OrganizationID: organizationID, Kind: dependency.NodeIntent,
			Title: string(id), State: dependency.StatePending, CreatedAt: now,
			UpdatedAt: now, Version: 1,
		}
		if err := store.PutNode(context.Background(), node); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	if err := store.AddEdge(context.Background(), dependency.Edge{
		OrganizationID: organizationID, Prerequisite: "a", Dependent: "b",
		Kind: dependency.EdgeDependency, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, edge := range []dependency.Edge{
		{OrganizationID: organizationID, Prerequisite: "b", Dependent: "c",
			Kind: dependency.EdgeDependency, CreatedAt: now},
		{OrganizationID: organizationID, Prerequisite: "c", Dependent: "a",
			Kind: dependency.EdgeDependency, CreatedAt: now},
	} {
		group.Add(1)
		go func(candidate dependency.Edge) {
			defer group.Done()
			results <- store.AddEdge(context.Background(), candidate)
		}(edge)
	}
	group.Wait()
	close(results)
	success, cycles := 0, 0
	for result := range results {
		switch {
		case result == nil:
			success++
		case errors.Is(result, dependency.ErrCycle):
			cycles++
		default:
			t.Fatalf("unexpected concurrent edge result: %v", result)
		}
	}
	if success != 1 || cycles != 1 {
		t.Fatalf("success=%d cycles=%d, want 1/1", success, cycles)
	}
	snapshot, err := store.Snapshot(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	dependents := make(map[dependency.NodeID]bool)
	prerequisites := make(map[dependency.NodeID]bool)
	for _, edge := range snapshot.Edges {
		dependents[edge.Dependent] = true
		prerequisites[edge.Prerequisite] = true
	}
	var root dependency.NodeID
	for candidate := range prerequisites {
		if !dependents[candidate] {
			root = candidate
			break
		}
	}
	if root == "" {
		t.Fatalf("acyclic committed graph has no root: %+v", snapshot.Edges)
	}
	if err := store.Transition(context.Background(), organizationID, root, 1,
		dependency.StateCancelled, "owner_cancelled"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range snapshot.Nodes {
		if node.State != dependency.StateCancelled {
			t.Fatalf("node %s state=%s, want propagated cancellation", node.ID, node.State)
		}
	}
}

func TestIntegration_GraphStore_IdempotencyResolutionAndCorrectionPropagation(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	store, err := dependency.New(integrationPool, "tenant:dependency", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	organizationID := contracts.OrganizationID("organization:resolve")
	seat := contracts.SeatID("seat:executor")
	department := contracts.DepartmentID("department:developer")
	terminal := contracts.RecordID("record:terminal")
	deadline := now.Add(time.Hour)
	root := dependency.Node{
		ID: "root", OrganizationID: organizationID, Kind: dependency.NodeGoal,
		OwnerSeatID: &seat, OwnerDepartmentID: &department, Title: "root",
		State: dependency.StateCompleted, CreatedAt: now, UpdatedAt: now,
		Deadline: &deadline, TerminalRecordID: &terminal, Version: 1,
	}
	child := dependency.Node{
		ID: "child", OrganizationID: organizationID, Kind: dependency.NodeIntent,
		Title: "child", State: dependency.StatePending, CreatedAt: now,
		UpdatedAt: now, Version: 1,
	}
	for _, node := range []dependency.Node{root, child} {
		if err := store.PutNode(context.Background(), node); err != nil {
			t.Fatal(err)
		}
		if err := store.PutNode(context.Background(), node); err != nil {
			t.Fatalf("idempotent put: %v", err)
		}
	}
	changed := child
	changed.Title = "different"
	if err := store.PutNode(context.Background(), changed); !errors.Is(err, dependency.ErrConflict) {
		t.Fatalf("conflicting put = %v, want ErrConflict", err)
	}
	edge := dependency.Edge{
		OrganizationID: organizationID, Prerequisite: "root", Dependent: "child",
		Kind: dependency.EdgeDependency, CreatedAt: now,
	}
	if err := store.AddEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEdge(context.Background(), edge); err != nil {
		t.Fatalf("idempotent edge: %v", err)
	}
	conflictingEdge := edge
	conflictingEdge.CreatedAt = now.Add(time.Second)
	if err := store.AddEdge(context.Background(), conflictingEdge); !errors.Is(err, dependency.ErrConflict) {
		t.Fatalf("conflicting edge = %v, want ErrConflict", err)
	}
	if err := store.AddEdge(context.Background(), dependency.Edge{
		OrganizationID: organizationID, Prerequisite: "missing", Dependent: "child",
		Kind: dependency.EdgeDependency, CreatedAt: now,
	}); !errors.Is(err, dependency.ErrNotFound) {
		t.Fatalf("missing endpoint edge = %v, want ErrNotFound", err)
	}
	projection, err := store.Resolve(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Eligible) != 1 || projection.Eligible[0].ID != "child" {
		t.Fatalf("eligible projection = %+v", projection.Eligible)
	}
	snapshot, err := store.Snapshot(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	var childVersion uint64
	for _, node := range snapshot.Nodes {
		if node.ID == "child" {
			childVersion = node.Version
			if node.State != dependency.StateEligible {
				t.Fatalf("persisted child state = %s", node.State)
			}
		}
	}
	if err := store.Transition(context.Background(), organizationID, "child",
		childVersion, dependency.StateContested, ""); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range snapshot.Nodes {
		if node.ID == "child" && (!node.Contested || node.State != dependency.StateContested) {
			t.Fatalf("correction contamination not persisted: %+v", node)
		}
	}
	if err := store.Transition(context.Background(), organizationID, "missing", 1,
		dependency.StateCancelled, "missing"); !errors.Is(err, dependency.ErrConflict) {
		t.Fatalf("missing transition = %v, want fail closed conflict", err)
	}
	if err := store.Transition(context.Background(), organizationID, "child", 1,
		dependency.StateCancelled, "stale"); !errors.Is(err, dependency.ErrConflict) {
		t.Fatalf("stale transition = %v, want ErrConflict", err)
	}
}

func TestIntegration_GraphStore_PersistsDelegationIncidentsAndNormalizesTimes(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	store, err := dependency.New(integrationPool, "tenant:incidents", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	organizationID := contracts.OrganizationID("organization:incidents")
	for _, node := range []dependency.Node{
		{ID: "delegated", OrganizationID: organizationID, Kind: dependency.NodeDelegation,
			Title: "delegated", State: dependency.StateWaiting, CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour), Version: 1},
		{ID: "consumer", OrganizationID: organizationID, Kind: dependency.NodeIntent,
			Title: "consumer", State: dependency.StatePending, CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour), Version: 1},
		{ID: "orphan", OrganizationID: organizationID, Kind: dependency.NodeIntent,
			Title: "orphan", State: dependency.StatePending, CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour), Version: 1},
	} {
		if err := store.PutNode(context.Background(), node); err != nil {
			t.Fatal(err)
		}
	}
	expires, sla := now.Add(-time.Minute), now.Add(-time.Hour)
	edge := dependency.Edge{
		OrganizationID: organizationID, Prerequisite: "delegated", Dependent: "consumer",
		Kind: dependency.EdgeDelegation, RequiredResponseSchema: "response.v1",
		ExpiresAt: &expires, TimeoutAction: contracts.TimeoutEscalate,
		SLAAt: &sla, CreatedAt: now.Add(-2 * time.Hour),
	}
	if err := store.AddEdge(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	projection, err := store.Resolve(context.Background(), organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Incidents) < 4 {
		t.Fatalf("persisted incident count = %d, want at least 4", len(projection.Incidents))
	}
	var count int
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_work_incidents
		WHERE tenant_id=$1 AND organization_id=$2
	`, "tenant:incidents", organizationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(projection.Incidents) {
		t.Fatalf("incident rows=%d projection=%d", count, len(projection.Incidents))
	}
}

func TestGraphStore_RejectsInvalidConstructionAndClosedDatabase(t *testing.T) {
	now := func() time.Time { return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC) }
	if _, err := dependency.New(nil, "tenant", now); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, err := dependency.New(integrationPool, "", now); err == nil {
		t.Fatal("empty tenant accepted")
	}
	if _, err := dependency.New(integrationPool, "tenant", nil); err == nil {
		t.Fatal("nil time source accepted")
	}
	validStore, err := dependency.New(integrationPool, "tenant:validation", now)
	if err != nil {
		t.Fatal(err)
	}
	invalidNode := dependency.Node{}
	if err := validStore.PutNode(context.Background(), invalidNode); err == nil {
		t.Fatal("invalid node accepted by store")
	}
	if err := validStore.AddEdge(context.Background(), dependency.Edge{}); err == nil {
		t.Fatal("invalid edge accepted by store")
	}
	for index, transition := range []struct {
		organization contracts.OrganizationID
		node         dependency.NodeID
		version      uint64
		state        dependency.NodeState
		reason       string
	}{
		{"", "node", 1, dependency.StateCancelled, "cancel"},
		{"organization:test", "", 1, dependency.StateCancelled, "cancel"},
		{"organization:test", "node", 0, dependency.StateCancelled, "cancel"},
		{"organization:test", "node", 1, "unknown", ""},
		{"organization:test", "node", 1, dependency.StateCancelled, ""},
		{"organization:test", "node", 1, dependency.StatePending, "forbidden"},
	} {
		if err := validStore.Transition(context.Background(), transition.organization,
			transition.node, transition.version, transition.state, transition.reason); err == nil {
			t.Fatalf("invalid transition case %d accepted", index)
		}
	}
	invalidTime, err := dependency.New(integrationPool, "tenant:invalid-time",
		func() time.Time { return time.Now() })
	if err != nil {
		t.Fatal(err)
	}
	validNode := dependency.Node{
		ID: "node", OrganizationID: "organization:invalid-time",
		Kind: dependency.NodeIntent, Title: "node", State: dependency.StatePending,
		CreatedAt: now(), UpdatedAt: now(), Version: 1,
	}
	if err := invalidTime.PutNode(context.Background(), validNode); err == nil {
		t.Fatal("non-UTC store time accepted")
	}
	if _, err := invalidTime.Resolve(context.Background(), "organization:invalid-time"); err == nil {
		t.Fatal("non-UTC resolve time accepted")
	}
	closed, err := pgxpool.New(context.Background(),
		"postgres://postgres:workforce-test-password@127.0.0.1:1/workforce?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	store, err := dependency.New(closed, "tenant:closed", now)
	if err != nil {
		t.Fatal(err)
	}
	node := dependency.Node{
		ID: "node", OrganizationID: "organization:closed", Kind: dependency.NodeIntent,
		Title: "node", State: dependency.StatePending, CreatedAt: now(),
		UpdatedAt: now(), Version: 1,
	}
	if err := store.PutNode(context.Background(), node); err == nil {
		t.Fatal("closed database put succeeded")
	}
	if err := store.AddEdge(context.Background(), dependency.Edge{
		OrganizationID: "organization:closed", Prerequisite: "a", Dependent: "b",
		Kind: dependency.EdgeDependency, CreatedAt: now(),
	}); err == nil {
		t.Fatal("closed database edge succeeded")
	}
	if err := store.Transition(context.Background(), "organization:closed", "node", 1,
		dependency.StateCancelled, "cancel"); err == nil {
		t.Fatal("closed database transition succeeded")
	}
	if _, err := store.Resolve(context.Background(), "organization:closed"); err == nil {
		t.Fatal("closed database resolve succeeded")
	}
	if _, err := store.Snapshot(context.Background(), "organization:closed"); err == nil {
		t.Fatal("closed database snapshot succeeded")
	}
}

func TestIntegration_GraphStore_RealDatabaseFailuresFailClosed(t *testing.T) {
	t.Run("put", func(t *testing.T) {
		pool := isolatedPool(t, "put")
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_work_nodes CASCADE`); err != nil {
			t.Fatal(err)
		}
		store, err := dependency.New(pool, "tenant:error", integrationClock)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutNode(context.Background(), errorTestNode()); err == nil {
			t.Fatal("put succeeded without graph table")
		}
	})
	t.Run("edge", func(t *testing.T) {
		pool := isolatedPool(t, "edge")
		store, err := dependency.New(pool, "tenant:error", integrationClock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_work_nodes CASCADE`); err != nil {
			t.Fatal(err)
		}
		if err := store.AddEdge(context.Background(), dependency.Edge{
			OrganizationID: "organization:error", Prerequisite: "a", Dependent: "b",
			Kind: dependency.EdgeDependency, CreatedAt: integrationClock(),
		}); err == nil {
			t.Fatal("edge succeeded without graph table")
		}
	})
	t.Run("transition", func(t *testing.T) {
		pool := isolatedPool(t, "transition")
		store, err := dependency.New(pool, "tenant:error", integrationClock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_work_nodes CASCADE`); err != nil {
			t.Fatal(err)
		}
		if err := store.Transition(context.Background(), "organization:error", "node", 1,
			dependency.StateCancelled, "cancel"); err == nil {
			t.Fatal("transition succeeded without graph table")
		}
	})
	t.Run("edge scan", func(t *testing.T) {
		pool := isolatedPool(t, "edge-scan")
		store, err := dependency.New(pool, "tenant:error", integrationClock)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutNode(context.Background(), errorTestNode()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `DROP TABLE workforce_work_edges`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Snapshot(context.Background(), "organization:error"); err == nil {
			t.Fatal("snapshot succeeded without edge table")
		}
		if _, err := store.Resolve(context.Background(), "organization:error"); err == nil {
			t.Fatal("resolve succeeded without edge table")
		}
	})
}

func startPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", fmt.Errorf("random container suffix: %w", err)
	}
	name := "workforce-dependency-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432",
		postgresImage,
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
			"/workforce?sslmode=disable",
		nil
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
	schema := "dependency_" + strings.ReplaceAll(strings.ToLower(label), "-", "_")
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
	if err := ledger.ApplyMigrations(context.Background(), pool, integrationClock()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func integrationClock() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func errorTestNode() dependency.Node {
	now := integrationClock()
	return dependency.Node{
		ID: "node", OrganizationID: "organization:error", Kind: dependency.NodeIntent,
		Title: "node", State: dependency.StatePending, CreatedAt: now,
		UpdatedAt: now, Version: 1,
	}
}
