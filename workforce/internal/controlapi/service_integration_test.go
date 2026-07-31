package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

const controlPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var controlPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startControlPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "controlapi integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(
			context.Background(), 20*time.Second,
		)
		defer stopCancel()
		_ = exec.CommandContext(
			stopCtx, "docker", "rm", "-f", container,
		).Run()
	}
	controlPool, err = waitControlPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "controlapi integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(
		ctx, controlPool, controlIntegrationTime(),
	); err != nil {
		controlPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "controlapi migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	controlPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_LifecyclePublishIsExactlyIdempotent(t *testing.T) {
	fixture := newControlFixture(t, "lifecycle")
	event := LifecycleEvent{
		ID:              "event:wake:running",
		OrganizationID:  fixture.principal.OrganizationID,
		Type:            "wake.running",
		ResourceKind:    "wake",
		ResourceID:      "wake:one",
		ResourceVersion: 1,
		Fields: map[string]any{
			"state": "working", "intent_id": "intent:one",
		},
	}
	first, err := fixture.service.Publish(
		context.Background(), fixture.principal, event,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed := event
	replayed.Fields = map[string]any{
		"intent_id": "intent:one", "state": "working",
	}
	second, err := fixture.service.Publish(
		context.Background(), fixture.principal, replayed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor == 0 || second.Cursor != first.Cursor ||
		!second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("idempotent lifecycle = first=%+v second=%+v", first, second)
	}
	conflict := event
	conflict.Fields = map[string]any{
		"state": "verifying", "intent_id": "intent:one",
	}
	if _, err := fixture.service.Publish(
		context.Background(), fixture.principal, conflict,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting lifecycle replay = %v", err)
	}
	var count int
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		event.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("lifecycle rows = %d", count)
	}
}

