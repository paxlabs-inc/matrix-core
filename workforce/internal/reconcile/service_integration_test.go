package reconcile

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/testauthority"
)

const reconcilePostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var reconcilePool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startReconcilePostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	reconcilePool, err = waitReconcilePostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "reconcile integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, reconcilePool, reconcileTime()); err != nil {
		reconcilePool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "reconcile migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	reconcilePool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_ReconciliationSweep_ResolvesSixOutcomesAndCorrection(t *testing.T) {
	ctx := context.Background()
	now := reconcileTime()
	tenant := "tenant:reconcile"
	organizationID := contracts.OrganizationID("organization:reconcile")
	graph, err := dependency.New(reconcilePool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	authority, err := testauthority.New(
		reconcilePool, reconcileVault(t, tenant), tenant, organizationID,
		"reconcile", func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(reconcilePool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(reconcilePool, tenant, circuit.Config{
		FailureThreshold: 20, SuccessThreshold: 1, Window: time.Hour,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter := reconcileAdapter(t)
	gateway, err := effect.New(
		reconcilePool, reconcileVault(t, tenant), leaseStore, authority.Store(), breakers,
		tenant, approval.Authority{}, func() time.Time { return now }, adapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := []struct {
		label   string
		outcome skills.ProbeOutcome
		want    dependency.NodeState
	}{
		{"completed", skills.ProbeCompletedOutOfBand, dependency.StateWaiting},
		{"unchanged", skills.ProbeUnchanged, dependency.StateWaiting},
		{"reversed", skills.ProbeReversed, dependency.StateContested},
		{"drifted", skills.ProbeDrifted, dependency.StateContested},
		{"conflicted", skills.ProbeConflicted, dependency.StateContested},
		{"unknown", skills.ProbeUnknown, dependency.StateContested},
		{"unavailable", skills.ProbeUnknown, dependency.StateContested},
		{"corrected", skills.ProbeCompletedOutOfBand, dependency.StateContested},
	}
	for _, item := range outcomes {
		nodeID := dependency.NodeID("node:" + item.label)
		if err := graph.PutNode(ctx, dependency.Node{
			ID: nodeID, OrganizationID: organizationID, Kind: dependency.NodeIntent,
			Title: "reconcile " + item.label, State: dependency.StateLeased,
			BasePriority: 1, CreatedAt: now, UpdatedAt: now, Version: 1,
		}); err != nil {
			t.Fatal(err)
		}
		request := reconcileLeaseRequest(item.label, organizationID, nodeID, now)
		published, err := authority.Publish(ctx, request, "skill:reconcile")
		if err != nil {
			t.Fatal(err)
		}
		request = published.Request
		grant, err := leaseStore.Acquire(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authority.Register(ctx, published, grant.Fence, item.label); err != nil {
			t.Fatal(err)
		}
		hash := contracts.ContentHash{
			Algorithm: "sha256", Digest: strings.Repeat("a", 64),
		}
		proposal := effect.Proposal{
			ID: "proposal:" + item.label, OrganizationID: organizationID,
			IntentID: contracts.IntentID("intent:" + item.label), NodeID: nodeID,
			SeatID: request.SeatID, LeaseID: request.ID, Fence: grant.Fence,
			Provider: adapter.Name(), SkillID: "skill:reconcile",
			EffectClass:    skills.EffectReversible,
			Operation:      "probe_" + item.label,
			IdempotencyKey: "effect:" + item.label,
			SkillDigest:    hash, OperationDigest: hash,
			Input: []byte("payload-" + item.label), Deadline: now.Add(30 * time.Minute),
		}
		authorizeReconcileProposal(t, tenant, proposal)
		result, err := gateway.Execute(ctx, proposal)
		if !errors.Is(err, effect.ErrAmbiguous) ||
			result.State != effect.StateExternallyAmbiguous {
			t.Fatalf("%s ambiguous setup = %+v, %v", item.label, result, err)
		}
		if item.label == "corrected" {
			if err := graph.Transition(ctx, organizationID, nodeID, 1,
				dependency.StateContested, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = now.Add(2 * time.Hour)
	service, err := New(reconcilePool, gateway, graph, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	summary, err := service.Sweep(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Events) != len(outcomes) || summary.Completed != 2 ||
		summary.Unchanged != 1 || summary.Blocked != 5 {
		t.Fatalf("summary = %+v", summary)
	}
	for index := 1; index < len(summary.Events); index++ {
		if summary.Events[index-1].ProposalID > summary.Events[index].ProposalID {
			t.Fatal("events are not deterministically ordered")
		}
	}
	snapshot, err := graph.Snapshot(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[dependency.NodeID]dependency.NodeState, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		states[node.ID] = node.State
	}
	for _, item := range outcomes {
		if got := states[dependency.NodeID("node:"+item.label)]; got != item.want {
			t.Fatalf("%s graph state = %s, want %s", item.label, got, item.want)
		}
	}
	var eventCount int
	if err := reconcilePool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_reconciliation_events
		WHERE tenant_id=$1 AND organization_id=$2
	`, tenant, organizationID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != len(outcomes) {
		t.Fatalf("durable event count = %d", eventCount)
	}
	again, err := service.Sweep(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != len(outcomes)-2 {
		t.Fatalf("repeat unresolved sweep events = %d", len(again.Events))
	}
}

func TestReconciliationService_FailsClosedOnInvalidConstructionAndState(t *testing.T) {
	if _, err := New(nil, nil, nil, "", nil); err == nil {
		t.Fatal("empty construction accepted")
	}
	if state := aggregate(nil); state != dependency.StateWaiting {
		t.Fatalf("empty aggregate = %s", state)
	}
	if state := aggregate([]skills.ProbeOutcome{
		skills.ProbeCompletedOutOfBand, skills.ProbeCompletedOutOfBand,
	}); state != dependency.StateWaiting {
		t.Fatalf("completed aggregate = %s", state)
	}
	if state := aggregate([]skills.ProbeOutcome{
		skills.ProbeCompletedOutOfBand, skills.ProbeUnchanged,
	}); state != dependency.StateWaiting {
		t.Fatalf("unchanged aggregate = %s", state)
	}
	for _, outcome := range []skills.ProbeOutcome{
		skills.ProbeReversed, skills.ProbeDrifted, skills.ProbeConflicted,
		skills.ProbeUnknown, "invalid",
	} {
		if state := aggregate([]skills.ProbeOutcome{outcome}); state != dependency.StateContested {
			t.Fatalf("aggregate %q = %s", outcome, state)
		}
	}
	service := &Service{now: time.Now}
	if _, err := service.Sweep(context.Background(), "organization"); err == nil {
		t.Fatal("non-UTC time source accepted")
	}
	service.now = reconcileTime
	if _, err := service.Sweep(context.Background(), ""); err == nil {
		t.Fatal("empty organization accepted")
	}
}

func TestIntegration_ReconciliationSweep_OpenCircuitBecomesUnknownBlock(t *testing.T) {
	ctx := context.Background()
	now := reconcileTime()
	tenant := "tenant:reconcile-open"
	organizationID := contracts.OrganizationID("organization:reconcile-open")
	nodeID := dependency.NodeID("node:reconcile-open")
	graph, err := dependency.New(reconcilePool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.PutNode(ctx, dependency.Node{
		ID: nodeID, OrganizationID: organizationID, Kind: dependency.NodeIntent,
		Title: "open circuit", State: dependency.StateLeased, BasePriority: 1,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(reconcilePool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := reconcileLeaseRequest("reconcile-open", organizationID, nodeID, now)
	authority, err := testauthority.New(
		reconcilePool, reconcileVault(t, tenant), tenant, organizationID,
		"reconcile-open", func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	published, err := authority.Publish(ctx, request, "skill:reconcile")
	if err != nil {
		t.Fatal(err)
	}
	request = published.Request
	grant, err := leaseStore.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Register(ctx, published, grant.Fence, "reconcile-open"); err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(reconcilePool, tenant, circuit.Config{
		FailureThreshold: 1, SuccessThreshold: 1, Window: time.Hour,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter := reconcileAdapter(t)
	gateway, err := effect.New(
		reconcilePool, reconcileVault(t, tenant), leaseStore, authority.Store(), breakers,
		tenant, approval.Authority{}, func() time.Time { return now }, adapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)}
	proposal := effect.Proposal{
		ID: "proposal:reconcile-open", OrganizationID: organizationID,
		IntentID: "intent:reconcile-open", NodeID: nodeID, SeatID: request.SeatID,
		LeaseID: request.ID, Fence: grant.Fence, Provider: adapter.Name(),
		SkillID: "skill:reconcile", EffectClass: skills.EffectReversible,
		Operation: "probe_unchanged", IdempotencyKey: "effect:reconcile-open",
		SkillDigest: hash, OperationDigest: hash, Input: []byte("payload"),
		Deadline: now.Add(time.Hour),
	}
	authorizeReconcileProposal(t, tenant, proposal)
	if _, err := gateway.Execute(ctx, proposal); !errors.Is(err, effect.ErrAmbiguous) {
		t.Fatal(err)
	}
	service, err := New(reconcilePool, gateway, graph, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	summary, err := service.Sweep(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Events) != 1 || summary.Events[0].Outcome != skills.ProbeUnknown ||
		summary.Events[0].SafeReason != "probe_circuit_open" ||
		summary.Blocked != 1 {
		t.Fatalf("open-circuit summary = %+v", summary)
	}
	if _, err := reconcilePool.Exec(ctx, `
		CREATE FUNCTION force_reconcile_event_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced reconcile event failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER force_reconcile_event_failure
		BEFORE INSERT ON workforce_reconciliation_events
		FOR EACH ROW WHEN (NEW.tenant_id='tenant:reconcile-open')
		EXECUTE FUNCTION force_reconcile_event_failure()
	`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = reconcilePool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS force_reconcile_event_failure
				ON workforce_reconciliation_events;
			DROP FUNCTION IF EXISTS force_reconcile_event_failure()
		`)
	})
	if _, err := service.Sweep(ctx, organizationID); err == nil {
		t.Fatal("reconciliation event persistence failure did not stop selection")
	}
}

func TestIntegration_ReconciliationInternalFailuresRemainClosed(t *testing.T) {
	ctx := context.Background()
	now := reconcileTime()
	tenant := "tenant:reconcile-paths"
	graph, err := dependency.New(reconcilePool, tenant, reconcileTime)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pool: reconcilePool, graph: graph, tenantID: tenant, now: reconcileTime,
	}
	event := Event{
		OrganizationID: "organization:reconcile-paths",
		ProposalID:     "proposal:path", IntentID: "intent:path", NodeID: "node:path",
		Outcome: skills.ProbeUnknown, EffectState: effect.StateExternallyAmbiguous,
		SafeReason: "unknown", ObservedAt: now,
	}
	if err := service.record(ctx, event); err != nil {
		t.Fatal(err)
	}
	conflict := event
	conflict.Outcome = skills.ProbeDrifted
	if err := service.record(ctx, conflict); err == nil {
		t.Fatal("reconciliation event identity conflict accepted")
	}
	if err := service.applyIntent(ctx, event.OrganizationID,
		"node:missing", []skills.ProbeOutcome{skills.ProbeUnknown}); err == nil {
		t.Fatal("missing graph node accepted")
	}

	closed, err := pgxpool.New(ctx,
		"postgres://postgres:password@127.0.0.1:1/database?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	closedGraph, err := dependency.New(closed, "tenant:closed-reconcile", reconcileTime)
	if err != nil {
		t.Fatal(err)
	}
	closedService := &Service{
		pool: closed, graph: closedGraph, tenantID: "tenant:closed-reconcile",
		now: reconcileTime,
	}
	if err := closedService.record(ctx, event); err == nil {
		t.Fatal("closed event store accepted write")
	}
	if err := closedService.applyIntent(ctx, event.OrganizationID,
		event.NodeID, []skills.ProbeOutcome{skills.ProbeUnknown}); err == nil {
		t.Fatal("closed graph accepted reconciliation")
	}

	leaseStore, err := lease.New(reconcilePool, "tenant:pending-error", reconcileTime)
	if err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(reconcilePool, "tenant:pending-error", circuit.Config{
		FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, reconcileTime)
	if err != nil {
		t.Fatal(err)
	}
	closedGateway, err := effect.New(
		closed, reconcileVault(t, "tenant:pending-error"), leaseStore,
		pendingPolicyAuthority(t).Store(), breakers,
		"tenant:pending-error", approval.Authority{}, reconcileTime,
		reconcileAdapter(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	pendingService, err := New(
		reconcilePool, closedGateway, graph, "tenant:pending-error", reconcileTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pendingService.Sweep(ctx, "organization:pending-error"); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("pending storage failure error = %v", err)
	}
}

func reconcileAdapter(t *testing.T) *effect.CommandAdapter {
	t.Helper()
	directory := t.TempDir()
	write := `file="$1/$WORKFORCE_IDEMPOTENCY_KEY"; cat >"$file"; exit 7`
	operations := make(map[string][]string)
	probes := make(map[string][]string)
	for _, outcome := range []skills.ProbeOutcome{
		skills.ProbeUnchanged, skills.ProbeCompletedOutOfBand, skills.ProbeReversed,
		skills.ProbeDrifted, skills.ProbeConflicted, skills.ProbeUnknown,
	} {
		label := string(outcome)
		if outcome == skills.ProbeCompletedOutOfBand {
			label = "completed"
		}
		operation := "probe_" + label
		operations[operation] = []string{"-c", write, "sh", directory}
		if outcome == skills.ProbeCompletedOutOfBand {
			probes[operation] = []string{"-c",
				`printf '{"outcome":"completed_out_of_band","observation":{"status":"done"}}'`}
		} else {
			probes[operation] = []string{"-c",
				`printf '{"outcome":"` + string(outcome) + `","reason":"observed"}'`}
		}
	}
	operations["probe_unavailable"] = []string{"-c", write, "sh", directory}
	probes["probe_unavailable"] = []string{"-c", "exit 9"}
	operations["probe_corrected"] = []string{"-c", write, "sh", directory}
	probes["probe_corrected"] = []string{"-c",
		`printf '{"outcome":"completed_out_of_band","observation":{"status":"done"}}'`}
	adapter, err := effect.NewCommandAdapter(
		"provider", "/bin/sh", operations, probes,
		[]string{"PATH=/usr/bin:/bin"}, directory, reconcileTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func reconcileLeaseRequest(
	label string,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
	now time.Time,
) lease.Request {
	return lease.Request{
		ID: contracts.LeaseID("lease:" + label), WakeID: contracts.WakeID("wake:" + label),
		OrganizationID: organizationID, SeatID: contracts.SeatID("seat:" + label),
		NodeID: nodeID, MandateID: contracts.MandateID("mandate:" + label),
		MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: contracts.PolicyID("policy:" + label), Version: 1,
			Hash: contracts.ContentHash{
				Algorithm: "sha256", Digest: strings.Repeat("a", 64),
			},
		}},
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func pendingPolicyAuthority(t *testing.T) *testauthority.Fixture {
	t.Helper()
	fixture, err := testauthority.New(
		reconcilePool, reconcileVault(t, "tenant:pending-error"),
		"tenant:pending-error", "organization:pending-error",
		"pending-error", reconcileTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

var (
	reconcileVaultMutex sync.Mutex
	reconcileVaults     = map[string]*vault.UserVault{}
)

// reconcileVault returns one stable Vault per tenant for the whole test binary,
// so records sealed by a fixture open under the same user key the service uses.
func reconcileVault(t *testing.T, tenant string) *vault.UserVault {
	t.Helper()
	reconcileVaultMutex.Lock()
	defer reconcileVaultMutex.Unlock()
	if existing, found := reconcileVaults[tenant]; found {
		return existing
	}
	directory, err := os.MkdirTemp("", "workforce-reconcile-vault-")
	if err != nil {
		t.Fatal(err)
	}
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: directory, UserDID: tenant, KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !session.Encrypting() || session.UserVault() == nil {
		t.Fatal("Vault did not initialize encryption")
	}
	reconcileVaults[tenant] = session.UserVault()
	return session.UserVault()
}

// authorizeReconcileProposal records the durable compiled plan a prior real
// compilation would have written for this exact proposal, sealed with the same
// tenant Vault the gateway opens.
func authorizeReconcileProposal(
	t *testing.T,
	tenant string,
	proposal effect.Proposal,
) {
	t.Helper()
	planHash := strings.Repeat("c", 64)
	encoded, err := json.Marshal(struct {
		PlanHash  contracts.ContentHash `json:"plan_hash"`
		Operation effect.Proposal       `json:"operation"`
	}{
		PlanHash:  contracts.ContentHash{Algorithm: "sha256", Digest: planHash},
		Operation: proposal,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := reconcileVault(t, tenant).SealRecord(vault.AD{
		User: tenant, Store: "workforce.compiled.plan",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconcilePool.Exec(context.Background(), `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING
	`, tenant, proposal.OrganizationID, proposal.ID, proposal.IntentID,
		proposal.SkillID, proposal.SkillDigest.Digest, proposal.OperationDigest.Digest,
		planHash, planHash, effect.ProposalHash(proposal), sealed,
		reconcileTime()); err != nil {
		t.Fatalf("authorize compiled reconcile proposal: %v", err)
	}
}

func reconcileTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startReconcilePostgres(ctx context.Context) (string, string, error) {
	command := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", reconcilePostgresImage)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start postgres: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("resolve postgres port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	separator := strings.LastIndex(address, ":")
	if separator < 0 {
		return containerID, "", fmt.Errorf("invalid postgres port output %q", address)
	}
	return containerID, "postgres://postgres:password@127.0.0.1:" +
		address[separator+1:] + "/workforce?sslmode=disable", nil
}

func waitReconcilePostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
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
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
