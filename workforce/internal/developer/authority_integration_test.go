package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/testauthority"
)

const developerPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var developerPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startDeveloperPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "developer integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	developerPool, err = waitDeveloperPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "developer integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, developerPool, developerNow()); err != nil {
		developerPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "developer migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	developerPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_DeveloperScopesConflictAndAllowIndependentRealChanges(t *testing.T) {
	executable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeDeveloperRepository(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database := filepath.Join(root, ".cg", "codegraph.db")
	output, err := exec.CommandContext(
		ctx, executable, "--db", database, "build", root,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("initialize real CodeGraph: %v: %s", err, output)
	}
	graph, err := projectbrain.NewCodeGraph(executable, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	tenant := "tenant:developer:" + strings.ReplaceAll(t.Name(), "/", ":")
	organizationID := contracts.OrganizationID("organization:developer")
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	rememberDeveloperVault(t, tenant, session.UserVault())
	policyAuthority := developerPolicyAuthority(t, tenant, organizationID)
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := projectbrain.New(
		developerPool, session.UserVault(), tenant, "kernel-authority",
		authorityPublic, policyAuthority.Store(), graph, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(developerPool, tenant, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthority(
		developerPool, leaseStore, graph, brain, tenant, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}

	firstRequest := developerLeaseRequest("first", organizationID, "task:first")
	secondRequest := developerLeaseRequest("second", organizationID, "task:second")
	insertDeveloperTask(t, tenant, organizationID, firstRequest.NodeID)
	insertDeveloperTask(t, tenant, organizationID, secondRequest.NodeID)
	firstRequest = insertDeveloperAuthority(t, tenant, firstRequest)
	secondRequest = insertDeveloperAuthority(t, tenant, secondRequest)
	firstScope := ScopeRequest{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:real", WorkspaceID: "workspace:real",
		TaskNodeID: firstRequest.NodeID, WorkspaceRoot: root,
		Files: []string{"a.go"}, Symbols: []string{"Alpha"},
	}
	bindDeveloperCapability(t, &firstScope, tenant, firstRequest, authorityPrivate)
	secondScope := ScopeRequest{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:real", WorkspaceID: "workspace:real",
		TaskNodeID: secondRequest.NodeID, WorkspaceRoot: root,
		Files: []string{"b.go"}, Symbols: []string{"Beta"},
	}
	bindDeveloperCapability(t, &secondScope, tenant, secondRequest, authorityPrivate)
	var wait sync.WaitGroup
	wait.Add(2)
	type result struct {
		grant Grant
		err   error
	}
	results := make(chan result, 2)
	go func() {
		defer wait.Done()
		grant, acquireErr := authority.Acquire(ctx, firstRequest, firstScope)
		results <- result{grant: grant, err: acquireErr}
	}()
	go func() {
		defer wait.Done()
		grant, acquireErr := authority.Acquire(ctx, secondRequest, secondScope)
		results <- result{grant: grant, err: acquireErr}
	}()
	wait.Wait()
	close(results)
	grants := make([]Grant, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatalf("non-conflicting concurrent acquire: %v", result.err)
		}
		registerDeveloperLease(t, tenant, result.grant.Lease)
		grants = append(grants, result.grant)
	}
	if len(grants) != 2 {
		t.Fatalf("non-conflicting grants = %d", len(grants))
	}
	if _, err := developerPool.Exec(ctx, `
		UPDATE workforce_developer_change_scopes SET scope_hash=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND lease_id=$4
	`, strings.Repeat("0", 64), tenant, organizationID, firstRequest.ID); err == nil {
		t.Fatal("durable Developer scope authority was mutable")
	}
	if _, err := developerPool.Exec(ctx, `
		DELETE FROM workforce_developer_change_claims
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, tenant, organizationID, firstRequest.ID); err == nil {
		t.Fatal("durable Developer resource claims were deletable")
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	developerAdapter, err := NewAdapter(
		authority, brain, []VerificationCommand{{
			ID: "go_test", Bubblewrap: bubblewrap, Executable: goExecutable,
			Arguments: []string{"test", "./..."},
			Timeout:   time.Minute,
		}}, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(developerPool, tenant, circuit.Config{
		FailureThreshold: 3, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := effect.New(
		developerPool, session.UserVault(), leaseStore,
		developerPolicyAuthority(t, tenant, organizationID).Store(), breakers, tenant,
		approval.Authority{}, developerNow, developerAdapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	grantsByTask := make(map[dependency.NodeID]Grant, len(grants))
	for _, grant := range grants {
		if err := authority.Authorize(ctx, grant); err != nil {
			t.Fatalf("authorize exact change scope: %v", err)
		}
		grantsByTask[grant.Scope.TaskNodeID] = grant
	}

	conflictRequest := developerLeaseRequest("conflict", organizationID, "task:conflict")
	insertDeveloperTask(t, tenant, organizationID, conflictRequest.NodeID)
	conflictRequest = insertDeveloperAuthority(t, tenant, conflictRequest)
	conflictScope := firstScope
	conflictScope.TaskNodeID = conflictRequest.NodeID
	bindDeveloperCapability(
		t, &conflictScope, tenant, conflictRequest, authorityPrivate,
	)
	if _, err := authority.Acquire(
		ctx, conflictRequest, conflictScope,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting implementation error = %v", err)
	}
	var conflictState string
	if err := developerPool.QueryRow(ctx, `
		SELECT state FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, tenant, organizationID, conflictRequest.ID).Scan(&conflictState); err != nil {
		t.Fatal(err)
	}
	if conflictState != "cancelled" {
		t.Fatalf("failed change scope left generic lease %q", conflictState)
	}

	forgedRequest := developerLeaseRequest("forged", organizationID, "task:forged")
	insertDeveloperTask(t, tenant, organizationID, forgedRequest.NodeID)
	forgedRequest = insertDeveloperAuthority(t, tenant, forgedRequest)
	forgedScope := firstScope
	forgedScope.TaskNodeID = forgedRequest.NodeID
	bindDeveloperCapability(
		t, &forgedScope, tenant, forgedRequest, authorityPrivate,
	)
	forgedScope.ProjectID = "project:relabelled"
	forgedScope.Capability.ProjectID = forgedScope.ProjectID
	if _, err := authority.Acquire(
		ctx, forgedRequest, forgedScope,
	); err == nil {
		t.Fatal("tampered signed capability relabelled the same workspace")
	}

	raceRequests := []lease.Request{
		developerLeaseRequest("race-one", organizationID, "task:race-one"),
		developerLeaseRequest("race-two", organizationID, "task:race-two"),
	}
	for index, request := range raceRequests {
		insertDeveloperTask(t, tenant, organizationID, request.NodeID)
		raceRequests[index] = insertDeveloperAuthority(t, tenant, request)
	}
	raceScopes := make([]ScopeRequest, len(raceRequests))
	for index, request := range raceRequests {
		raceScopes[index] = ScopeRequest{
			SchemaVersion: contracts.SchemaVersionV1,
			ProjectID:     "project:real", WorkspaceID: "workspace:real",
			TaskNodeID: request.NodeID, WorkspaceRoot: root,
			Files: []string{"c.go"}, Symbols: []string{"Gamma"},
		}
		bindDeveloperCapability(
			t, &raceScopes[index], tenant, request, authorityPrivate,
		)
	}
	raceResults := make(chan error, 2)
	for index, request := range raceRequests {
		request := request
		scope := raceScopes[index]
		go func() {
			_, acquireErr := authority.Acquire(ctx, request, scope)
			raceResults <- acquireErr
		}()
	}
	var raceSuccesses, raceConflicts int
	for index := 0; index < 2; index++ {
		switch err := <-raceResults; {
		case err == nil:
			raceSuccesses++
		case errors.Is(err, ErrConflict):
			raceConflicts++
		default:
			t.Fatalf("concurrent conflicting acquire: %v", err)
		}
	}
	if raceSuccesses != 1 || raceConflicts != 1 {
		t.Fatalf(
			"concurrent conflict successes=%d conflicts=%d",
			raceSuccesses, raceConflicts,
		)
	}

	blockedRequest := developerLeaseRequest("blocked", organizationID, "task:blocked")
	insertDeveloperTask(t, tenant, organizationID, blockedRequest.NodeID)
	blockedRequest = insertDeveloperAuthority(t, tenant, blockedRequest)
	prerequisite := dependency.NodeID("task:prerequisite")
	insertDeveloperTaskState(
		t, tenant, organizationID, prerequisite, dependency.StatePending,
	)
	if _, err := developerPool.Exec(ctx, `
		INSERT INTO workforce_work_edges (
			tenant_id,organization_id,prerequisite_node_id,dependent_node_id,
			edge_kind,created_at
		) VALUES ($1,$2,$3,$4,'dependency',$5)
	`, tenant, organizationID, prerequisite, blockedRequest.NodeID,
		developerNow().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	blockedScope := secondScope
	blockedScope.TaskNodeID = blockedRequest.NodeID
	bindDeveloperCapability(
		t, &blockedScope, tenant, blockedRequest, authorityPrivate,
	)
	if _, err := authority.Acquire(
		ctx, blockedRequest, blockedScope,
	); !errors.Is(err, ErrDependency) {
		t.Fatalf("blocked dependency acquire error = %v", err)
	}

	coordinatedRequests := []lease.Request{
		developerLeaseRequest("coordinated-one", organizationID, "task:coordinated-one"),
		developerLeaseRequest("coordinated-two", organizationID, "task:coordinated-two"),
	}
	for index, request := range coordinatedRequests {
		insertDeveloperTask(t, tenant, organizationID, request.NodeID)
		coordinatedRequests[index] = insertDeveloperAuthority(t, tenant, request)
	}
	planID := commitDeveloperCoordinationPlan(
		t, ctx, brain, graph, root, tenant, organizationID,
		coordinatedRequests, authorityPrivate,
	)
	coordinatedResults := make(chan error, 2)
	for _, request := range coordinatedRequests {
		request := request
		scope := ScopeRequest{
			SchemaVersion: contracts.SchemaVersionV1,
			ProjectID:     "project:real", WorkspaceID: "workspace:real",
			TaskNodeID: request.NodeID, WorkspaceRoot: root,
			Files: []string{"d.go"}, Symbols: []string{"Delta"},
			CoordinationPlanID: &planID,
		}
		bindDeveloperCapability(t, &scope, tenant, request, authorityPrivate)
		readGrant := developerCoordinationReadGrant(
			t, tenant, request, scope, planID, authorityPrivate,
		)
		scope.CoordinationGrant = &readGrant
		go func() {
			_, acquireErr := authority.Acquire(ctx, request, scope)
			coordinatedResults <- acquireErr
		}()
	}
	for range coordinatedRequests {
		if err := <-coordinatedResults; err != nil {
			t.Fatalf("verified coordination plan did not authorize overlap: %v", err)
		}
	}

	writeErrors := make(chan error, 2)
	go func() {
		proposal, proposalErr := developerEffectProposal(
			grantsByTask[firstRequest.NodeID], "first-change",
			[]SourceChange{{
				Path:       "a.go",
				BeforeHash: grantsByTask[firstRequest.NodeID].Scope.Files[0].Hash,
				Content:    []byte("package scoped\n\nfunc Alpha() int { return 2 }\n"),
			}},
		)
		if proposalErr == nil {
			proposalErr = authorizeDeveloperProposal(ctx, tenant, session.UserVault(), proposal)
		}
		if proposalErr == nil {
			_, proposalErr = gateway.Execute(ctx, proposal)
		}
		writeErrors <- proposalErr
	}()
	go func() {
		proposal, proposalErr := developerEffectProposal(
			grantsByTask[secondRequest.NodeID], "second-change",
			[]SourceChange{{
				Path:       "b.go",
				BeforeHash: grantsByTask[secondRequest.NodeID].Scope.Files[0].Hash,
				Content:    []byte("package scoped\n\nfunc Beta() int { return 3 }\n"),
			}},
		)
		if proposalErr == nil {
			proposalErr = authorizeDeveloperProposal(ctx, tenant, session.UserVault(), proposal)
		}
		if proposalErr == nil {
			_, proposalErr = gateway.Execute(ctx, proposal)
		}
		writeErrors <- proposalErr
	}()
	for index := 0; index < 2; index++ {
		if err := <-writeErrors; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := developerPool.Exec(ctx, `
		DELETE FROM workforce_developer_scope_events
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, tenant, organizationID, firstRequest.ID); err == nil {
		t.Fatal("Developer effect audit events were deletable")
	}
	if _, err := developerPool.Exec(ctx, `
		DELETE FROM workforce_developer_file_events
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, tenant, organizationID, firstRequest.ID); err == nil {
		t.Fatal("Developer file transition evidence was deletable")
	}
	if err := authority.Authorize(
		ctx, grantsByTask[firstRequest.NodeID],
	); err != nil {
		t.Fatalf("committed scoped bytes invalidated their own lease: %v", err)
	}
	if _, err := developerAdapter.runVerification(ctx, root, "go_test"); err != nil {
		t.Fatalf("real post-change verification failed before dispatch: %v", err)
	}
	verifyInput, err := json.Marshal(OperationEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Grant:         grantsByTask[firstRequest.NodeID],
		Verification:  "go_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyProposal := effect.Proposal{
		ID:             "proposal:verify-first",
		OrganizationID: organizationID,
		IntentID:       contracts.IntentID(firstRequest.NodeID),
		NodeID:         firstRequest.NodeID,
		SeatID:         firstRequest.SeatID,
		LeaseID:        firstRequest.ID,
		Fence:          grantsByTask[firstRequest.NodeID].Lease.Fence,
		Provider:       developerAdapterName,
		SkillID:        skills.DeveloperVerifySkill,
		EffectClass:    skills.EffectRead,
		Operation:      "run_verification",
		IdempotencyKey: "developer:verify-first",
		SkillDigest:    developerHash("skill:verify-first"),
		OperationDigest: developerHash(
			"operation:verify-first",
		),
		Input: verifyInput, Deadline: developerNow().Add(30 * time.Minute),
	}
	if err := authorizeDeveloperProposal(ctx, tenant, session.UserVault(), verifyProposal); err != nil {
		t.Fatal(err)
	}
	verifyResult, err := gateway.Execute(ctx, verifyProposal)
	if err != nil || verifyResult.State != effect.StateSucceeded {
		t.Fatalf("post-change fenced verification = %#v, %v", verifyResult, err)
	}
	restoreContent := []byte("package scoped\n\nfunc Alpha() int { return 1 }\n")
	restoreInput, err := json.Marshal(OperationEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Grant:         grantsByTask[firstRequest.NodeID],
		Changes: []SourceChange{{
			Path: "a.go",
			BeforeHash: developerHash(
				"package scoped\n\nfunc Alpha() int { return 2 }\n",
			),
			Content: restoreContent,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	restoreProposal := effect.Proposal{
		ID:             "proposal:restore-first",
		OrganizationID: organizationID,
		IntentID:       contracts.IntentID(firstRequest.NodeID),
		NodeID:         firstRequest.NodeID,
		SeatID:         firstRequest.SeatID,
		LeaseID:        firstRequest.ID,
		Fence:          grantsByTask[firstRequest.NodeID].Lease.Fence,
		Provider:       developerAdapterName,
		SkillID:        skills.DeveloperImplementSkill,
		EffectClass:    skills.EffectReversible,
		Operation:      "restore_source_snapshot",
		IdempotencyKey: "developer:restore-first",
		SkillDigest:    developerHash("skill:restore-first"),
		OperationDigest: developerHash(
			"operation:restore-first",
		),
		Input: restoreInput, Deadline: developerNow().Add(30 * time.Minute),
	}
	if err := authorizeDeveloperProposal(ctx, tenant, session.UserVault(), restoreProposal); err != nil {
		t.Fatal(err)
	}
	restoreResult, err := gateway.Execute(ctx, restoreProposal)
	if err != nil || restoreResult.State != effect.StateSucceeded {
		t.Fatalf("fenced source compensation = %#v, %v", restoreResult, err)
	}
	if err := authority.Authorize(
		ctx, grantsByTask[firstRequest.NodeID],
	); err != nil {
		t.Fatalf("restored scoped bytes invalidated lease: %v", err)
	}
	if _, err := authority.ApplyScopedChanges(
		ctx, grantsByTask[firstRequest.NodeID], "apply_scoped_change",
		[]SourceChange{{
			Path:       "c.go",
			BeforeHash: grantsByTask[firstRequest.NodeID].Scope.Files[0].Hash,
			Content:    []byte("package scoped\n\nfunc Gamma() int { return 9 }\n"),
		}},
	); err == nil {
		t.Fatal("fenced effect accepted a file outside the exact scope")
	}
	output, err = exec.CommandContext(
		ctx, executable, "--db", database, "build", root, "--incremental",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("sync real CodeGraph: %v: %s", err, output)
	}
	command := exec.CommandContext(ctx, "go", "test", "./...")
	command.Dir = root
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("verify concurrently changed real repository: %v: %s", err, output)
	}
}

func developerEffectProposal(
	grant Grant,
	label string,
	changes []SourceChange,
) (effect.Proposal, error) {
	input, err := json.Marshal(OperationEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Grant:         grant,
		Changes:       changes,
	})
	if err != nil {
		return effect.Proposal{}, err
	}
	return effect.Proposal{
		ID:              "proposal:" + label,
		OrganizationID:  grant.Lease.OrganizationID,
		IntentID:        contracts.IntentID(grant.Lease.NodeID),
		NodeID:          grant.Lease.NodeID,
		SeatID:          grant.Lease.SeatID,
		LeaseID:         grant.Lease.ID,
		Fence:           grant.Lease.Fence,
		Provider:        developerAdapterName,
		SkillID:         skills.DeveloperImplementSkill,
		EffectClass:     skills.EffectReversible,
		Operation:       "apply_scoped_change",
		IdempotencyKey:  "developer:" + label,
		SkillDigest:     developerHash("skill:" + label),
		OperationDigest: developerHash("operation:" + label),
		Input:           input,
		Deadline:        developerNow().Add(30 * time.Minute),
	}, nil
}

// authorizeDeveloperProposal records the durable compiled plan a prior real
// compilation would have written, sealed with the same tenant Vault the gateway
// opens, so dispatch authority is established exactly as it is in production.
func authorizeDeveloperProposal(
	ctx context.Context,
	tenant string,
	userVault *vault.UserVault,
	proposal effect.Proposal,
) error {
	hash := developerHash("compiled:" + proposal.ID)
	encoded, err := json.Marshal(struct {
		PlanHash  contracts.ContentHash `json:"plan_hash"`
		Operation effect.Proposal       `json:"operation"`
	}{PlanHash: hash, Operation: proposal})
	if err != nil {
		return err
	}
	sealed, err := userVault.SealRecord(vault.AD{
		User: tenant, Store: "workforce.compiled.plan",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, encoded)
	if err != nil {
		return err
	}
	_, err = developerPool.Exec(ctx, `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING
	`, tenant, proposal.OrganizationID, proposal.ID, proposal.IntentID,
		proposal.SkillID, proposal.SkillDigest.Digest, proposal.OperationDigest.Digest,
		hash.Digest, hash.Digest, effect.ProposalHash(proposal),
		sealed, developerNow())
	return err
}

func bindDeveloperCapability(
	t *testing.T,
	scope *ScopeRequest,
	tenant string,
	request lease.Request,
	authorityPrivate ed25519.PrivateKey,
) {
	t.Helper()
	seat := developerCurrentSeat(t, tenant, request.OrganizationID, request.SeatID)
	scope.Capability = projectbrain.CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "grant:" + string(request.ID),
		TenantID:      tenant, OrganizationID: request.OrganizationID,
		ProjectID: scope.ProjectID, WorkspaceID: scope.WorkspaceID,
		WorkspaceRoot:           scope.WorkspaceRoot,
		Operation:               projectbrain.CapabilityChangeScope,
		RequesterSeatID:         seat.ID,
		RequesterSeatVersion:    seat.Version,
		RequesterSeatDID:        seat.DID,
		RequesterBindingID:      seat.BindingID,
		RequesterBindingVersion: seat.BindingVersion,
		Purpose:                 "developer_change_scope:" + string(scope.TaskNodeID),
		IssuedAt:                developerNow().Add(-time.Minute),
		ExpiresAt:               developerNow().Add(time.Hour),
	}
	if err := projectbrain.SignCapabilityGrant(
		&scope.Capability, "kernel-authority", authorityPrivate,
	); err != nil {
		t.Fatal(err)
	}
}

func writeDeveloperRepository(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/scoped\n\ngo 1.25\n",
		"a.go":   "package scoped\n\nfunc Alpha() int { return 1 }\n",
		"b.go":   "package scoped\n\nfunc Beta() int { return 1 }\n",
		"c.go":   "package scoped\n\nfunc Gamma() int { return 1 }\n",
		"d.go":   "package scoped\n\nfunc Delta() int { return 1 }\n",
		"a_test.go": `package scoped

import "testing"

func TestAlpha(t *testing.T) {
	if Alpha() <= 0 {
		t.Fatal("Alpha must stay positive")
	}
}
`,
		"b_test.go": `package scoped

import "testing"

func TestBeta(t *testing.T) {
	if Beta() <= 0 {
		t.Fatal("Beta must stay positive")
	}
}
`,
		"c_test.go": `package scoped

import "testing"

func TestGamma(t *testing.T) {
	if Gamma() <= 0 {
		t.Fatal("Gamma must stay positive")
	}
}
`,
		"d_test.go": `package scoped

import "testing"

func TestDelta(t *testing.T) {
	if Delta() <= 0 {
		t.Fatal("Delta must stay positive")
	}
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func commitDeveloperCoordinationPlan(
	t *testing.T,
	ctx context.Context,
	brain *projectbrain.Store,
	graph *projectbrain.CodeGraph,
	root, tenant string,
	organizationID contracts.OrganizationID,
	requests []lease.Request,
	authorityPrivate ed25519.PrivateKey,
) projectbrain.RecordID {
	t.Helper()
	source, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	authorPublic, authorPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierPublic, verifierPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fileEvidence := make([]projectbrain.FileEvidence, 0, len(source.Files))
	for _, file := range source.Files {
		fileEvidence = append(fileEvidence, projectbrain.FileEvidence{
			Path: file.Path, Hash: file.Hash, StartLine: 1, EndLine: 1,
		})
	}
	claims := make([]projectbrain.Claim, 0, len(requests)*2)
	for _, request := range requests {
		claims = append(claims,
			projectbrain.Claim{
				Statement: "coordinate_task:" + string(request.NodeID),
				Files:     fileEvidence,
			},
			projectbrain.Claim{
				Statement: "coordinate_seat:" + string(request.SeatID),
				Files:     fileEvidence,
			},
		)
	}
	expires := developerNow().Add(45 * time.Minute)
	proposal := projectbrain.Proposal{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "plan:coordinated-delta", OrganizationID: organizationID,
		ProjectID: "project:real", WorkspaceID: "workspace:real",
		AuthorSeatID: "seat:plan-author", ParentIntentID: "intent:coordination",
		Kind: projectbrain.KindPlan, Origin: projectbrain.OriginSource, Version: 1,
		Source: source,
		Content: projectbrain.Content{
			Summary: "coordinate two exact Delta implementation scopes",
			Claims:  claims, ExpiresAt: &expires,
		},
		CreatedAt: developerNow().Add(-time.Minute),
	}
	if err := projectbrain.SignProposal(&proposal, "plan-author-key", authorPrivate); err != nil {
		t.Fatal(err)
	}
	proposalHash, err := projectbrain.ProposalHash(proposal)
	if err != nil {
		t.Fatal(err)
	}
	verification := projectbrain.Verification{
		SchemaVersion: contracts.SchemaVersionV1,
		RecordID:      proposal.ID, VerifierSeatID: "seat:plan-verifier",
		ProposalHash: proposalHash, Accepted: true,
		Procedure: "developer-coordination-plan.v1",
		Evidence: []contracts.EvidenceRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:coordination-plan", Hash: developerHash("coordination-plan"),
			Kind: "independent_review", ObservedAt: developerNow(),
		}},
		VerifiedAt: developerNow(),
	}
	if err := projectbrain.SignVerification(
		&verification, "plan-verifier-key", verifierPrivate,
	); err != nil {
		t.Fatal(err)
	}
	record := projectbrain.EngineeringRecord{
		Proposal: proposal, Verification: verification,
	}
	authorSeat := publishDeveloperProjectBrainSeat(
		t, tenant, organizationID, proposal.AuthorSeatID,
	)
	verifierSeat := publishDeveloperProjectBrainSeat(
		t, tenant, organizationID, verification.VerifierSeatID,
	)
	recordID := proposal.ID
	writeGrant := projectbrain.CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "grant:coordination-plan-write", TenantID: tenant,
		OrganizationID: organizationID,
		ProjectID:      proposal.ProjectID, WorkspaceID: proposal.WorkspaceID,
		WorkspaceRoot: root, Operation: projectbrain.CapabilityWrite,
		RequesterSeatID:         authorSeat.ID,
		RequesterSeatVersion:    authorSeat.Version,
		RequesterSeatDID:        authorSeat.DID,
		RequesterBindingID:      authorSeat.BindingID,
		RequesterBindingVersion: authorSeat.BindingVersion,
		RecordID:                &recordID,
		Author: developerProjectBrainKeyBinding(
			authorSeat, proposal.Signature.KeyID, authorPublic,
		),
		Verifier: developerProjectBrainKeyBinding(
			verifierSeat, verification.Signature.KeyID, verifierPublic,
		),
		Purpose:  "verified-developer-coordination",
		IssuedAt: developerNow().Add(-time.Minute), ExpiresAt: developerNow().Add(time.Hour),
	}
	if err := projectbrain.SignCapabilityGrant(
		&writeGrant, "kernel-authority", authorityPrivate,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := brain.Commit(ctx, record, writeGrant); err != nil {
		t.Fatal(err)
	}
	return recordID
}

func developerCoordinationReadGrant(
	t *testing.T,
	tenant string,
	request lease.Request,
	scope ScopeRequest,
	planID projectbrain.RecordID,
	authorityPrivate ed25519.PrivateKey,
) projectbrain.CapabilityGrant {
	t.Helper()
	seat := developerCurrentSeat(t, tenant, request.OrganizationID, request.SeatID)
	grant := projectbrain.CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "grant:coordination:" + string(request.ID), TenantID: tenant,
		OrganizationID: request.OrganizationID,
		ProjectID:      scope.ProjectID, WorkspaceID: scope.WorkspaceID,
		WorkspaceRoot: scope.WorkspaceRoot, Operation: projectbrain.CapabilityRead,
		RequesterSeatID:         seat.ID,
		RequesterSeatVersion:    seat.Version,
		RequesterSeatDID:        seat.DID,
		RequesterBindingID:      seat.BindingID,
		RequesterBindingVersion: seat.BindingVersion,
		MaxRecords:              64,
		Purpose:                 "developer_coordination:" + string(planID),
		IssuedAt:                developerNow().Add(-time.Minute), ExpiresAt: developerNow().Add(time.Hour),
	}
	if err := projectbrain.SignCapabilityGrant(
		&grant, "kernel-authority", authorityPrivate,
	); err != nil {
		t.Fatal(err)
	}
	return grant
}

func developerLeaseRequest(
	label string,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
) lease.Request {
	return lease.Request{
		ID:             contracts.LeaseID("lease:" + label),
		WakeID:         contracts.WakeID("wake:" + label),
		OrganizationID: organizationID,
		SeatID:         contracts.SeatID("seat:" + label),
		NodeID:         nodeID,
		MandateID:      contracts.MandateID("mandate:" + label),
		MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: contracts.PolicyID("policy:" + label), Version: 1,
			Hash: developerHash("policy-" + label),
		}},
		IssuedAt:  developerNow().Add(-time.Minute),
		ExpiresAt: developerNow().Add(time.Hour),
	}
}

func insertDeveloperTask(
	t *testing.T,
	tenant string,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
) {
	t.Helper()
	insertDeveloperTaskState(
		t, tenant, organizationID, nodeID, dependency.StateEligible,
	)
}

func insertDeveloperTaskState(
	t *testing.T,
	tenant string,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
	state dependency.NodeState,
) {
	t.Helper()
	_, err := developerPool.Exec(context.Background(), `
		INSERT INTO workforce_work_nodes (
			tenant_id,organization_id,node_id,node_kind,title,state,base_priority,
			created_at,updated_at,contested,version
		) VALUES ($1,$2,$3,'intent',$3,$4,0,$5,$5,FALSE,1)
	`, tenant, organizationID, nodeID, state, developerNow().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
}

func insertDeveloperAuthority(
	t *testing.T,
	tenant string,
	request lease.Request,
) lease.Request {
	t.Helper()
	fixture := developerPolicyAuthority(t, tenant, request.OrganizationID)
	published, err := fixture.Publish(
		context.Background(), request,
		skills.DeveloperImplementSkill, skills.DeveloperVerifySkill,
	)
	if err != nil {
		t.Fatal(err)
	}
	developerAuthorityMutex.Lock()
	developerPublishedLeases[tenant+"|"+string(request.ID)] = published
	developerAuthorityMutex.Unlock()
	return published.Request
}

func registerDeveloperLease(t *testing.T, tenant string, grant lease.Grant) contracts.WakeLease {
	t.Helper()
	key := tenant + "|" + string(grant.ID)
	developerAuthorityMutex.Lock()
	published, ok := developerPublishedLeases[key]
	developerAuthorityMutex.Unlock()
	if !ok {
		t.Fatalf("no published signed authority for %s", grant.ID)
	}
	fixture := developerPolicyAuthority(t, tenant, grant.OrganizationID)
	value, err := fixture.Register(context.Background(), published, grant.Fence, string(grant.ID))
	if err != nil {
		t.Fatal(err)
	}
	developerAuthorityMutex.Lock()
	developerRegisteredLeases[key] = value
	developerAuthorityMutex.Unlock()
	return value
}

func developerPolicyAuthority(
	t *testing.T,
	tenant string,
	organizationID contracts.OrganizationID,
) *testauthority.Fixture {
	t.Helper()
	key := tenant + "|" + string(organizationID)
	developerAuthorityMutex.Lock()
	fixture := developerAuthorityFixtures[key]
	developerAuthorityMutex.Unlock()
	if fixture != nil {
		return fixture
	}
	userVault := registeredDeveloperVault(t, tenant)
	created, err := testauthority.New(
		developerPool, userVault, tenant, organizationID,
		"developer:"+tenant, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	developerAuthorityMutex.Lock()
	if fixture = developerAuthorityFixtures[key]; fixture == nil {
		developerAuthorityFixtures[key] = created
		fixture = created
	}
	developerAuthorityMutex.Unlock()
	return fixture
}

func developerCurrentSeat(
	t *testing.T,
	tenant string,
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
) contracts.Seat {
	t.Helper()
	fixture := developerPolicyAuthority(t, tenant, organizationID)
	seat, err := fixture.Store().LoadCurrentSeat(context.Background(), seatID)
	if err != nil {
		t.Fatalf("load current developer seat %s: %v", seatID, err)
	}
	return seat
}

func publishDeveloperProjectBrainSeat(
	t *testing.T,
	tenant string,
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
) contracts.Seat {
	t.Helper()
	fixture := developerPolicyAuthority(t, tenant, organizationID)
	if seat, err := fixture.Store().LoadCurrentSeat(context.Background(), seatID); err == nil {
		return seat
	}
	request := lease.Request{
		ID:             contracts.LeaseID("lease:projectbrain:" + string(seatID)),
		WakeID:         contracts.WakeID("wake:projectbrain:" + string(seatID)),
		OrganizationID: organizationID,
		SeatID:         seatID,
		NodeID:         dependency.NodeID("intent:projectbrain:" + string(seatID)),
		MandateID:      contracts.MandateID("mandate:projectbrain:" + string(seatID)),
		MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID:      contracts.PolicyID("policy:projectbrain:" + string(seatID)),
			Version: 1,
		}},
		IssuedAt:  developerNow().Add(-time.Minute),
		ExpiresAt: developerNow().Add(time.Hour),
	}
	published, err := fixture.Publish(
		context.Background(), request, skills.DeveloperVerifySkill,
	)
	if err != nil {
		t.Fatalf("publish current Project Brain seat %s: %v", seatID, err)
	}
	return published.Seat
}

func developerProjectBrainKeyBinding(
	seat contracts.Seat,
	keyID string,
	publicKey ed25519.PublicKey,
) *projectbrain.SeatKeyBinding {
	return &projectbrain.SeatKeyBinding{
		SeatID: seat.ID, SeatVersion: seat.Version, SeatDID: seat.DID,
		BindingID: seat.BindingID, BindingVersion: seat.BindingVersion,
		KeyID:     keyID,
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	}
}

func rememberDeveloperVault(
	t *testing.T,
	tenant string,
	userVault *vault.UserVault,
) {
	t.Helper()
	if userVault == nil {
		t.Fatal("developer Vault is required")
	}
	developerAuthorityMutex.Lock()
	developerUserVaults[tenant] = userVault
	developerAuthorityMutex.Unlock()
}

func registeredDeveloperVault(t *testing.T, tenant string) *vault.UserVault {
	t.Helper()
	developerAuthorityMutex.Lock()
	defer developerAuthorityMutex.Unlock()
	userVault := developerUserVaults[tenant]
	if userVault == nil {
		t.Fatalf("no developer Vault registered for %s", tenant)
	}
	return userVault
}

func developerPublishedAuthority(
	t *testing.T,
	tenant string,
	leaseID contracts.LeaseID,
) (testauthority.Published, contracts.WakeLease) {
	t.Helper()
	key := tenant + "|" + string(leaseID)
	developerAuthorityMutex.Lock()
	defer developerAuthorityMutex.Unlock()
	published, publishedOK := developerPublishedLeases[key]
	registered, registeredOK := developerRegisteredLeases[key]
	if !publishedOK || !registeredOK {
		t.Fatalf("incomplete signed authority for %s", leaseID)
	}
	return published, registered
}

var (
	developerAuthorityMutex    sync.Mutex
	developerAuthorityFixtures = map[string]*testauthority.Fixture{}
	developerPublishedLeases   = map[string]testauthority.Published{}
	developerRegisteredLeases  = map[string]contracts.WakeLease{}
	developerUserVaults        = map[string]*vault.UserVault{}
)

func developerHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func developerNow() time.Time {
	return developerClock
}

var developerClock = time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)

func startDeveloperPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-developer-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		developerPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(
		ctx, "docker", "port", containerID, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return containerID, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	_, port, found := strings.Cut(address, ":")
	if !found {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL mapping %q", address)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			port + "/workforce?sslmode=disable",
		nil
}

func waitDeveloperPostgres(
	ctx context.Context,
	databaseURL string,
) (*pgxpool.Pool, error) {
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
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
