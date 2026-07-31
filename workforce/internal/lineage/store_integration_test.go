package lineage

import (
	"bytes"
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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/testauthority"
	"matrix/workforce/internal/workcompile"
)

func lineageCanonicalHash[T contracts.Validatable](t *testing.T, value T) contracts.ContentHash {
	t.Helper()
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

// signLineagePacket signs the packet's authority with the owner root, as the
// kernel does before a packet is ever handed to a compiler.
func signLineagePacket(
	t *testing.T,
	packet *contracts.WorkPacket,
	keyID string,
	privateKey ed25519.PrivateKey,
) {
	t.Helper()
	if err := policy.SignMandate(&packet.Mandate, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := policy.SignSeat(&packet.Seat, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := policy.SignWakeLease(&packet.Lease, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
}

const lineagePostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var lineagePool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startLineagePostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lineage integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	}
	lineagePool, err = waitLineagePostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "lineage integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, lineagePool, lineageNow()); err != nil {
		lineagePool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "lineage migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	lineagePool.Close()
	cleanup()
	os.Exit(code)
}

func TestSkillCompilationModelEvidenceAndReceiptLineage(t *testing.T) {
	ctx := context.Background()
	scope := lineageScope(t.Name())
	tenant := "tenant-" + scope
	organizationID := contracts.OrganizationID("org-" + scope)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "owner-" + scope
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	skillStore, err := skills.NewStore(
		lineagePool, session.UserVault(), tenant, organizationID,
		keyID, publicKey, lineageNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	contractV1 := lineageSkill(t, 1, "lease active")
	publishedV1 := signedSkill(t, contractV1, organizationID, keyID, privateKey)
	if err := skillStore.Publish(ctx, publishedV1); err != nil {
		t.Fatal(err)
	}
	if err := skillStore.Publish(ctx, publishedV1); err != nil {
		t.Fatalf("idempotent skill publication: %v", err)
	}
	loaded, err := skillStore.LoadCurrent(ctx, contractV1.ID)
	if err != nil || loaded.Contract.Digest != contractV1.Digest {
		t.Fatalf("current skill = %#v, %v", loaded, err)
	}
	packet := lineagePacket(t, scope, organizationID, contractV1)
	// The compiler admits only genuinely signed authority, so the fixture signs
	// the packet with the owner root and takes a live runtime lease for it.
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := "owner-" + contracts.OwnerID(scope)
	authority, err := policy.New(lineagePool, session.UserVault(), policy.OwnerRoot{
		TenantID: tenant, OrganizationID: organizationID,
		OwnerID: ownerID, KeyID: "owner", PublicKey: ownerPublic,
	}, lineageNow)
	if err != nil {
		t.Fatal(err)
	}
	ownerGrant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: organizationID, OwnerID: ownerID,
		Scope: "authority:write", IssuedAt: lineageNow().Add(-time.Minute),
		ExpiresAt: lineageNow().Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&ownerGrant, "owner", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := testauthority.PublishRuntimeAuthority(
		ctx, authority, organizationID, "owner",
		ownerPublic, ownerPrivate, ownerGrant, lineageNow(),
	); err != nil {
		t.Fatal(err)
	}
	policyValue := contracts.Policy{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "policy-" + contracts.PolicyID(scope), Version: 1,
		OrganizationID: organizationID, Kind: "autonomy",
		EffectiveAt: lineageNow(),
		Rules: []contracts.PolicyRule{{
			ClauseID: "bounded-work", Outcome: "allow",
			Scope: "compile only within the signed mandate",
		}},
	}
	if err := policy.SignPolicy(&policyValue, "owner", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := authority.PublishPolicy(ctx, policyValue, ownerGrant); err != nil {
		t.Fatal(err)
	}
	policyRef := contracts.PolicyRef{
		ID: policyValue.ID, Version: policyValue.Version,
		Hash: lineageCanonicalHash(t, &policyValue),
	}
	packet.Policies = []contracts.PolicyRef{policyRef}
	packet.Lease.Policies = []contracts.PolicyRef{policyRef}
	if err := policy.SignMandate(&packet.Mandate, "owner", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := authority.PublishMandate(ctx, packet.Mandate, ownerGrant); err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(lineagePool, tenant, lineageNow)
	if err != nil {
		t.Fatal(err)
	}
	runtimeGrant, err := leaseStore.Acquire(ctx, lease.Request{
		ID: packet.Lease.ID, WakeID: packet.Lease.WakeID,
		OrganizationID: organizationID, SeatID: packet.Seat.ID,
		NodeID:    dependency.NodeID(packet.Intent.ID),
		MandateID: packet.Mandate.ID, MandateVersion: packet.Mandate.Version,
		Policies: append([]contracts.PolicyRef(nil), packet.Policies...),
		IssuedAt: packet.Lease.IssuedAt, ExpiresAt: packet.Lease.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	packet.Lease.Fence = runtimeGrant.Fence
	signLineagePacket(t, &packet, "owner", ownerPrivate)
	if err := authority.PublishSeat(ctx, packet.Seat, ownerGrant); err != nil {
		t.Fatal(err)
	}
	if err := authority.RegisterLease(ctx, packet.Lease); err != nil {
		t.Fatal(err)
	}
	compiler, err := workcompile.New(
		lineagePool, session.UserVault(), tenant, skillStore,
		authority, leaseStore, lineageNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	source := contracts.SourceState{
		RootDigest: lineageHash("source"), GraphGeneration: 7, LedgerCursor: 19,
	}
	proposal := workcompile.Proposal{
		SchemaVersion: contracts.SchemaVersionV1, ID: "proposal-" + scope,
		OrganizationID: organizationID, WakeID: packet.Lease.WakeID,
		IntentID: packet.Intent.ID, SeatID: packet.Seat.ID,
		Skill: packet.Skills[0], Operation: "inspect", Provider: "internal",
		IdempotencyKey: "operation-" + scope,
		Input:          json.RawMessage(`{"path":"internal/lineage"}`),
		Deadline:       lineageNow().Add(30 * time.Minute),
	}
	plan, err := compiler.Compile(ctx, packet, proposal, source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeatDID != packet.Seat.DID || plan.Model != packet.Lease.Model ||
		plan.MGS != packet.Lease.MGS || plan.Runtime != packet.Lease.Runtime ||
		plan.Source != source || plan.VerifierDigest != contractV1.VerifierDigest ||
		plan.Resources != contractV1.Resources {
		t.Fatalf("compiled plan omitted lineage: %#v", plan)
	}
	replayedPlan, err := compiler.Compile(ctx, packet, proposal, source)
	if err != nil || replayedPlan.PlanHash != plan.PlanHash {
		t.Fatalf("idempotent compile = %#v, %v", replayedPlan, err)
	}
	invalidProposal := proposal
	invalidProposal.ID = "proposal-invalid-" + scope
	invalidProposal.Input = json.RawMessage(`{"unexpected":true}`)
	if _, err := compiler.Compile(ctx, packet, invalidProposal, source); !errors.Is(err, workcompile.ErrProposalInvalid) {
		t.Fatalf("invalid typed input error = %v", err)
	}

	lineageStore, err := New(
		lineagePool, session.UserVault(), tenant, "kernel-"+scope,
		privateKey, lineageNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	modelEvidence, err := lineageStore.PutModelEvidence(ctx, ModelExchange{
		ID:             "model-evidence-" + contracts.EvidenceID(scope),
		OrganizationID: organizationID, WakeID: packet.Lease.WakeID,
		Model: packet.Lease.Model, MGS: packet.Lease.MGS,
		Runtime:        packet.Lease.Runtime,
		Request:        []byte(`{"messages":[{"role":"user","content":"inspect"}],"temperature":0}`),
		Response:       []byte(`{"result":"bounded observation"}`),
		Output:         []byte(`{"disposition":"progressed"}`),
		ReplayRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedEvidence, err := lineageStore.PutModelEvidence(ctx, ModelExchange{
		ID: modelEvidence.ID, OrganizationID: organizationID,
		WakeID: packet.Lease.WakeID, Model: packet.Lease.Model,
		MGS: packet.Lease.MGS, Runtime: packet.Lease.Runtime,
		Request:        []byte(`{"temperature":0,"messages":[{"content":"inspect","role":"user"}]}`),
		Response:       []byte(`{"result":"bounded observation"}`),
		Output:         []byte(`{"disposition":"progressed"}`),
		ReplayRequired: true,
	})
	if err != nil || replayedEvidence.RequestHash != modelEvidence.RequestHash {
		t.Fatalf("canonical model evidence = %#v, %v", replayedEvidence, err)
	}
	openedEvidence, requestBytes, responseBytes, err := lineageStore.OpenModelEvidence(
		ctx, organizationID, modelEvidence.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if openedEvidence != modelEvidence || len(requestBytes) == 0 ||
		!bytes.Equal(responseBytes, []byte(`{"result":"bounded observation"}`)) {
		t.Fatal("model byte evidence did not round trip")
	}
	outputEvidence, modelOutput, err := lineageStore.OpenModelOutput(
		ctx, organizationID, modelEvidence.ID,
	)
	if err != nil || outputEvidence != modelEvidence ||
		!bytes.Equal(modelOutput, []byte(`{"disposition":"progressed"}`)) {
		t.Fatalf(
			"model output lineage = %#v, %q, %v",
			outputEvidence, modelOutput, err,
		)
	}
	receipt, err := lineageStore.BuildReceipt(ReceiptInput{
		ID:     "receipt-" + contracts.ReceiptID(scope),
		Packet: packet, Plan: plan, ModelEvidence: modelEvidence,
		Constraints: []string{"lease, mandate, policy, and resource bounds"},
		Artifacts:   packet.Artifacts, Evidence: packet.Evidence,
		CostMinor: 17, LatencyMillis: 250,
		Disposition:      contracts.DispositionFailed,
		UnresolvedRisk:   "provider returned a typed terminal failure",
		OperationOutcome: "failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lineageStore.PublishReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	openedReceipt, err := lineageStore.OpenReceipt(ctx, organizationID, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if openedReceipt.ContentHash != receipt.ContentHash ||
		openedReceipt.SeatDID != packet.Seat.DID ||
		openedReceipt.MandateVersion != packet.Mandate.Version ||
		openedReceipt.ModelRequestHash != modelEvidence.RequestHash ||
		openedReceipt.Runtime.OperationRegistryDigest != packet.Lease.Runtime.OperationRegistryDigest ||
		openedReceipt.Source.GraphGeneration != source.GraphGeneration ||
		openedReceipt.Disposition != contracts.DispositionFailed {
		t.Fatalf("receipt omitted required lineage: %#v", openedReceipt)
	}

	contractV2 := lineageSkill(t, 2, "lease active")
	if err := skillStore.Publish(ctx, signedSkill(
		t, contractV2, organizationID, keyID, privateKey,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := skillStore.LoadAccepted(ctx, packet.Skills[0]); err != nil {
		t.Fatalf("nonmaterial version forced reauthorization: %v", err)
	}
	contractV3 := lineageSkill(t, 3, "lease and human approval active")
	if err := skillStore.Publish(ctx, signedSkill(
		t, contractV3, organizationID, keyID, privateKey,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := skillStore.LoadAccepted(ctx, packet.Skills[0]); !errors.Is(err, skills.ErrReauthorizationRequired) {
		t.Fatalf("material change error = %v", err)
	}

	for _, query := range []string{
		`SELECT sealed_contract FROM workforce_skill_versions WHERE tenant_id=$1 LIMIT 1`,
		`SELECT sealed_plan FROM workforce_compiled_plans WHERE tenant_id=$1 LIMIT 1`,
		`SELECT sealed_envelope FROM workforce_model_evidence WHERE tenant_id=$1 LIMIT 1`,
		`SELECT sealed_receipt FROM workforce_execution_receipts WHERE tenant_id=$1 LIMIT 1`,
	} {
		var sealed []byte
		if err := lineagePool.QueryRow(ctx, query, tenant).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if !vault.IsVault(sealed) || bytes.Contains(sealed, []byte("bounded observation")) {
			t.Fatal("lineage record was not Vault-sealed")
		}
	}
}

func lineageSkill(t *testing.T, version uint64, precondition string) skills.Contract {
	t.Helper()
	value := skills.Contract{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "inspect", Version: version,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"read"}, DataScopes: []string{"project"},
		Preconditions: []string{precondition},
		Operations: []skills.Operation{{
			Name: "inspect", EffectClass: skills.EffectRead,
			InputSchema:  json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability:   "read", DataScopes: []string{"project"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"internal"},
		}},
		Postconditions: []string{"authoritative observation produced"},
		VerifierDigest: lineageHash("verifier"),
		Retry:          skills.RetryPolicy{MaxAttempts: 2, RetryOn: []string{"transient"}},
		Idempotency: skills.IdempotencyStrategy{
			Scope: "intent", KeyFields: []string{"intent_id"},
		},
		ScheduleEligibility: skills.ScheduleEligibility{WakeReasons: []string{"eligible_work"}},
		Resources: skills.ResourceEstimate{
			MaxDuration: time.Minute, ModelCalls: 1,
			EffectCalls: 1, CostMicros: 1000, MemoryBytes: 1 << 20,
		},
	}
	var err error
	value.Digest, err = value.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func signedSkill(
	t *testing.T,
	contract skills.Contract,
	organizationID contracts.OrganizationID,
	keyID string,
	privateKey ed25519.PrivateKey,
) skills.SignedContract {
	t.Helper()
	value := skills.SignedContract{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID, Contract: contract,
		EffectiveAt: lineageNow(),
	}
	if err := skills.SignContract(&value, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	return value
}

func lineagePacket(
	t *testing.T,
	scope string,
	organizationID contracts.OrganizationID,
	skill skills.Contract,
) contracts.WorkPacket {
	t.Helper()
	now := lineageNow()
	signature := contracts.Signature{
		Algorithm: "ed25519", KeyID: "owner",
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "developer-lead-" + contracts.SeatID(scope), Version: 1,
		DID:            "did:matrix:developer:" + contracts.SeatDID(scope),
		OrganizationID: organizationID, DepartmentID: "department-developer",
		Role: contracts.SeatLead, MandateID: "mandate-" + contracts.MandateID(scope),
		MandateVersion: 1, BindingID: "binding-" + contracts.SeatBindingID(scope),
		BindingVersion: 1, EffectiveAt: now.Add(-time.Hour), Signature: signature,
	}
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            seat.MandateID, Version: 1, OrganizationID: organizationID,
		DepartmentKind: contracts.DepartmentDeveloper, SeatRole: contracts.SeatLead,
		AllowedSkills: []contracts.SkillID{skill.ID},
		DataScopes: []contracts.DataScope{{
			Name: "project", Classification: contracts.ClassificationProject,
			Purpose: "Inspect current project state",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "authority missing", Action: "escalate",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID: "no-prod", Description: "No production mutation",
		}},
		EffectiveAt: now.Add(-time.Hour), Signature: signature,
	}
	policyRef := contracts.PolicyRef{
		ID: "policy-" + contracts.PolicyID(scope), Version: 1,
		Hash: lineageHash("policy"),
	}
	intentID := contracts.IntentID("intent-" + scope)
	lease := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "lease-" + contracts.LeaseID(scope), WakeID: "wake-" + contracts.WakeID(scope),
		OrganizationID: organizationID, SeatID: seat.ID, SeatDID: seat.DID,
		Reason: "eligible_work", MandateID: mandate.ID, MandateVersion: 1,
		Policies:   []contracts.PolicyRef{policyRef},
		GraphScope: []contracts.IntentID{intentID},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "model-" + contracts.ModelBindingID(scope),
			Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: lineageHash("sampling"),
		},
		MGS: contracts.MGSGenomeRef{Reference: "mgs-" + scope, Digest: lineageHash("mgs")},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             lineageHash("build"),
			AuditorBuildDigest:      lineageHash("auditor-build"),
			OperationRegistryDigest: lineageHash("registry"),
		},
		SkillCatalogDigest: lineageHash("catalog"),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
			MaxSteps:          20, MaxModelCalls: 10, MaxToolCalls: 20,
			MaxCostMinor: 100, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: now, ExpiresAt: now.Add(time.Hour), Fence: 1, Signature: signature,
	}
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease:         lease, Seat: seat, Mandate: mandate,
		Goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1, ID: "goal-" + contracts.GoalID(scope),
			OrganizationID: organizationID, WorkOrderID: "order-" + contracts.WorkOrderID(scope),
			Title: "Compile a bounded operation", SuccessCriteria: []string{"Receipt validates"},
			CreatedAt: now.Add(-time.Hour),
		},
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1, ID: intentID,
			OrganizationID: organizationID, GoalID: "goal-" + contracts.GoalID(scope),
			OwnerSeatID: seat.ID, Summary: "Compile and record current work",
			Priority: 10, CreatedAt: now.Add(-time.Hour),
		},
		Skills: []contracts.SkillRef{{
			ID: skill.ID, Version: skill.Version, Digest: skill.Digest,
		}},
		Policies: []contracts.PolicyRef{policyRef},
		RequiredOutputs: []contracts.RequiredOutput{{
			Kind: "receipt", SuccessPredicate: "signed receipt opens",
		}},
		AssembledAt: now,
	}
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	return packet
}

func lineageHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func lineageScope(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func lineageNow() time.Time {
	return time.Date(2026, time.July, 30, 21, 0, 0, 0, time.UTC)
}

func startLineagePostgres(ctx context.Context) (string, string, error) {
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		lineagePostgresImage,
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

func waitLineagePostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
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
