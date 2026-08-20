package actorstate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/ledger"
	"centra/workforce/internal/mail"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/projectbrain"
	"centra/workforce/internal/skills"
	"centra/workforce/internal/testauthority"
)

const actorstatePostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var actorstatePool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startActorstatePostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actorstate integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	}
	actorstatePool, err = waitActorstatePostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "actorstate integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, actorstatePool, actorstateNow()); err != nil {
		actorstatePool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "actorstate migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	actorstatePool.Close()
	cleanup()
	os.Exit(code)
}

type assemblerFixture struct {
	tenant         string
	organizationID contracts.OrganizationID
	keyID          string
	publicKey      ed25519.PublicKey
	privateKey     ed25519.PrivateKey
	userVault      *vault.UserVault
	authority      *policy.Store
	graph          *dependency.Store
	leases         *lease.Store
	records        *ledger.Store
	mailbox        *mail.Store
	catalog        *skills.Catalog
	seat           contracts.Seat
	mandate        contracts.Mandate
	leaseID        contracts.LeaseID
	intentID       contracts.IntentID
	authorityLease contracts.WakeLease
	goal           contracts.Goal
	intent         contracts.Intent
}

func newAssemblerFixture(
	t *testing.T,
	ctx context.Context,
	scope string,
	now func() time.Time,
) assemblerFixture {
	t.Helper()
	tenant := "tenant-" + scope
	organizationID := contracts.OrganizationID("org-" + scope)
	ownerID := contracts.OwnerID("owner-" + scope)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "owner-key-" + scope
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := policy.OwnerRoot{
		TenantID: tenant, OrganizationID: organizationID,
		OwnerID: ownerID, KeyID: keyID, PublicKey: publicKey,
	}
	authority, err := policy.New(actorstatePool, session.UserVault(), root, now)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := policy.BuildSeed(
		organizationID, ownerID, "Actorstate Organization",
		now(), keyID, privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: organizationID, OwnerID: ownerID,
		Scope: "authority:write", IssuedAt: now().Add(-time.Minute),
		ExpiresAt: now().Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := testauthority.PublishRuntimeAuthority(
		ctx, authority, organizationID, keyID, publicKey, privateKey,
		grant, now(),
	); err != nil {
		t.Fatal(err)
	}
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	if err := authority.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatal(err)
	}
	if err := authority.PublishSeat(ctx, seat, grant); err != nil {
		t.Fatal(err)
	}
	policyValue := contracts.Policy{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "policy-" + contracts.PolicyID(scope), Version: 1,
		OrganizationID: organizationID, Kind: "autonomy",
		EffectiveAt: now(),
		Rules: []contracts.PolicyRule{{
			ClauseID: "bounded-work", Outcome: "allow",
			Scope: "work within the signed mandate",
		}},
	}
	if err := policy.SignPolicy(&policyValue, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := authority.PublishPolicy(ctx, policyValue, grant); err != nil {
		t.Fatal(err)
	}
	policyHash := canonicalHash(t, &policyValue)
	policyRef := contracts.PolicyRef{
		ID: policyValue.ID, Version: policyValue.Version, Hash: policyHash,
	}
	graph, err := dependency.New(actorstatePool, tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	intentID := contracts.IntentID("intent-" + scope)
	if err := graph.PutNode(ctx, dependency.Node{
		ID: dependency.NodeID(intentID), OrganizationID: organizationID,
		Kind: dependency.NodeIntent, OwnerSeatID: &seat.ID,
		OwnerDepartmentID: &seat.DepartmentID,
		Title:             "Current eligible work", State: dependency.StateEligible,
		BasePriority: 10, CreatedAt: now(),
		UpdatedAt: now(), Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	runtimeLeases, err := lease.New(actorstatePool, tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	leaseID := contracts.LeaseID("lease-" + scope)
	wakeID := contracts.WakeID("wake-" + scope)
	runtimeGrant, err := runtimeLeases.Acquire(ctx, lease.Request{
		ID: leaseID, WakeID: wakeID, OrganizationID: organizationID,
		SeatID: seat.ID, NodeID: dependency.NodeID(intentID),
		MandateID: mandate.ID, MandateVersion: mandate.Version,
		Policies: []contracts.PolicyRef{policyRef},
		IssuedAt: now(), ExpiresAt: now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	skillContracts := make([]skills.Contract, 0, len(mandate.AllowedSkills))
	for _, id := range mandate.AllowedSkills {
		skillContracts = append(skillContracts, actorstateSkill(t, id))
	}
	catalog, err := skills.NewCatalog(skillContracts)
	if err != nil {
		t.Fatal(err)
	}
	authorityLease := integrationWakeLease(
		leaseID, wakeID, organizationID, seat, mandate,
		policyRef, catalog.Digest(), runtimeGrant.Fence, now(),
	)
	if err := policy.SignWakeLease(&authorityLease, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := authority.RegisterLease(ctx, authorityLease); err != nil {
		t.Fatal(err)
	}
	records, err := ledger.New(actorstatePool, session.UserVault(), tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := mail.New(
		actorstatePool, session.UserVault(), graph, tenant,
		mail.Config{
			MaxMailboxMessages: 100, MaxThreadMessages: 50, MaxThreadDepth: 10,
			MaxRecipients: 10, MaxAutoReplies: 5, MaxAttachmentBytes: 1 << 20,
			MaxMessageLifetime: 24 * time.Hour,
		},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	goalID := contracts.GoalID("goal-" + scope)
	return assemblerFixture{
		tenant: tenant, organizationID: organizationID,
		keyID: keyID, publicKey: publicKey, privateKey: privateKey,
		userVault: session.UserVault(), authority: authority, graph: graph,
		leases: runtimeLeases, records: records, mailbox: mailbox,
		catalog: catalog, seat: seat, mandate: mandate,
		leaseID: leaseID, intentID: intentID, authorityLease: authorityLease,
		goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1, ID: goalID,
			OrganizationID: organizationID, WorkOrderID: "order-" + contracts.WorkOrderID(scope),
			Title: "Complete current eligible work", SuccessCriteria: []string{"Typed output exists"},
			CreatedAt: now().Add(-time.Hour),
		},
		intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1, ID: intentID,
			OrganizationID: organizationID, GoalID: goalID, OwnerSeatID: seat.ID,
			Summary: "Execute the current graph node", Priority: 10,
			CreatedAt: now().Add(-time.Hour),
		},
	}
}

func (fixture assemblerFixture) request() AssemblyRequest {
	return AssemblyRequest{
		LeaseID: fixture.leaseID,
		Goal:    fixture.goal,
		Intent:  fixture.intent,
		Tools: []contracts.ToolRef{{
			Name: "inspect", SchemaDigest: actorstateHash("tool"),
		}},
		RequiredOutput: []contracts.RequiredOutput{{
			Kind: "typed_result", SuccessPredicate: "result validates",
		}},
	}
}

func TestAssemblerUsesCurrentAuthorizedDurableSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newAssemblerFixture(t, ctx, shortScope(t.Name()), actorstateNow)
	assembler, err := NewAssembler(
		fixture.graph, fixture.records, fixture.mailbox, fixture.authority,
		fixture.leases, fixture.catalog, nil, actorstateNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := assembler.Assemble(ctx, fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if packet.Lease.ID != fixture.leaseID || packet.Seat.ID != fixture.seat.ID ||
		packet.Mandate.ID != fixture.mandate.ID ||
		len(packet.Skills) != len(fixture.mandate.AllowedSkills) {
		t.Fatalf("assembler returned an incomplete current projection: %#v", packet)
	}
	stale := fixture.authorityLease
	stale.Fence++
	if _, err := fixture.leases.Authorize(
		ctx, fixture.organizationID, stale.ID, stale.Fence,
	); err == nil {
		t.Fatal("runtime authority accepted a stale fence")
	}
	scopeless := fixture.mandate
	scopeless.DataScopes = nil
	recordRequest := fixture.request()
	recordRequest.RecordIDs = []contracts.RecordID{"record-current"}
	if _, _, err := assembler.openRecords(
		ctx, fixture.authorityLease, fixture.seat, scopeless, recordRequest, nil,
	); err == nil || !strings.Contains(err.Error(), "no data scopes") {
		t.Fatalf("scope-less mandate with requested records = %v", err)
	}
}

func TestAssemblerProjectBrainRequiresSignedCurrentCapability(t *testing.T) {
	executable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatalf("locate owned cg: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(workspace, "go.mod"),
		[]byte("module project\n\ngo 1.26\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "main.go"),
		[]byte("package project\n\nfunc Current() int { return 1 }\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(workspace, ".cg", "codegraph.db")
	if output, err := exec.CommandContext(
		ctx, executable, "--db", database, "build", workspace,
	).CombinedOutput(); err != nil {
		t.Fatalf("build owned cg graph: %v: %s", err, output)
	}
	moment := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	now := func() time.Time { return moment }
	scope := shortScope(t.Name())
	fixture := newAssemblerFixture(t, ctx, scope, now)
	brainGraph, err := projectbrain.NewCodeGraph(executable, now)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := projectbrain.New(
		actorstatePool, fixture.userVault, fixture.tenant, fixture.keyID,
		fixture.publicKey, fixture.authority, brainGraph, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler, err := NewAssembler(
		fixture.graph, fixture.records, fixture.mailbox, fixture.authority,
		fixture.leases, fixture.catalog, brain, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	grant := projectbrain.CapabilityGrant{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "grant-read-" + scope,
		TenantID:       fixture.tenant,
		OrganizationID: fixture.organizationID,
		ProjectID:      contracts.ProjectID("project-" + scope),
		WorkspaceID:    contracts.WorkspaceID("workspace-" + scope),
		WorkspaceRoot:  workspace, Operation: projectbrain.CapabilityRead,
		RequesterSeatID:         fixture.seat.ID,
		RequesterSeatVersion:    fixture.seat.Version,
		RequesterSeatDID:        fixture.seat.DID,
		RequesterBindingID:      fixture.seat.BindingID,
		RequesterBindingVersion: fixture.seat.BindingVersion,
		Purpose:                 "implementation",
		MaxRecords:              16,
		IssuedAt:                moment.Add(-time.Minute), ExpiresAt: moment.Add(30 * time.Minute),
	}
	if err := projectbrain.SignCapabilityGrant(
		&grant, fixture.keyID, fixture.privateKey,
	); err != nil {
		t.Fatal(err)
	}
	request := fixture.request()
	request.ProjectBrain = &grant
	packet, err := assembler.Assemble(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if packet.ProjectBrain == nil || packet.ProjectBrain.ProjectID != grant.ProjectID ||
		packet.ProjectBrain.WorkspaceID != grant.WorkspaceID ||
		packet.ProjectBrain.ViewDigest.Digest == "" || !packet.ProjectBrain.Fresh ||
		packet.ProjectBrain.ExpiresAt != grant.ExpiresAt {
		t.Fatalf("assembled Project Brain reference = %#v", packet.ProjectBrain)
	}
	denied := []struct {
		name   string
		mutate func(*projectbrain.CapabilityGrant)
		resign bool
	}{
		{"foreign requester seat", func(value *projectbrain.CapabilityGrant) {
			value.RequesterSeatID = "seat-intruder"
		}, true},
		{"foreign organization", func(value *projectbrain.CapabilityGrant) {
			value.OrganizationID = "org-intruder"
		}, true},
		{"foreign tenant", func(value *projectbrain.CapabilityGrant) {
			value.TenantID = "tenant-intruder"
		}, true},
		{"expiry beyond wake lease", func(value *projectbrain.CapabilityGrant) {
			value.ExpiresAt = fixture.authorityLease.ExpiresAt.Add(time.Minute)
		}, true},
		{"tampered after signing", func(value *projectbrain.CapabilityGrant) {
			value.Purpose = "widened"
		}, false},
	}
	for _, attempt := range denied {
		value := grant
		attempt.mutate(&value)
		if attempt.resign {
			value.ID = grant.ID + "-" + shortScope(attempt.name)
			if err := projectbrain.SignCapabilityGrant(
				&value, fixture.keyID, fixture.privateKey,
			); err != nil {
				t.Fatal(err)
			}
		}
		deniedRequest := fixture.request()
		deniedRequest.ProjectBrain = &value
		if _, err := assembler.Assemble(ctx, deniedRequest); err == nil {
			t.Fatalf("%s grant assembled a Project Brain reference", attempt.name)
		}
	}
	authorPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recordID := projectbrain.RecordID("record-" + scope)
	writeGrant := grant
	writeGrant.ID = grant.ID + "-write"
	writeGrant.Operation = projectbrain.CapabilityWrite
	writeGrant.MaxRecords = 0
	writeGrant.RecordID = &recordID
	writeGrant.Author = &projectbrain.SeatKeyBinding{
		SeatID: fixture.seat.ID, SeatVersion: fixture.seat.Version,
		SeatDID: fixture.seat.DID, BindingID: fixture.seat.BindingID,
		BindingVersion: fixture.seat.BindingVersion, KeyID: "author-key",
		PublicKey: base64.RawURLEncoding.EncodeToString(authorPublic),
	}
	writeGrant.Verifier = &projectbrain.SeatKeyBinding{
		SeatID: "seat-verifier", SeatVersion: 1,
		SeatDID: "did:matrix:seat-verifier", BindingID: "binding:seat-verifier",
		BindingVersion: 1, KeyID: "verifier-key",
		PublicKey: base64.RawURLEncoding.EncodeToString(verifierPublic),
	}
	if err := projectbrain.SignCapabilityGrant(
		&writeGrant, fixture.keyID, fixture.privateKey,
	); err != nil {
		t.Fatal(err)
	}
	writeRequest := fixture.request()
	writeRequest.ProjectBrain = &writeGrant
	if _, err := assembler.Assemble(ctx, writeRequest); err == nil {
		t.Fatal("write-operation grant assembled a Project Brain reference")
	}
	_, intruderKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged := grant
	forged.ID = grant.ID + "-forged"
	if err := projectbrain.SignCapabilityGrant(
		&forged, fixture.keyID, intruderKey,
	); err != nil {
		t.Fatal(err)
	}
	forgedRequest := fixture.request()
	forgedRequest.ProjectBrain = &forged
	if _, err := assembler.Assemble(ctx, forgedRequest); err == nil {
		t.Fatal("forged kernel signature assembled a Project Brain reference")
	}
	blind, err := NewAssembler(
		fixture.graph, fixture.records, fixture.mailbox, fixture.authority,
		fixture.leases, fixture.catalog, nil, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	blindRequest := fixture.request()
	blindRequest.ProjectBrain = &grant
	if _, err := blind.Assemble(ctx, blindRequest); err == nil {
		t.Fatal("assembler without a Project Brain authority source did not fail closed")
	}
}

func integrationWakeLease(
	leaseID contracts.LeaseID,
	wakeID contracts.WakeID,
	organizationID contracts.OrganizationID,
	seat contracts.Seat,
	mandate contracts.Mandate,
	policyRef contracts.PolicyRef,
	catalogDigest contracts.ContentHash,
	fence contracts.FenceToken,
	issuedAt time.Time,
) contracts.WakeLease {
	signature := contracts.Signature{
		Algorithm: "ed25519", KeyID: "kernel-key",
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	return contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            leaseID, WakeID: wakeID, OrganizationID: organizationID,
		SeatID: seat.ID, SeatDID: seat.DID, Reason: "eligible_work",
		MandateID: mandate.ID, MandateVersion: mandate.Version,
		Policies:   []contracts.PolicyRef{policyRef},
		GraphScope: []contracts.IntentID{"intent-" + contracts.IntentID(strings.TrimPrefix(string(leaseID), "lease-"))},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "model-" + contracts.ModelBindingID(leaseID),
			Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: actorstateHash("sampling"),
		},
		MGS: contracts.MGSGenomeRef{Reference: "mgs-" + string(leaseID), Digest: actorstateHash("mgs")},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             actorstateHash("build"),
			AuditorBuildDigest:      actorstateHash("auditor-build"),
			OperationRegistryDigest: actorstateHash("operations"),
		},
		SkillCatalogDigest: catalogDigest,
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
			MaxSteps:          20, MaxModelCalls: 10, MaxToolCalls: 40,
			MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour),
		Fence: fence, Signature: signature,
	}
}

func actorstateSkill(t *testing.T, id contracts.SkillID) skills.Contract {
	t.Helper()
	value := skills.Contract{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            id, Version: 1,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"read"}, DataScopes: []string{"department"},
		Preconditions: []string{"lease active"},
		Operations: []skills.Operation{{
			Name: "inspect", EffectClass: skills.EffectRead,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability:   "read", DataScopes: []string{"department"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"filesystem"},
		}},
		Postconditions: []string{"typed evidence produced"},
		VerifierDigest: actorstateHash("verifier-" + string(id)),
		Retry:          skills.RetryPolicy{MaxAttempts: 1},
		Idempotency: skills.IdempotencyStrategy{
			Scope: "intent", KeyFields: []string{"intent_id"},
		},
		ScheduleEligibility: skills.ScheduleEligibility{WakeReasons: []string{"eligible_work"}},
		Resources: skills.ResourceEstimate{
			MaxDuration: time.Minute, ModelCalls: 1, MemoryBytes: 1 << 20,
		},
	}
	var err error
	value.Digest, err = value.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func canonicalHash[T contracts.Validatable](t *testing.T, value T) contracts.ContentHash {
	t.Helper()
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func actorstateHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func shortScope(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func actorstateNow() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startActorstatePostgres(ctx context.Context) (string, string, error) {
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		actorstatePostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
	if err != nil {
		return container, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf("invalid PostgreSQL port %q", address)
	}
	return container, "postgres://postgres:workforce-test-password@127.0.0.1:" +
		address[index+1:] + "/workforce?sslmode=disable", nil
}

func waitActorstatePostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
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
