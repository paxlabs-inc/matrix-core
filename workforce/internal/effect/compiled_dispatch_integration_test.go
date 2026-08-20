package effect_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/skills"
	"centra/workforce/internal/workcompile"
)

func TestIntegration_RealCompilerBindsIrreversibleDispatchToSignedApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	label := "compiled-irreversible"
	tenant := gatewayTenant(label)
	now := baseTime()
	leaseStore, err := lease.New(integrationPool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := leaseRequest(label, now)
	request, policyFixture := prepareGatewayPolicyAuthority(t, tenant, request, label)
	grant, err := leaseStore.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	adapter := testAdapter(t, label)
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "owner-key:" + label
	skillStore, err := skills.NewStore(
		integrationPool, testVault(t, tenant), tenant, request.OrganizationID,
		keyID, ownerPublic, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := compiledDispatchSkill(t, label)
	published := skills.SignedContract{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: request.OrganizationID, Contract: contract,
		EffectiveAt: now.Add(-time.Minute),
	}
	if err := skills.SignContract(&published, keyID, ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := skillStore.Publish(ctx, published); err != nil {
		t.Fatal(err)
	}
	skillRef := contracts.SkillRef{
		ID: contract.ID, Version: contract.Version, Digest: contract.Digest,
	}
	catalog, err := skills.NewCatalog([]skills.Contract{contract})
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := approval.New(
		integrationPool, testVault(t, tenant), tenant, request.OrganizationID,
		contracts.OwnerID("owner:"+label), keyID, ownerPublic, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	intentID := contracts.IntentID(request.NodeID)
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       contracts.ApprovalID("approval:" + label),
		TenantID:      tenant, OrganizationID: request.OrganizationID,
		IntentIDs:                  []contracts.IntentID{intentID},
		AggregateCeilingMicrounits: 100,
		ExpiresAt:                  now.Add(time.Hour),
		OwnerID:                    contracts.OwnerID("owner:" + label),
	}
	if err := approval.SignBatch(&batch, keyID, ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := approvals.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	packet := compiledDispatchPacket(t, label, request, grant.Fence, catalog.Digest(), skillRef)
	packet.Seat = policyFixture.seat
	packet.Mandate = policyFixture.mandate
	if err := policy.SignWakeLease(
		&packet.Lease, policyFixture.keyID, policyFixture.private,
	); err != nil {
		t.Fatal(err)
	}
	if err := policyFixture.store.RegisterLease(ctx, packet.Lease); err != nil {
		t.Fatal(err)
	}
	// The gateway can only spend against approvals it can attribute to this
	// exact owner and a current registered signed WakeLease.
	gateway, err := effect.New(
		integrationPool, testVault(t, tenant), leaseStore, policyFixture.store,
		testCircuit(t, tenant, func() time.Time { return now }), tenant,
		approvals.Authority(), func() time.Time { return now }, adapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := workcompile.New(
		integrationPool, testVault(t, tenant), tenant, skillStore,
		policyFixture.store, leaseStore, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(ctx, packet, workcompile.Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "proposal:" + label,
		OrganizationID: request.OrganizationID,
		WakeID:         request.WakeID, IntentID: intentID, SeatID: request.SeatID,
		Skill: skillRef, Operation: "write", Provider: adapter.Name(),
		IdempotencyKey: "effect:" + label,
		ApprovalID:     batch.BatchID, ApprovalCost: 75,
		Input:    json.RawMessage(`{"value":"compiled"}`),
		Deadline: now.Add(30 * time.Minute),
	}, contracts.SourceState{
		RootDigest: contract.Digest, GraphGeneration: 1, LedgerCursor: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Operation.Irreversible || plan.Operation.ApprovalID != batch.BatchID ||
		plan.Operation.ApprovalCost != 75 {
		t.Fatalf("compiled irreversible operation = %#v", plan.Operation)
	}
	var compiledHash string
	if err := integrationPool.QueryRow(ctx, `
		SELECT effect_proposal_hash FROM workforce_compiled_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, request.OrganizationID, plan.ProposalID).Scan(&compiledHash); err != nil {
		t.Fatal(err)
	}
	if compiledHash != effect.ProposalHash(plan.Operation) {
		t.Fatal("compiler did not persist the exact dispatch authority hash")
	}
	result, err := gateway.Execute(ctx, plan.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != effect.StateSucceeded || result.EvidenceHash.Digest == "" {
		t.Fatalf("compiled irreversible dispatch = %#v", result)
	}
	var consumed uint64
	if err := integrationPool.QueryRow(ctx, `
		SELECT consumed_microunits FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, tenant, request.OrganizationID, batch.BatchID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 75 {
		t.Fatalf("consumed approval = %d, want 75", consumed)
	}
	duplicate, err := gateway.Execute(ctx, plan.Operation)
	if err != nil || !duplicate.Deduplicated || duplicate.EvidenceHash != result.EvidenceHash {
		t.Fatalf("idempotent compiled dispatch = %#v, %v", duplicate, err)
	}
	tampered := plan.Operation
	tampered.Input = json.RawMessage(`{"value":"tampered"}`)
	if _, err := gateway.Execute(ctx, tampered); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("post-compile tampered input = %v", err)
	}
	widened := plan.Operation
	widened.ApprovalCost = 100
	if _, err := gateway.Execute(ctx, widened); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("post-compile widened approval cost = %v", err)
	}
	if err := integrationPool.QueryRow(ctx, `
		SELECT consumed_microunits FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, tenant, request.OrganizationID, batch.BatchID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 75 {
		t.Fatalf("rejected replays consumed approval = %d, want 75", consumed)
	}
	replacedSeat := policyFixture.seat
	replacedSeat.Version++
	replacedSeat.BindingVersion++
	replacedSeat.EffectiveAt = now
	if err := policy.SignSeat(
		&replacedSeat, policyFixture.keyID, policyFixture.private,
	); err != nil {
		t.Fatal(err)
	}
	if err := policyFixture.store.PublishSeat(
		ctx, replacedSeat, policyFixture.grant,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Execute(ctx, plan.Operation); err == nil {
		t.Fatalf("dispatch under a replaced current seat = %v", err)
	}
}

func contentHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func compiledDispatchSkill(t *testing.T, label string) skills.Contract {
	t.Helper()
	value := skills.Contract{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.SkillID("skill:" + label), Version: 1,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"write"}, DataScopes: []string{"organization"},
		Preconditions: []string{"lease active"},
		Operations: []skills.Operation{{
			Name: "write", EffectClass: skills.EffectIrreversible,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability:   "write", DataScopes: []string{"organization"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"filesystem"}, CostMicrounits: 75,
		}},
		Postconditions: []string{"typed evidence produced"},
		VerifierDigest: contentHash("verifier:" + label),
		Retry:          skills.RetryPolicy{MaxAttempts: 1},
		Idempotency: skills.IdempotencyStrategy{
			Scope: "intent", KeyFields: []string{"intent_id"},
		},
		ScheduleEligibility: skills.ScheduleEligibility{WakeReasons: []string{"eligible_work"}},
		Resources: skills.ResourceEstimate{
			MaxDuration: time.Minute, ModelCalls: 1, EffectCalls: 1, MemoryBytes: 1 << 20,
		},
	}
	var err error
	value.Digest, err = value.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func compiledDispatchPacket(
	t *testing.T,
	label string,
	request lease.Request,
	fence contracts.FenceToken,
	catalogDigest contracts.ContentHash,
	skillRef contracts.SkillRef,
) contracts.WorkPacket {
	t.Helper()
	now := baseTime()
	organizationID := request.OrganizationID
	intentID := contracts.IntentID(request.NodeID)
	goalID := contracts.GoalID("goal:" + label)
	seatDID := contracts.SeatDID("did:matrix:effect:" + label)
	placeholder := contracts.Signature{
		Algorithm: "ed25519", KeyID: "kernel:" + label,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease: contracts.WakeLease{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            request.ID, WakeID: request.WakeID,
			OrganizationID: organizationID, SeatID: request.SeatID, SeatDID: seatDID,
			Reason:    "eligible_work",
			MandateID: request.MandateID, MandateVersion: request.MandateVersion,
			Policies:   append([]contracts.PolicyRef(nil), request.Policies...),
			GraphScope: []contracts.IntentID{intentID},
			Model: contracts.ModelBinding{
				SchemaVersion: contracts.SchemaVersionV1,
				ID:            contracts.ModelBindingID("model:" + label),
				Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
				SamplingDigest: contentHash("sampling:" + label),
			},
			MGS: contracts.MGSGenomeRef{
				Reference: "mgs:" + label, Digest: contentHash("mgs:" + label),
			},
			Runtime: contracts.RuntimeBinding{
				BuildDigest:             contentHash("runtime:" + label),
				AuditorBuildDigest:      contentHash("auditor-runtime:" + label),
				OperationRegistryDigest: contentHash("registry:" + label),
			},
			SkillCatalogDigest: catalogDigest,
			Budget: contracts.WakeBudget{
				MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
				MaxSteps:          32, MaxModelCalls: 8, MaxToolCalls: 32,
				MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
			},
			IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt,
			Fence: fence, Signature: placeholder,
		},
		Seat: contracts.Seat{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            request.SeatID, Version: 1, DID: seatDID,
			OrganizationID: organizationID, DepartmentID: "department:" + contracts.DepartmentID(label),
			Role:      contracts.SeatExecutor,
			MandateID: request.MandateID, MandateVersion: request.MandateVersion,
			BindingID: contracts.SeatBindingID("binding:" + label), BindingVersion: 1,
			EffectiveAt: request.IssuedAt.Add(-time.Hour),
			Signature:   placeholder,
		},
		Mandate: contracts.Mandate{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            request.MandateID, Version: request.MandateVersion,
			OrganizationID: organizationID,
			DepartmentKind: contracts.DepartmentDeveloper,
			SeatRole:       contracts.SeatExecutor,
			AllowedSkills:  []contracts.SkillID{skillRef.ID},
			DataScopes: []contracts.DataScope{{
				Name: "source", Classification: contracts.ClassificationProject,
				Purpose: "Dispatch only the compiled fenced operation",
			}},
			EscalationRules: []contracts.EscalationRule{{
				Condition: "Current durable evidence is insufficient",
				Action:    "Escalate to the human owner",
			}},
			Prohibitions: []contracts.Prohibition{{
				ClauseID:    "no-ambient-authority",
				Description: "No effect credentials or prior-session memory",
			}},
			EffectiveAt: request.IssuedAt.Add(-time.Hour),
			Signature:   placeholder,
		},
		Goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1, ID: goalID,
			OrganizationID: organizationID, WorkOrderID: contracts.WorkOrderID("order:" + label),
			Title:           "Complete the compiled irreversible dispatch",
			SuccessCriteria: []string{"Approved external effect is receipted"},
			CreatedAt:       request.IssuedAt.Add(-time.Hour),
		},
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1, ID: intentID,
			OrganizationID: organizationID, GoalID: goalID, OwnerSeatID: request.SeatID,
			Summary: "Execute only the compiled approved operation", Priority: 10,
			CreatedAt: request.IssuedAt.Add(-time.Hour),
		},
		Tools: []contracts.ToolRef{{
			Name: "inspect", SchemaDigest: contentHash("tool:" + label),
		}},
		Skills: []contracts.SkillRef{skillRef},
		Policies: append(
			[]contracts.PolicyRef(nil), request.Policies...,
		),
		RequiredOutputs: []contracts.RequiredOutput{{
			Kind:             "typed_result",
			SuccessPredicate: "Provider evidence validates",
		}},
		AssembledAt: now,
	}
	if err := packet.Validate(); err != nil {
		t.Fatalf("compiled dispatch WorkPacket: %v", err)
	}
	return packet
}
