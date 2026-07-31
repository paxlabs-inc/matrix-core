package wakeruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	neoprovider "matrix/neo/provider"
	"matrix/vault"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/audit"
	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/controlapi"
	"matrix/workforce/internal/departmentadapter"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/developer"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/execution"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/lineage"
	"matrix/workforce/internal/mail"
	"matrix/workforce/internal/modelclient"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workcompile"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

const wakeRuntimePostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

func TestIntegration_RecoveredProviderEvidenceCompletesOneRealDeveloperWake(
	t *testing.T,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	fixture := newWakeRuntimeFixture(t, ctx)
	order := fixture.developerOrder()
	if err := workorder.Sign(
		&order, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.CreateWorkOrder(
		ctx, fixture.principal, order,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.runner.Orders.LoadContext(
		ctx, fixture.principal.OrganizationID,
		contracts.IntentID(created.IntentIDs[0]),
	); err != nil {
		t.Fatalf(
			"load committed Work Order: %v (%s)",
			err, fixture.diagnoseWorkOrderIntegrity(
				t, ctx, order, created.IntentIDs[0],
			),
		)
	}
	claims, err := fixture.scheduler.ClaimDue(
		ctx, string(fixture.principal.OrganizationID), 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Envelope.WakeID != created.WakeID {
		t.Fatalf("claimed wakes = %#v, created = %#v", claims, created)
	}
	claim := claims[0]

	firstErr := fixture.runner.RunClaim(ctx, claim)
	if firstErr == nil || !isRetryableWake(firstErr) {
		t.Fatalf("unavailable real provider boundary = %v", firstErr)
	}
	packet, found, err := fixture.runner.openPacket(
		ctx, fixture.principal.OrganizationID,
		contracts.WakeID(claim.Envelope.WakeID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("retryable provider failure did not preserve the WorkPacket")
	}
	checkpoint, err := fixture.runner.Execution.Load(
		ctx, fixture.principal.OrganizationID, packet.Lease.WakeID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Stage != execution.StagePropose {
		t.Fatalf("provider crash checkpoint = %#v", checkpoint)
	}

	modelOutput, err := json.Marshal(map[string]any{
		"schema_version": contracts.SchemaVersionV1,
		"disposition":    contracts.DispositionProgressed,
		"proposal": map[string]any{
			"skill_id":  skills.DeveloperImplementSkill,
			"operation": "inspect_source",
			"provider":  "developer",
			"input":     map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.runner.Lineage.PutModelEvidence(
		ctx, lineage.ModelExchange{
			ID:             contracts.EvidenceID("model:" + claim.Envelope.WakeID),
			OrganizationID: fixture.principal.OrganizationID,
			WakeID:         packet.Lease.WakeID,
			Model:          packet.Lease.Model,
			MGS:            packet.Lease.MGS,
			Runtime:        packet.Lease.Runtime,
			Request:        []byte(`{"captured_provider_request":true}`),
			Response:       []byte(`{"captured_provider_response":true}`),
			Output:         modelOutput,
			ReplayRequired: true,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.RunClaim(ctx, claim); err != nil {
		t.Fatalf("recover immutable provider exchange: %v", err)
	}
	if err := fixture.runner.RunClaim(ctx, claim); err != nil {
		t.Fatalf("exact terminal replay: %v", err)
	}
	fixture.assertCompletedOnce(t, ctx, claim, created)
}

func TestIntegration_LiveMiMoCompletesOneRealDeveloperWake(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("XIAOMI_API_KEY"))
	}
	if apiKey == "" {
		t.Skip("set MIMO_API_KEY or XIAOMI_API_KEY to run the live MiMo wake proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	fixture := newWakeRuntimeFixture(t, ctx)
	model, err := modelclient.New(modelclient.Config{
		Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		ModelVersion: neoprovider.MiMoV25ProModel,
		Endpoint:     neoprovider.MiMoChatEndpoint, APIKey: apiKey,
		Temperature: neoprovider.MiMoTemperature,
		MaxTokens:   4096, Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.runner.Model = model
	order := fixture.developerOrder()
	if err := workorder.Sign(&order, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.CreateWorkOrder(ctx, fixture.principal, order)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := fixture.scheduler.ClaimDue(
		ctx, string(fixture.principal.OrganizationID), 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Envelope.WakeID != created.WakeID {
		t.Fatalf("claimed wakes = %#v, created = %#v", claims, created)
	}
	if err := fixture.runner.RunClaim(ctx, claims[0]); err != nil {
		t.Fatal(err)
	}
	fixture.assertCompletedOnce(t, ctx, claims[0], created)
}

func TestIntegration_LiveMiMoCompletesSevenDepartmentReceiptChain(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("XIAOMI_API_KEY"))
	}
	if apiKey == "" {
		t.Skip("set MIMO_API_KEY or XIAOMI_API_KEY to run the live MiMo chain proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fixture := newWakeRuntimeFixture(t, ctx)
	model, err := modelclient.New(modelclient.Config{
		Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		ModelVersion: neoprovider.MiMoV25ProModel,
		Endpoint:     neoprovider.MiMoChatEndpoint, APIKey: apiKey,
		Temperature: neoprovider.MiMoTemperature,
		MaxTokens:   4096, Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.runner.Model = model
	order := fixture.sevenDepartmentOrder()
	if err := workorder.Sign(&order, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.CreateWorkOrder(ctx, fixture.principal, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.IntentIDs) != len(order.Departments) {
		t.Fatalf("created intents = %d", len(created.IntentIDs))
	}
	for index, department := range order.Departments {
		claims, claimErr := fixture.scheduler.ClaimDue(
			ctx, string(fixture.principal.OrganizationID), 1,
		)
		if claimErr != nil || len(claims) != 1 {
			t.Fatalf("department %s claim = %#v, %v", department, claims, claimErr)
		}
		claim := claims[0]
		if claim.Envelope.GraphScope != created.IntentIDs[index] {
			t.Fatalf(
				"department %s claimed intent %q, want %q",
				department, claim.Envelope.GraphScope, created.IntentIDs[index],
			)
		}
		if runErr := fixture.runner.RunClaim(ctx, claim); runErr != nil {
			_, output, outputErr := fixture.runner.Lineage.OpenModelOutput(
				ctx, fixture.principal.OrganizationID,
				contracts.EvidenceID("model:"+claim.Envelope.WakeID),
			)
			t.Fatalf(
				"department %s wake %s: %v model_output=%s model_error=%v",
				department, claim.Envelope.WakeID, runErr, output, outputErr,
			)
		}
		checkpoint, loadErr := fixture.runner.Execution.Load(
			ctx, fixture.principal.OrganizationID,
			contracts.WakeID(claim.Envelope.WakeID),
		)
		if loadErr != nil || checkpoint.Stage != execution.StageSleep ||
			checkpoint.ReceiptID == "" {
			t.Fatalf("department %s checkpoint = %#v, %v", department, checkpoint, loadErr)
		}
		wantDisposition := contracts.DispositionProgressed
		if index == len(order.Departments)-1 {
			wantDisposition = contracts.DispositionGoalCompleted
		}
		if checkpoint.Disposition != wantDisposition {
			t.Fatalf(
				"department %s disposition = %q, want %q",
				department, checkpoint.Disposition, wantDisposition,
			)
		}
		receipt, openErr := fixture.runner.Lineage.OpenReceipt(
			ctx, fixture.principal.OrganizationID, checkpoint.ReceiptID,
		)
		if openErr != nil || receipt.ParentIntentID != contracts.IntentID(created.IntentIDs[index]) ||
			receipt.Model.Provider != "mimo" ||
			receipt.Model.ModelID != neoprovider.MiMoV25ProModel {
			t.Fatalf("department %s receipt = %#v, %v", department, receipt, openErr)
		}
	}
	for table, expected := range map[string]int{
		"workforce_model_evidence":     7,
		"workforce_effect_operations":  7,
		"workforce_verdict_records":    7,
		"workforce_execution_receipts": 7,
	} {
		var count int
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE tenant_id=$1 AND organization_id=$2`, table)
		if err := fixture.pool.QueryRow(
			ctx, query, fixture.principal.TenantID,
			fixture.principal.OrganizationID,
		).Scan(&count); err != nil || count != expected {
			t.Fatalf("%s rows = %d, want %d: %v", table, count, expected, err)
		}
	}
}

type wakeRuntimeFixture struct {
	pool          *pgxpool.Pool
	service       *controlapi.Service
	principal     controlapi.Principal
	ownerKeyID    string
	ownerPrivate  ed25519.PrivateKey
	ownerPublic   ed25519.PublicKey
	userVault     *vault.UserVault
	scheduler     *scheduler.Store
	runner        *Runner
	now           time.Time
	workspaceRoot string
	auditorSeatID contracts.SeatID
}

func newWakeRuntimeFixture(
	t *testing.T,
	ctx context.Context,
) wakeRuntimeFixture {
	t.Helper()
	container, databaseURL, err := startWakeRuntimePostgres(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	})
	pool, err := waitWakeRuntimePostgres(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Now().UTC().Add(2 * time.Second)
	if err := ledger.ApplyMigrations(ctx, pool, now); err != nil {
		t.Fatal(err)
	}

	tenantID := "tenant:wakeruntime-recovery"
	principal := controlapi.Principal{
		TenantID: tenantID,
		OrganizationID: contracts.OrganizationID(
			"organization:wakeruntime-recovery",
		),
		OwnerID: "owner:wakeruntime-recovery",
	}
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimePublic, runtimePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerKeyID := "key:owner:wakeruntime-recovery"
	runtimeKeyID := "key:runtime:wakeruntime-recovery"
	authenticator, err := controlapi.NewStaticAuthenticator(
		map[string]controlapi.Principal{"token:wakeruntime-recovery": principal},
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := controlapi.New(
		pool, authenticator,
		map[string]controlapi.OwnerKey{
			tenantID: {KeyID: ownerKeyID, PublicKey: ownerPublic},
		},
		func() time.Time { return now }, 32,
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID,
		KEKHex: hex.EncodeToString(
			[]byte("0123456789abcdef0123456789abcdef"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	userVault := session.UserVault()
	if err := service.AttachVault(userVault); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeAuthority(
		runtimeKeyID, runtimePublic,
	); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeModel("mimo", neoprovider.MiMoV25ProModel); err != nil {
		t.Fatal(err)
	}
	schedulerStore, err := scheduler.New(
		pool, userVault, tenantID,
		scheduler.Config{
			MaxOrganizationConcurrency: 4,
			MaxSeatConcurrency:         1,
			DailyTaskLimit:             100,
			DailySpendMicrounits:       1_000_000,
			ClaimLease:                 2 * time.Minute,
		},
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachScheduler(schedulerStore); err != nil {
		t.Fatal(err)
	}
	auditorSeatID := activateWakeRuntimeOrganization(
		t, ctx, service, principal, ownerKeyID, ownerPrivate, now,
	)

	moduleRoot := wakeRuntimeModuleRoot(t)
	bubblewrap := wakeRuntimeExecutable(t, "bwrap")
	codegraph := wakeRuntimeExecutable(t, "cg")
	workspaceRoot := buildWakeRuntimeCodeGraph(t, ctx, codegraph)
	codeGraphInfo, err := os.Stat(
		filepath.Join(workspaceRoot, ".cg", "codegraph.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	now = codeGraphInfo.ModTime().UTC().Add(time.Second)
	seatBinary := filepath.Join(t.TempDir(), "workforce-seat")
	auditorBinary := filepath.Join(t.TempDir(), "workforce-auditor")
	buildWakeRuntimeBinary(
		t, ctx, moduleRoot, seatBinary, "./cmd/workforce-seat",
	)
	buildWakeRuntimeBinary(
		t, ctx, moduleRoot, auditorBinary, "./cmd/workforce-auditor",
	)

	authority, err := policy.New(
		pool, userVault, policy.OwnerRoot{
			TenantID: tenantID, OrganizationID: principal.OrganizationID,
			OwnerID: principal.OwnerID, KeyID: ownerKeyID,
			PublicKey: ownerPublic,
		}, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	skillStore, err := skills.NewStore(
		pool, userVault, tenantID, principal.OrganizationID,
		ownerKeyID, ownerPublic, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := skills.WorkforcePack()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.NewCatalog(pack)
	if err != nil {
		t.Fatal(err)
	}
	graphStore, err := dependency.New(
		pool, tenantID, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(
		pool, tenantID, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	ledgerStore, err := ledger.New(
		pool, userVault, tenantID, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	mailStore, err := mail.New(
		pool, userVault, graphStore, tenantID, mail.Config{
			MaxMailboxMessages: 10000, MaxThreadMessages: 1000,
			MaxThreadDepth: 64, MaxRecipients: 64, MaxAutoReplies: 100,
			MaxAttachmentBytes: 64 << 20,
			MaxMessageLifetime: 90 * 24 * time.Hour,
		}, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	codeGraph, err := projectbrain.NewCodeGraph(
		codegraph, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := projectbrain.New(
		pool, userVault, tenantID, runtimeKeyID, runtimePublic,
		authority, codeGraph, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler, err := actorstate.NewAssembler(
		graphStore, ledgerStore, mailStore, authority, leaseStore,
		catalog, brain, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	orderStore, err := workorder.NewStore(
		pool, userVault, tenantID, ownerKeyID, ownerPublic,
	)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := workcompile.New(
		pool, userVault, tenantID, skillStore, authority, leaseStore,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	executionStore, err := execution.New(
		pool, userVault, tenantID, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	lineageStore, err := lineage.New(
		pool, userVault, tenantID, runtimeKeyID, runtimePrivate,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	auditStore, err := audit.NewStore(
		pool, userVault, tenantID, runtimeKeyID, runtimePrivate,
		authority, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(
		pool, tenantID, circuit.Config{
			FailureThreshold: 3, SuccessThreshold: 2,
			Window: 5 * time.Minute, OpenDuration: time.Minute,
			HalfOpenLimit: 1, TrialDuration: 30 * time.Second,
		}, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	developerAuthority, err := developer.NewAuthority(
		pool, leaseStore, codeGraph, brain, tenantID,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	developerAdapter, err := developer.NewAdapter(
		developerAuthority, brain,
		[]developer.VerificationCommand{{
			ID: "codegraph-stats", Bubblewrap: bubblewrap,
			Executable: codegraph,
			Arguments: []string{
				"stats", "--repo", filepath.Base(workspaceRoot),
			},
			Timeout: 30 * time.Second,
		}},
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	approvalStore, err := approval.New(
		pool, userVault, tenantID, principal.OrganizationID,
		contracts.OwnerID(principal.OwnerID), ownerKeyID, ownerPublic,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	departmentAdapters, err := departmentadapter.New(
		func() time.Time { return now }, approvalStore,
	)
	if err != nil {
		t.Fatal(err)
	}
	adapters := append([]effect.Adapter{developerAdapter}, departmentAdapters...)
	effectGateway, err := effect.New(
		pool, userVault, leaseStore, authority, breakers, tenantID,
		approval.Authority{
			OwnerID: principal.OwnerID, KeyID: ownerKeyID,
			PublicKey: ownerPublic,
		},
		func() time.Time { return now }, adapters...,
	)
	if err != nil {
		t.Fatal(err)
	}
	model, err := modelclient.New(modelclient.Config{
		Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		ModelVersion: neoprovider.MiMoV25ProModel,
		Endpoint:     "http://127.0.0.1:1/v1/chat/completions",
		APIKey:       "unavailable-provider-boundary",
		Temperature:  neoprovider.MiMoTemperature,
		MaxTokens:    256, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	seatDigest := wakeRuntimeFileDigest(t, seatBinary)
	auditorDigest := wakeRuntimeFileDigest(t, auditorBinary)
	runtime := contracts.RuntimeBinding{
		BuildDigest: seatDigest, AuditorBuildDigest: auditorDigest,
		OperationRegistryDigest: catalog.Digest(),
	}
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Scheduler: schedulerStore, Graph: graphStore, Orders: orderStore,
		Authority: authority, Leases: leaseStore, Assembler: assembler,
		Seat: actorstate.Runner{
			Bubblewrap: bubblewrap, Binary: seatBinary,
		},
		Model: model, Compiler: compiler, Effects: effectGateway,
		Execution: executionStore, Lineage: lineageStore,
		Auditor: audit.Runner{
			Bubblewrap: bubblewrap, Binary: auditorBinary,
			DeveloperAuthorityKeyID: runtimeKeyID,
			DeveloperAuthorityKey:   runtimePublic,
		},
		Audits: auditStore, Catalog: catalog, SkillStore: skillStore,
		Developer:    developerAuthority,
		RuntimeKeyID: runtimeKeyID, RuntimeKey: runtimePrivate,
		Runtime: runtime, TenantID: tenantID, AuditorSeatID: auditorSeatID,
		LeaseDuration: 45 * time.Minute,
		WakeBudget: contracts.WakeBudget{
			MaxDurationMillis: uint64((45 * time.Minute) / time.Millisecond),
			MaxSteps:          25, MaxModelCalls: 25, MaxToolCalls: 25,
			MaxCostMinor: 1_000_000, Currency: "USD",
			MaxOutputBytes: 1 << 20,
		},
		ReplayEvidence: true, Now: func() time.Time { return now },
		PublishLifecycle: func(
			publishCtx context.Context,
			resourceID, kind string,
			verified bool,
			receiptID contracts.ReceiptID,
			fields map[string]any,
		) error {
			_, publishErr := service.Publish(
				publishCtx, principal, controlapi.LifecycleEvent{
					ID:             "event:" + kind + ":" + resourceID,
					OrganizationID: principal.OrganizationID,
					Type:           kind, ResourceKind: "wake",
					ResourceID: resourceID, ResourceVersion: 1,
					VerifiedCompletion: verified, ReceiptID: receiptID,
					Fields: fields,
				},
			)
			return publishErr
		},
	}
	if err := runner.Validate(); err != nil {
		t.Fatal(err)
	}
	return wakeRuntimeFixture{
		pool: pool, service: service, principal: principal,
		ownerKeyID: ownerKeyID, ownerPrivate: ownerPrivate,
		ownerPublic: ownerPublic, userVault: userVault,
		scheduler: schedulerStore, runner: runner, now: now,
		workspaceRoot: workspaceRoot, auditorSeatID: auditorSeatID,
	}
}

func (fixture wakeRuntimeFixture) diagnoseWorkOrderIntegrity(
	t *testing.T,
	ctx context.Context,
	expected workorder.Order,
	intentID string,
) string {
	t.Helper()
	var sealed []byte
	var expectedHash, orderID, goalID string
	if err := fixture.pool.QueryRow(ctx, `
		WITH RECURSIVE successors(node_id) AS (
			VALUES ($3::TEXT)
			UNION
			SELECT edge.dependent_node_id
			FROM workforce_work_edges edge
			JOIN successors ON successors.node_id=edge.prerequisite_node_id
			WHERE edge.tenant_id=$1 AND edge.organization_id=$2
		)
		SELECT order_record.sealed_order,order_record.canonical_hash,
		       order_record.work_order_id,order_record.goal_id
		FROM successors
		JOIN workforce_work_orders order_record
		  ON order_record.tenant_id=$1
		 AND order_record.organization_id=$2
		 AND order_record.goal_id=successors.node_id
		LIMIT 1
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		intentID).Scan(&sealed, &expectedHash, &orderID, &goalID); err != nil {
		return "locate=" + err.Error()
	}
	canonical, err := fixture.userVault.OpenRecord(vault.AD{
		User: fixture.principal.TenantID, Store: "workforce.work-order",
		Stream: string(fixture.principal.OrganizationID) + ":" + orderID,
		Schema: "workforce.work-order.v1",
	}, sealed)
	if err != nil {
		return "open=" + err.Error()
	}
	sum := sha256.Sum256(canonical)
	if actual := hex.EncodeToString(sum[:]); actual != expectedHash {
		return "hash=" + actual + " expected=" + expectedHash
	}
	decoded, err := contracts.DecodeCanonical[
		workorder.Order, *workorder.Order,
	](canonical)
	if err != nil {
		return "decode=" + err.Error()
	}
	if err := workorder.Verify(
		decoded, fixture.ownerKeyID, fixture.ownerPublic,
	); err != nil {
		return "verify=" + err.Error()
	}
	if decoded.ID != expected.ID || goalID == "" {
		return fmt.Sprintf(
			"identity decoded=%q expected=%q goal=%q",
			decoded.ID, expected.ID, goalID,
		)
	}
	var node dependency.Node
	var ownerSeat, ownerDepartment *string
	node.ID = dependency.NodeID(intentID)
	node.OrganizationID = fixture.principal.OrganizationID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT node_kind,owner_seat_id,owner_department_id,title,state,
		       base_priority,created_at,updated_at,deadline,contested,
		       COALESCE(cancellation_reason,''),version
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		intentID).Scan(
		&node.Kind, &ownerSeat, &ownerDepartment, &node.Title, &node.State,
		&node.BasePriority, &node.CreatedAt, &node.UpdatedAt, &node.Deadline,
		&node.Contested, &node.CancellationReason, &node.Version,
	); err != nil {
		return "node=" + err.Error()
	}
	if ownerSeat != nil {
		value := contracts.SeatID(*ownerSeat)
		node.OwnerSeatID = &value
	}
	if ownerDepartment != nil {
		value := contracts.DepartmentID(*ownerDepartment)
		node.OwnerDepartmentID = &value
	}
	if err := node.Validate(); err != nil {
		return "node-validate=" + err.Error()
	}
	return "order and node valid; derived goal or intent rejected"
}

func activateWakeRuntimeOrganization(
	t *testing.T,
	ctx context.Context,
	service *controlapi.Service,
	principal controlapi.Principal,
	ownerKeyID string,
	ownerPrivate ed25519.PrivateKey,
	now time.Time,
) contracts.SeatID {
	t.Helper()
	preview, err := service.PreviewActivation(
		ctx, principal, controlapi.ActivationPreviewRequest{
			Name: "Recovery Proof Workforce", KeyID: ownerKeyID,
			EffectiveAt: now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var auditorSeatID contracts.SeatID
	for departmentIndex := range preview.Seed.Organization.Departments {
		department := &preview.Seed.Organization.Departments[departmentIndex]
		for seatIndex := range department.Seats {
			seat := &department.Seats[seatIndex]
			if department.Kind == contracts.DepartmentExecutive &&
				seat.Role == contracts.SeatAuditor {
				auditorSeatID = seat.ID
			}
			if err := policy.SignSeat(
				seat, ownerKeyID, ownerPrivate,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index := range preview.Seed.Mandates {
		if err := policy.SignMandate(
			&preview.Seed.Mandates[index], ownerKeyID, ownerPrivate,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := policy.SignRuntimeAuthority(
		&preview.Seed.RuntimeAuthority, ownerKeyID, ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	for index := range preview.Seed.Policies {
		if err := policy.SignPolicy(
			&preview.Seed.Policies[index], ownerKeyID, ownerPrivate,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := policy.SignOrganization(
		&preview.Seed.Organization, ownerKeyID, ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	for index := range preview.SkillContracts {
		if err := skills.SignContract(
			&preview.SkillContracts[index], ownerKeyID, ownerPrivate,
		); err != nil {
			t.Fatal(err)
		}
	}
	if auditorSeatID == "" {
		t.Fatal("activation preview omitted an independent Executive Auditor")
	}
	result, err := service.ActivateOrganization(
		ctx, principal, controlapi.ActivationBundle{
			Seed: preview.Seed, SkillContracts: preview.SkillContracts,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Departments != 7 || result.Seats != 21 || result.Deduplicated {
		t.Fatalf("activation result = %#v", result)
	}
	return auditorSeatID
}

func (fixture wakeRuntimeFixture) developerOrder() controlapi.WorkOrder {
	return controlapi.WorkOrder{
		SchemaVersion:  "workforce.work-order.v1",
		ID:             "work-order:wakeruntime-recovery",
		OrganizationID: fixture.principal.OrganizationID,
		OwnerID:        fixture.principal.OwnerID,
		Version:        1,
		Objective: "Recover one captured model exchange and produce a " +
			"receipt-backed Developer observation",
		Scope:       fixture.workspaceRoot,
		ProjectID:   "project:wakeruntime-recovery",
		WorkspaceID: "workspace:wakeruntime-recovery",
		ScopeFiles:  []string{"main.go"},
		ScopeSymbols: []string{
			"Value",
		},
		Departments: []contracts.DepartmentKind{
			contracts.DepartmentDeveloper,
		},
		Priority: 10,
		Budget: controlapi.WorkOrderBudget{
			MaxTasks: 1, MaxSpendMicrounits: 1000,
		},
		Deadline: fixture.now.Add(24 * time.Hour),
		Autonomy: "supervised",
		AcceptanceCriteria: []string{
			"evidence_hash: the fenced Developer observation is content-addressed",
		},
		ModelProvider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		MGSReference:   "mgs:workforce:v1",
		MGSDigest:      strings.Repeat("a", 64),
		CreatedAt:      fixture.now,
		IdempotencyKey: "work-order:wakeruntime-recovery",
	}
}

func (fixture wakeRuntimeFixture) sevenDepartmentOrder() controlapi.WorkOrder {
	order := fixture.developerOrder()
	order.ID = "work-order:live-seven-department"
	order.Objective = "Produce a receipt-backed launch readiness chain using exact predecessor evidence and no publication or payment"
	order.Departments = []contracts.DepartmentKind{
		contracts.DepartmentDeveloper,
		contracts.DepartmentExecutive,
		contracts.DepartmentResearch,
		contracts.DepartmentMarketing,
		contracts.DepartmentLegal,
		contracts.DepartmentAccounting,
		contracts.DepartmentBackOffice,
	}
	order.Budget.MaxTasks = uint32(len(order.Departments))
	order.Budget.MaxSpendMicrounits = 100_000
	order.IdempotencyKey = "work-order:live-seven-department"
	return order
}

func (fixture wakeRuntimeFixture) assertCompletedOnce(
	t *testing.T,
	ctx context.Context,
	claim scheduler.Claim,
	created controlapi.WorkOrderResult,
) {
	t.Helper()
	checkpoint, err := fixture.runner.Execution.Load(
		ctx, fixture.principal.OrganizationID,
		contracts.WakeID(claim.Envelope.WakeID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Stage != execution.StageSleep ||
		checkpoint.Disposition != contracts.DispositionGoalCompleted ||
		checkpoint.ReceiptID == "" {
		_, modelOutput, modelErr := fixture.runner.Lineage.OpenModelOutput(
			ctx, fixture.principal.OrganizationID,
			contracts.EvidenceID("model:"+claim.Envelope.WakeID),
		)
		t.Fatalf(
			"terminal checkpoint = %#v model_output=%s model_error=%v",
			checkpoint, modelOutput, modelErr,
		)
	}
	snapshot, err := fixture.runner.Graph.Snapshot(
		ctx, fixture.principal.OrganizationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	var completedGoal, completedIntent int
	for _, node := range snapshot.Nodes {
		if node.State != dependency.StateCompleted ||
			node.TerminalRecordID == nil {
			continue
		}
		switch node.Kind {
		case dependency.NodeGoal:
			completedGoal++
		case dependency.NodeIntent:
			completedIntent++
		}
	}
	if completedGoal != 1 || completedIntent != 1 {
		t.Fatalf(
			"receipt-backed graph goal=%d intent=%d snapshot=%#v",
			completedGoal, completedIntent, snapshot,
		)
	}
	var scheduledState string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT state FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		claim.Envelope.WakeID).Scan(&scheduledState); err != nil {
		t.Fatal(err)
	}
	if scheduledState != "completed" {
		t.Fatalf("scheduled wake state = %q", scheduledState)
	}
	for table, expected := range map[string]int{
		"workforce_model_evidence":          1,
		"workforce_effect_operations":       1,
		"workforce_effect_evidence":         1,
		"workforce_verdict_records":         1,
		"workforce_execution_receipts":      1,
		"workforce_developer_change_scopes": 1,
	} {
		var count int
		query := fmt.Sprintf(`
			SELECT COUNT(*) FROM %s
			WHERE tenant_id=$1 AND organization_id=$2
		`, table)
		if err := fixture.pool.QueryRow(
			ctx, query, fixture.principal.TenantID,
			fixture.principal.OrganizationID,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("%s rows = %d, want %d", table, count, expected)
		}
	}
	var provider, operation, effectState string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT provider,operation,state FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID,
		fixture.principal.OrganizationID).Scan(
		&provider, &operation, &effectState,
	); err != nil {
		t.Fatal(err)
	}
	if provider != "developer" || operation != "inspect_source" ||
		effectState != "succeeded" {
		t.Fatalf(
			"Developer effect = provider:%q operation:%q state:%q",
			provider, operation, effectState,
		)
	}
	var executingDepartment, auditorDepartment, outcome string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT executing_department_id,auditor_department_id,outcome
		FROM workforce_verdict_records
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID,
		fixture.principal.OrganizationID).Scan(
		&executingDepartment, &auditorDepartment, &outcome,
	); err != nil {
		t.Fatal(err)
	}
	if executingDepartment == auditorDepartment || outcome != "pass" {
		t.Fatalf(
			"independent verdict = executing:%q auditor:%q outcome:%q",
			executingDepartment, auditorDepartment, outcome,
		)
	}
	var artifactCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_wake_runtime_artifacts
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		claim.Envelope.WakeID).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if artifactCount != 3 {
		t.Fatalf("immutable runtime artifacts = %d, want 3", artifactCount)
	}
	var completedEvents int
	var verified bool
	var receiptID string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*),bool_and(verified_completion),min(receipt_id)
		FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2
		  AND event_type='wake.receipt_committed' AND resource_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		claim.Envelope.WakeID).Scan(
		&completedEvents, &verified, &receiptID,
	); err != nil {
		t.Fatal(err)
	}
	if completedEvents != 1 || !verified ||
		receiptID != string(checkpoint.ReceiptID) ||
		created.GoalID == "" {
		t.Fatalf(
			"verified lifecycle count=%d verified=%t receipt=%q goal=%q",
			completedEvents, verified, receiptID, created.GoalID,
		)
	}
}

func buildWakeRuntimeCodeGraph(
	t *testing.T,
	ctx context.Context,
	codegraph string,
) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.test/recovery\n\ngo 1.23\n",
		"main.go": "package recovery\n\nfunc Value() int { return 1 }\n",
		"main_test.go": "package recovery\n\nimport \"testing\"\n\n" +
			"func TestValue(t *testing.T) { if Value() != 1 { t.Fatal(Value()) } }\n",
	}
	for path, content := range files {
		if err := os.WriteFile(
			filepath.Join(root, path), []byte(content), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	database := filepath.Join(root, ".cg", "codegraph.db")
	command := exec.CommandContext(
		ctx, codegraph, "--db", database, "build", root,
	)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build real CodeGraph fixture: %v: %s", err, output)
	}
	return root
}

func buildWakeRuntimeBinary(
	t *testing.T,
	ctx context.Context,
	moduleRoot, destination, target string,
) {
	t.Helper()
	command := exec.CommandContext(ctx, "go", "build", "-o", destination, target)
	command.Dir = moduleRoot
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", target, err, output)
	}
}

func wakeRuntimeModuleRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			if _, err := os.Stat(
				filepath.Join(current, "cmd", "workforce-seat"),
			); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("locate Workforce module root")
		}
		current = parent
	}
}

func wakeRuntimeExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("locate real %s executable: %v", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func wakeRuntimeFileDigest(
	t *testing.T,
	path string,
) contracts.ContentHash {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func startWakeRuntimePostgres(
	ctx context.Context,
) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-wakeruntime-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", wakeRuntimePostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(
		ctx, "docker", "port", container, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return container, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf("invalid PostgreSQL port %q", address)
	}
	return container,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			address[index+1:] + "/workforce?sslmode=disable",
		nil
}

func waitWakeRuntimePostgres(
	ctx context.Context,
	databaseURL string,
) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if err := pool.Ping(ctx); err == nil {
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

func TestRetryableWakeErrorRemainsDiscoverable(t *testing.T) {
	cause := errors.New("provider unavailable")
	if !errors.Is(retryWake(cause), cause) {
		t.Fatal("retryable wake error does not preserve its cause")
	}
}