func TestIntegration_ActivationAndWorkOrderUseExactRuntimeModel(t *testing.T) {
	fixture := newControlFixture(t, "work-order")
	activation, err := fixture.activate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if activation.Departments != 7 || activation.Seats != 21 ||
		activation.Deduplicated {
		t.Fatalf("activation = %+v", activation)
	}
	order := fixture.order("one", "mimo", "mimo-v2.5-pro")
	if err := workorder.Sign(
		&order, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, order,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.GoalID == "" || len(created.IntentIDs) != 2 ||
		created.WakeID == "" || created.Deduplicated {
		t.Fatalf("created Work Order = %+v", created)
	}
	replayed, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, order,
	)
	if err != nil || !replayed.Deduplicated ||
		replayed.WakeID != created.WakeID {
		t.Fatalf("replayed Work Order = %+v, %v", replayed, err)
	}
	mismatch := fixture.order("mismatch", "mimo", "mimo-v2.5-pro-other")
	if err := workorder.Sign(
		&mismatch, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, mismatch,
	); err == nil || !strings.Contains(
		err.Error(), "does not match the executable runtime",
	) {
		t.Fatalf("mismatched runtime model = %v", err)
	}
	var goals, intents, wakes, events int
	if err := controlPool.QueryRow(context.Background(), `
		SELECT
		  COUNT(*) FILTER (WHERE node_kind='goal'),
		  COUNT(*) FILTER (WHERE node_kind='intent')
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&goals, &intents,
	); err != nil {
		t.Fatal(err)
	}
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&wakes,
	); err != nil {
		t.Fatal(err)
	}
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2
		  AND event_type='work_order.queued'
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&events,
	); err != nil {
		t.Fatal(err)
	}
	if goals != 1 || intents != 2 || wakes != 1 || events != 1 {
		t.Fatalf(
			"atomic projections goals=%d intents=%d wakes=%d events=%d",
			goals, intents, wakes, events,
		)
	}
}

func TestIntegration_RuntimeOwnerRootFollowsActivatedControlKey(t *testing.T) {
	fixture := newControlFixture(t, "runtime-owner-root")
	ctx := context.Background()
	if _, err := fixture.service.RuntimeOwnerRoot(
		ctx, fixture.principal,
	); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("root before activation = %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "key:browser:runtime-owner-root"
	if err := fixture.service.RegisterControlKey(
		ctx, fixture.principal, ControlKeyRegistration{
			KeyID:     keyID,
			PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		},
	); err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "Browser-owned Workforce", KeyID: keyID,
			EffectiveAt: fixture.now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(
		&preview, keyID, privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, SkillContracts: preview.SkillContracts,
		},
	); err != nil {
		t.Fatal(err)
	}
	root, err := fixture.service.RuntimeOwnerRoot(ctx, fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if root.KeyID != keyID || root.OwnerID != fixture.principal.OwnerID ||
		!root.PublicKey.Equal(publicKey) {
		t.Fatalf("activated runtime owner root = %#v", root)
	}
}

type controlFixture struct {
	service      *Service
	principal    Principal
	ownerKeyID   string
	ownerPrivate ed25519.PrivateKey
	now          time.Time
}

func newControlFixture(t *testing.T, label string) controlFixture {
	t.Helper()
	now := controlIntegrationTime()
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		TenantID:       "tenant:" + label,
		OrganizationID: contracts.OrganizationID("organization:" + label),
		OwnerID:        contracts.OwnerID("owner:" + label),
	}
	auth, err := NewStaticAuthenticator(map[string]Principal{
		"token:" + label: principal,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerKeyID := "key:owner:" + label
	service, err := New(
		controlPool, auth,
		map[string]OwnerKey{
			principal.TenantID: {
				KeyID: ownerKeyID, PublicKey: ownerPublic,
			},
		},
		func() time.Time { return now }, 16,
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(),
		UserDID: principal.TenantID,
		KEKHex: hex.EncodeToString(
			[]byte("0123456789abcdef0123456789abcdef"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachVault(session.UserVault()); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeAuthority(
		"key:runtime:"+label, runtimePublic,
	); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeModel("mimo", "mimo-v2.5-pro"); err != nil {
		t.Fatal(err)
	}
	schedulerStore, err := scheduler.New(
		controlPool, session.UserVault(), principal.TenantID,
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
	return controlFixture{
		service: service, principal: principal,
		ownerKeyID: ownerKeyID, ownerPrivate: ownerPrivate, now: now,
	}
}

func (fixture controlFixture) activate(
	ctx context.Context,
) (ActivationResult, error) {
	preview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name:  "Workforce " + string(fixture.principal.OrganizationID),
			KeyID: fixture.ownerKeyID, EffectiveAt: fixture.now,
		},
	)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := signControlActivation(
		&preview, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		return ActivationResult{}, err
	}
	return fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, SkillContracts: preview.SkillContracts,
		},
	)
}

func signControlActivation(
	preview *ActivationPreview,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	for departmentIndex := range preview.Seed.Organization.Departments {
		for seatIndex := range preview.Seed.Organization.Departments[departmentIndex].Seats {
			if err := policy.SignSeat(
				&preview.Seed.Organization.Departments[departmentIndex].Seats[seatIndex],
				keyID, privateKey,
			); err != nil {
				return err
			}
		}
	}
	for index := range preview.Seed.Mandates {
		if err := policy.SignMandate(
			&preview.Seed.Mandates[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	if err := policy.SignRuntimeAuthority(
		&preview.Seed.RuntimeAuthority, keyID, privateKey,
	); err != nil {
		return err
	}
	for index := range preview.Seed.Policies {
		if err := policy.SignPolicy(
			&preview.Seed.Policies[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	if err := policy.SignOrganization(
		&preview.Seed.Organization, keyID, privateKey,
	); err != nil {
		return err
	}
	for index := range preview.SkillContracts {
		if err := skills.SignContract(
			&preview.SkillContracts[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	return nil
}

func (fixture controlFixture) order(
	label, provider, modelID string,
) WorkOrder {
	return WorkOrder{
		SchemaVersion:  "workforce.work-order.v1",
		ID:             "work-order:" + label + ":" + string(fixture.principal.OrganizationID),
		OrganizationID: fixture.principal.OrganizationID,
		OwnerID:        fixture.principal.OwnerID,
		Version:        1,
		Objective:      "Produce a receipt-backed bounded result",
		Scope:          "/root/matrix",
		ProjectID:      "project:matrix",
		WorkspaceID:    "workspace:matrix",
		ScopeFiles: []string{
			"workforce/internal/wakeruntime/recovery.go",
		},
		ScopeSymbols: []string{"RunClaim"},
		Departments: []contracts.DepartmentKind{
			contracts.DepartmentDeveloper,
			contracts.DepartmentExecutive,
		},
		Priority: 10,
		Budget: WorkOrderBudget{
			MaxTasks: 5, MaxSpendMicrounits: 1000,
		},
		Deadline: fixture.now.Add(24 * time.Hour),
		Autonomy: "supervised",
		AcceptanceCriteria: []string{
			"evidence_hash: authoritative provider evidence is content-addressed",
		},
		ModelProvider: provider, ModelID: modelID,
		MGSReference: "mgs:workforce:v1",
		MGSDigest:    strings.Repeat("a", 64),
		CreatedAt:    fixture.now,
		IdempotencyKey: "work-order:" + label + ":" +
			string(fixture.principal.OrganizationID),
	}
}

func controlIntegrationTime() time.Time {
	return time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
}

func startControlPostgres(
	ctx context.Context,
) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-control-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", controlPostgresImage,
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
		return container, "", fmt.Errorf(
			"invalid PostgreSQL port %q", address,
		)
	}
	return container,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			address[index+1:] + "/workforce?sslmode=disable",
		nil
}

func waitControlPostgres(
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
